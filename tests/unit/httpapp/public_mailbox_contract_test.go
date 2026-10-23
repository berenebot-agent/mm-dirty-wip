package httpapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestPublicMailboxActionFailureEnvelope(t *testing.T) {
	t.Parallel()
	svc := newIsolatedService(t)
	u := createAdmin(t, svc)
	ctx := context.Background()
	domain, err := svc.Store.CreateDomain(ctx, u.AccountID, "errors.example.test")
	if err != nil {
		t.Fatal(err)
	}
	box, err := svc.Store.CreateInbox(ctx, u.AccountID, domain.ID, "agent", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	action, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SettleMessageAction(ctx, action.ID, "ambiguous", "verify the provider outcome", time.Time{}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/inboxes/"+box.ID+"/trash/empty", nil)
	r.Header.Set("Authorization", "Bearer "+adminKey(t, svc, u))
	rr := httptest.NewRecorder()
	httpapp.New(svc, nil).Handler().ServeHTTP(rr, r)
	var out struct {
		Code   string              `json:"code"`
		Error  string              `json:"error"`
		Action store.MessageAction `json:"action"`
	}
	if rr.Code != 409 || json.Unmarshal(rr.Body.Bytes(), &out) != nil || out.Code != "conflict" || out.Error == "" || out.Action.ID != action.ID {
		t.Fatalf("action failure: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPublicMailboxAttachmentDiscovery(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, remote, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	raw := "From: sender@example.test\r\nSubject: Files\r\nMessage-ID: <files@example.test>\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=report.bin\r\n\r\nPAYLOAD\r\n--b--\r\n"
	fake.add("INBOX", raw, "<files@example.test>", "Files")
	if err := os.MkdirAll(filepath.Join(svc.Config.DataDir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.Config.DataDir, "messages/files.eml"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	local, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: domain, Provider: "mailgun", ProviderDeliveryID: "files", RFCMessageID: "<files@example.test>",
		From: model.Address{Address: "sender@example.test"}, To: []string{domain.Address}, Subject: "Files",
		RawPath: "messages/files.eml", SizeBytes: int64(len(raw)), ReceivedAt: time.Now().UTC(),
		Attachments: []store.AttachmentInput{{Filename: "report.bin", ContentType: "application/octet-stream", PartIndex: 1, Size: 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := apiGet(t, h, "/v1/inboxes/"+remote.ID+"/messages", key)
	var listing struct{ Items []model.Message }
	if err := json.Unmarshal(page.Body.Bytes(), &listing); err != nil || len(listing.Items) != 1 {
		t.Fatalf("remote list: %v %s", err, page.Body.String())
	}
	for _, id := range []string{local.ID, listing.Items[0].ID} {
		rr := apiGet(t, h, "/v1/messages/"+id+"/attachments", key)
		var items []struct {
			Filename string `json:"filename"`
			URL      string `json:"download_url"`
			Size     int64  `json:"size"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &items) != nil || len(items) != 1 {
			t.Fatalf("attachments: %d %s", rr.Code, rr.Body.String())
		}
		if items[0].Filename != "report.bin" || items[0].Size != 7 || !strings.HasPrefix(items[0].URL, "/v1/") {
			t.Fatalf("descriptor: %+v", items[0])
		}
		download := apiGet(t, h, items[0].URL, key)
		if download.Code != 200 || !strings.Contains(download.Body.String(), "PAYLOAD") || download.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("download: %d %s", download.Code, download.Body.String())
		}
		if rr := apiGet(t, h, items[0].URL, "invalid-key"); rr.Code != 401 {
			t.Fatalf("unauthenticated download: %d", rr.Code)
		}
	}
	files, err := filepath.Glob(filepath.Join(svc.Config.DataDir, "remote", ".tmp", "*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("remote MIME retained: %v %v", files, err)
	}
}

func TestPublicMailboxArrivalMutationAndScope(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, box, fake := standaloneFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	fake.add("INBOX", "From: sender@example.test\r\nMessage-ID: <arrival@test>\r\nSubject: Arrival\r\n\r\nbody", "<arrival@test>", "Arrival")
	if rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	arrival, _, err := svc.Store.RecordRemoteArrival(ctx, u.AccountID, box.ID, store.RemoteArrivalInput{FolderPath: "INBOX", UIDValidity: 100, UID: 1, RFCMessageID: "<arrival@test>", Subject: "Arrival"})
	if err != nil {
		t.Fatal(err)
	}
	_, restricted, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "other inbox", false, map[string]string{domain.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/v1/messages/"+arrival.ID, strings.NewReader(`{"read":true}`))
		r.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	if rr := patch(restricted); rr.Code != 404 {
		t.Fatalf("cross-inbox mutation: %d %s", rr.Code, rr.Body.String())
	}
	rr := patch(key)
	var message model.Message
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &message) != nil || message.ID == "" || message.ID == arrival.ID {
		t.Fatalf("arrival mutation: %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/v1/inboxes/" + box.ID + "/messages/" + arrival.ID, "/v1/messages/" + arrival.ID + "/content"} {
		if rr := apiGet(t, h, path, key); rr.Code != 200 {
			t.Fatalf("alias %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	actions, err := svc.Store.ListMessageActions(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if err != nil || len(actions) != 1 || actions[0].MessageID != message.ID || actions[0].State != "done" || actions[0].Input.Read == nil || !*actions[0].Input.Read {
		t.Fatalf("action canonical identity: %+v %v", actions, err)
	}
}

func TestPublicMailboxThreadPagination(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"domain", "imap"} {
		t.Run(kind, func(t *testing.T) {
			svc, h, u, domain, remote, fake := standaloneFixture(t)
			key := adminKey(t, svc, u)
			box := domain
			if kind == "imap" {
				box = remote
			}
			const count = 205
			base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < count; i++ {
				rfc := fmt.Sprintf("<thread-%d@test>", i)
				if i == 0 {
					rfc = "<root@test>"
				}
				if kind == "imap" {
					folder := "INBOX"
					if i%2 == 1 {
						folder = "Sent"
					}
					fake.addAt(folder, "From: sender@example.test\r\nSubject: Thread\r\nMessage-ID: "+rfc+"\r\nReferences: <root@test>\r\n\r\nbody", rfc, "Thread", base)
				} else {
					_, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: rfc, RFCMessageID: rfc, References: []string{"<root@test>"}, From: model.Address{Address: "sender@example.test"}, Subject: "Thread", ReceivedAt: base})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "imap" {
				for _, folder := range []string{"Trash", "Spam"} {
					fake.folders[folder] = 100
					rfc := "<hidden-" + folder + "@test>"
					fake.addAt(folder, "From: sender@example.test\r\nSubject: Thread\r\nMessage-ID: "+rfc+"\r\nReferences: <root@test>\r\n\r\nbody", rfc, "Thread", base)
				}
			}
			rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?limit=1", key)
			var first struct{ Items []model.Message }
			if json.Unmarshal(rr.Body.Bytes(), &first) != nil || len(first.Items) != 1 {
				t.Fatal(rr.Body.String())
			}
			thread := first.Items[0].ThreadID
			rr = apiGet(t, h, "/v1/inboxes/"+box.ID+"/threads/"+thread, key)
			var detail struct {
				Messages     []model.Message `json:"messages"`
				Completeness string          `json:"completeness"`
				URL          string          `json:"messages_url"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &detail) != nil || len(detail.Messages) != 200 || detail.Completeness != "partial" {
				t.Fatalf("thread preview: status=%d messages=%d completeness=%s", rr.Code, len(detail.Messages), detail.Completeness)
			}
			seen := map[string]bool{}
			cursor := ""
			for page := 0; page < 20; page++ {
				rr = apiGet(t, h, detail.URL+"&limit=37&before="+url.QueryEscape(cursor), key)
				var out struct {
					Items []model.Message `json:"items"`
					Next  string          `json:"next_cursor"`
				}
				if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &out) != nil {
					t.Fatalf("page: %d %s", rr.Code, rr.Body.String())
				}
				for _, m := range out.Items {
					if m.ThreadID != thread || seen[m.ID] {
						t.Fatalf("duplicate or wrong thread: %+v", m)
					}
					seen[m.ID] = true
				}
				cursor = out.Next
				if cursor == "" {
					break
				}
			}
			if len(seen) != count || cursor != "" {
				t.Fatalf("retrieved %d of %d; cursor %q", len(seen), count, cursor)
			}
		})
	}
}
