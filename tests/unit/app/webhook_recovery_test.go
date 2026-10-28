package app_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
)

type webhookRemoteFetch struct {
	path string
	err  error
}

func (f *webhookRemoteFetch) FetchArrivalRaw(context.Context, model.Inbox, store.RemoteArrival) (string, int64, error) {
	return f.path, 4, f.err
}

func (*webhookRemoteFetch) CleanupRemoteRaw(string) {}

func TestWebhookPreparationFailuresDoNotBlockClients(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"file missing", "secret unreadable", "remote transient", "remote ambiguous", "remote cache miss", "remote deleted"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, u, dom, box := testService(t)
			svc.Config.WebhookRetryWindow = time.Hour
			ctx := context.Background()
			remote := name == "remote transient" || name == "remote ambiguous" || name == "remote cache miss" || name == "remote deleted"
			var eventID int64
			var path string
			var raw []byte
			fetch := &webhookRemoteFetch{}
			if remote {
				box = standaloneInbox(t, svc, u.AccountID, "webhook@remote.example")
				a, _, err := svc.Store.RecordRemoteArrival(ctx, u.AccountID, box.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 7, UID: 42, RFCMessageID: "<remote-webhook@test>"})
				if err != nil {
					t.Fatal(err)
				}
				ev, _, err := svc.Store.RecordRemoteArrivalEvent(ctx, u.AccountID, box.ID, a)
				if err != nil {
					t.Fatal(err)
				}
				eventID = ev.ID
				path = filepath.Join(svc.Config.DataDir, "remote-test.eml")
				if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
					t.Fatal(err)
				}
				fetch.path = path
				fetch.err = errors.New("temporary provider failure")
				switch name {
				case "remote ambiguous":
					fetch.err = imap.Ambiguous("fetch outcome unknown", nil)
				case "remote cache miss":
					fetch.err = store.ErrNotFound
				case "remote deleted":
					fetch.err = imap.NotFound("message was deleted")
				}
				svc.RemoteForwarder = fetch
			} else {
				mid := seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)
				m, err := svc.Store.GetMessageByID(ctx, u.AccountID, mid)
				if err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(svc.Config.DataDir, m.RawPath)
				raw, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if name == "file missing" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				eventID = receivedEventID(t, svc, u.AccountID, box.ID)
			}
			enc, err := svc.EncryptSecretAAD(app.WebhookSecretAAD(u.AccountID, box.ID), []byte("test-secret"))
			if err != nil {
				t.Fatal(err)
			}
			badSecret := enc
			if name == "secret unreadable" {
				badSecret = "unreadable"
			}
			var badCalls, goodCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/bad" {
					badCalls.Add(1)
				} else {
					goodCalls.Add(1)
				}
				w.WriteHeader(204)
			}))
			defer srv.Close()
			bad, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "bad", srv.URL+"/bad", "forward", "signature", badSecret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "good", srv.URL+"/good", "notify", "signature", enc); err != nil {
				t.Fatal(err)
			}
			worker := app.NewWebhookWorker(svc)
			worker.SetHTTPClient(srv.Client())
			for range 4 {
				if err := worker.RunOnce(ctx); errors.Is(err, store.ErrNotFound) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if badCalls.Load() != 0 || goodCalls.Load() != 1 {
				t.Fatalf("preparation failure blocked other client: bad=%d good=%d", badCalls.Load(), goodCalls.Load())
			}
			log, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, bad.ID, 10, 1<<62)
			if err != nil || len(log) != 1 {
				t.Fatalf("missing outcome: %+v %v", log, err)
			}
			if name == "remote deleted" {
				if log[0].Status != "skipped" {
					t.Fatalf("confirmed deletion not skipped: %+v", log[0])
				}
				return
			}
			if log[0].Status != "pending" || log[0].Attempts < 1 || log[0].NextAttemptAt == "" {
				t.Fatalf("ambiguous/transient failure discarded: %+v", log[0])
			}
			fetch.err = nil
			if !remote {
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.Store.RotateWebhookSecret(ctx, u.AccountID, bad.ID, enc); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", svc.Store.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE webhook_deliveries SET next_attempt_at=? WHERE client_id=? AND event_id=?`, "2000-01-01T00:00:00Z", bad.ID, eventID); err != nil {
				t.Fatal(err)
			}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			log, err = svc.Store.ClientDeliveryLog(ctx, u.AccountID, bad.ID, 10, 1<<62)
			if err != nil || len(log) != 1 || log[0].Status != "delivered" || badCalls.Load() != 1 || goodCalls.Load() != 1 {
				t.Fatalf("recovery failed or duplicated delivery: %+v %v bad=%d good=%d", log, err, badCalls.Load(), goodCalls.Load())
			}
		})
	}
}

func TestWebhookRetryWindowAnchoredToFirstAttempt(t *testing.T) {
	t.Parallel()
	svc, u, dom, box := testService(t)
	svc.Config.WebhookRetryWindow = time.Hour
	ctx := context.Background()
	seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)
	enc, err := svc.EncryptSecretAAD(app.WebhookSecretAAD(u.AccountID, box.ID), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	client, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "retry", srv.URL, "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}
	worker := app.NewWebhookWorker(svc)
	worker.SetHTTPClient(srv.Client())
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", svc.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, age := range []time.Duration{30 * time.Minute, 2 * time.Hour} {
		first := time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
		if _, err := db.Exec(`UPDATE webhook_deliveries SET created_at=?,next_attempt_at=? WHERE client_id=?`, first, "2000-01-01T00:00:00Z", client.ID); err != nil {
			t.Fatal(err)
		}
		if err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		log, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, client.ID, 10, 1<<62)
		want := "pending"
		if age > time.Hour {
			want = "failed"
		}
		if err != nil || len(log) != 1 || log[0].Status != want {
			t.Fatalf("age=%v: log=%+v err=%v", age, log, err)
		}
		var storedFirst string
		if err := db.QueryRow(`SELECT created_at FROM webhook_deliveries WHERE client_id=?`, client.ID).Scan(&storedFirst); err != nil || storedFirst != first {
			t.Fatalf("retry moved first attempt: %q want=%q err=%v", storedFirst, first, err)
		}
	}
	healthy.Store(true)
	raw := "From: sender@outside.test\r\nTo: " + box.Address + "\r\nSubject: recovered\r\nMessage-ID: <recovered@test>\r\n\r\nbody"
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "hook-2", box.Address, raw)); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	log, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, client.ID, 10, 1<<62)
	if err != nil || len(log) != 2 || log[0].Status != "delivered" || log[1].Status != "failed" {
		t.Fatalf("expired event blocked later delivery: %+v %v", log, err)
	}
}
