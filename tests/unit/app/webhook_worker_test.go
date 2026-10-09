package app_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/store"
)

// parseWebhookSignature splits a "t=<unix>,v1=<hex>" header.
func parseWebhookSignature(t *testing.T, header string) (ts, sig string) {
	t.Helper()
	for _, part := range strings.Split(header, ",") {
		switch {
		case strings.HasPrefix(part, "t="):
			ts = strings.TrimPrefix(part, "t=")
		case strings.HasPrefix(part, "v1="):
			sig = strings.TrimPrefix(part, "v1=")
		}
	}
	if ts == "" || sig == "" {
		t.Fatalf("malformed signature header %q", header)
	}
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Fatalf("bad timestamp %q", ts)
	}
	return ts, sig
}

func seedWebhookMessage(t *testing.T, svc *app.Service, accountID, domainID, boxAddress string) string {
	t.Helper()
	ctx := context.Background()
	seedInbound(t, svc, accountID, domainID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	raw := "From: Sender <sender@outside.test>\r\nTo: " + boxAddress + "\r\nSubject: hook\r\nMessage-ID: <hook-inbound@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nhook body"
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "hook-1", boxAddress, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	return m.ID
}

func TestWebhookWorkerNotifySignsAndAcks(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	messageID := seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)

	const secret = "webhook-signing-secret"
	enc, err := svc.EncryptSecret([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "notify", "signature", enc); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	var gotSig, gotEvent, gotCursor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-MailMoose-Signature")
		gotEvent = r.Header.Get("X-MailMoose-Event")
		gotCursor = r.Header.Get("X-MailMoose-Cursor")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, err := svc.Store.ListWebhookClients(ctx, u.AccountID)
	if err != nil || len(c) != 1 {
		t.Fatalf("list %v %#v", err, c)
	}
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, c[0].ID, "ep", srv.URL, "notify", "signature"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload: %v (%s)", err, gotBody)
	}
	if payload["message_id"] != messageID || payload["event"] != "message.received" || gotEvent != "message.received" || gotCursor == "" {
		t.Fatalf("payload %v event=%q cursor=%q", payload, gotEvent, gotCursor)
	}

	ts, sig := parseWebhookSignature(t, gotSig)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(gotBody)
	if hex.EncodeToString(mac.Sum(nil)) != sig {
		t.Fatal("signature mismatch")
	}

	// A successful delivery acks the event so it is not sent again.
	if err := w.RunOnce(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second RunOnce err=%v, want not found", err)
	}
}

func TestWebhookWorkerForwardStreamsRawMIME(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)

	const secret = "forward-token"
	enc, _ := svc.EncryptSecret([]byte(secret))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}

	var body []byte
	var contentType, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		contentType = r.Header.Get("Content-Type")
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if !strings.HasPrefix(contentType, "message/rfc822") {
		t.Fatalf("content type %q", contentType)
	}
	if auth != "Bearer "+secret {
		t.Fatalf("authorization %q", auth)
	}
	if !strings.Contains(string(body), "Subject: hook") {
		t.Fatalf("raw MIME body missing subject: %q", body)
	}
}

func TestWebhookWorkerRetriesOnServerError(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)

	enc, _ := svc.EncryptSecret([]byte("s"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "notify", "signature"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce should record the failure, not return it: %v", err)
	}
	got, err := svc.Store.GetWebhookClient(ctx, u.AccountID, cl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError == "" {
		t.Fatal("failure was not recorded")
	}
	// The retry is visible in the client delivery log with its attempt count
	// and last error, so an operator can see the outstanding delivery.
	entries, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, cl.ID, 10, 1<<62)
	if err != nil || len(entries) != 1 {
		t.Fatalf("delivery log %v %#v", err, entries)
	}
	if entries[0].Status == "delivered" || entries[0].Status == "acknowledged" || entries[0].Attempts != 1 || entries[0].LastError == "" {
		t.Fatalf("retry log entry %#v", entries[0])
	}
}

// TestWebhookBackedOffClientDoesNotBlockOthers proves that one webhook whose
// delivery is backed off does not pin the queue head: another webhook on the
// same inbox with a due event is still delivered.
func TestWebhookBackedOffClientDoesNotBlockOthers(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)

	enc, _ := svc.EncryptSecret([]byte("s"))
	failing, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "failing", "https://hooks.example.test/x", "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}
	good, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "good", "https://hooks.example.test/y", "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer badSrv.Close()
	var goodDelivered int32
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&goodDelivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, failing.ID, "failing", badSrv.URL, "notify", "signature"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, good.ID, "good", goodSrv.URL, "notify", "signature"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(&http.Client{Transport: &fixedTransport{bad: badSrv.Client().Transport, good: goodSrv.Client().Transport, badHost: badSrv.URL, goodHost: goodSrv.URL}})

	// Drain until only the backed-off failing client remains.
	for i := 0; i < 8; i++ {
		err := w.RunOnce(ctx)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&goodDelivered) == 0 {
		t.Fatal("a backed-off webhook blocked another webhook's delivery")
	}
	// The good client's event is acknowledged; the failing one stays pending.
	entries, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, good.ID, 10, 1<<62)
	if err != nil || len(entries) == 0 || entries[0].Status != "delivered" {
		t.Fatalf("good client log %v %#v", err, entries)
	}
}

// fixedTransport routes requests to one of two upstreams by URL prefix, so a
// single worker can exercise a failing and a succeeding endpoint.
type fixedTransport struct {
	bad, good         http.RoundTripper
	badHost, goodHost string
}

func (t *fixedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.URL.String(), t.badHost) {
		return t.bad.RoundTrip(r)
	}
	return t.good.RoundTrip(r)
}

// TestWebhookWorkerDeliveryLogRecordsSuccess proves a successful delivery is
// recorded as "delivered" in the client delivery log.
func TestWebhookWorkerDeliveryLogRecordsSuccess(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	messageID := seedWebhookMessage(t, svc, u.AccountID, dom.ID, box.Address)

	enc, _ := svc.EncryptSecret([]byte("s"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "notify", "signature", enc)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "notify", "signature"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	entries, err := svc.Store.ClientDeliveryLog(ctx, u.AccountID, cl.ID, 10, 1<<62)
	if err != nil || len(entries) != 1 {
		t.Fatalf("delivery log %v %#v", err, entries)
	}
	if entries[0].Status != "delivered" || entries[0].MessageID != messageID {
		t.Fatalf("delivered log entry %#v", entries[0])
	}
}
