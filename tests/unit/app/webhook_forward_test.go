package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/cloudflare"
)

// forwardCapture is a test HTTP endpoint that records the exact request the
// forward webhook worker makes.
type forwardCapture struct {
	mu          chan struct{}
	body        []byte
	contentType string
	headers     http.Header
	auth        string
}

func newForwardCapture() *forwardCapture {
	c := &forwardCapture{mu: make(chan struct{}, 1)}
	c.mu <- struct{}{}
	return c
}

func (c *forwardCapture) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		<-c.mu
		c.body = body
		c.contentType = r.Header.Get("Content-Type")
		c.headers = r.Header.Clone()
		c.auth = r.Header.Get("Authorization")
		c.mu <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (c *forwardCapture) snapshot() (body []byte, contentType string, headers http.Header, auth string) {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()
	return c.body, c.contentType, c.headers, c.auth
}

// seedForwardMessage ingests one message with the given transport envelope
// sender and returns its id and the exact raw MIME bytes. The envelope
// recipient is always the inbox address; catch-all/alias routing is covered by
// the store tests.
func seedForwardMessage(t *testing.T, svc *app.Service, accountID, domainID, boxAddress, envelopeFrom, raw string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	seedInbound(t, svc, accountID, domainID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	req := cfRequest(t, testCFSecret, "fwd-"+boxAddress+"-"+time.Now().Format("150405.000000000"), boxAddress, raw)
	if envelopeFrom != "" {
		req.Header.Set(cloudflare.HeaderEnvelopeTo, envelopeFrom)
	} else {
		req.Header.Del(cloudflare.HeaderEnvelopeTo)
	}
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", req)
	if err != nil || dup {
		t.Fatalf("seed forward message %v dup=%v", err, dup)
	}
	return m.ID, []byte(raw)
}

func TestWebhookForwardEnvelopeHeadersAndExactMIME(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	// A sender exercising RFC 3986 escaping: space, plus, @, and a unicode rune.
	const sender = "Ünicode + Tag <user+tag@outside.test>"
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: forward exact\r\nMessage-ID: <fwd-exact@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody with \r\n CRLF preserved"
	messageID, wantBody := seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, sender, raw)

	const token = "forward-bearer-token"
	enc, _ := svc.EncryptSecret([]byte(token))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	cap := newForwardCapture()
	srv := cap.serve()
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	body, contentType, headers, auth := cap.snapshot()
	if contentType != "message/rfc822" {
		t.Fatalf("content type %q", contentType)
	}
	if auth != "Bearer "+token {
		t.Fatalf("authorization %q", auth)
	}
	// The raw MIME is byte-for-byte identical to what was ingested.
	if string(body) != string(wantBody) {
		t.Fatalf("raw MIME changed:\n got %q\nwant %q", body, wantBody)
	}
	// The shared contract headers carry the encoded envelope metadata.
	const wantFrom = "%C3%9Cnicode%20%2B%20Tag%20%3Cuser%2Btag%40outside.test%3E"
	if got := headers.Get(app.HeaderEnvelopeFrom); got != wantFrom {
		t.Fatalf("envelope-from header %q, want %q", got, wantFrom)
	}
	if got := headers.Get(app.HeaderEnvelopeTo); got != box.Address {
		// @ is escaped, so the plain address must appear encoded.
		if got != strings.ReplaceAll(strings.ReplaceAll(box.Address, "@", "%40"), "+", "%2B") {
			t.Fatalf("envelope-to header %q", got)
		}
	}
	// Exactly one of each header, and the existing headers are preserved.
	if n := len(headers.Values(app.HeaderEnvelopeFrom)); n != 1 {
		t.Fatalf("envelope-from header count %d", n)
	}
	if headers.Get("X-MailMoose-Event") != "message.received" || headers.Get("X-MailMoose-Cursor") == "" || headers.Get("X-MailMoose-Delivery") == "" || headers.Get("X-MailMoose-Message-Id") != messageID {
		t.Fatalf("existing headers lost: %#v", headers)
	}
}

func TestWebhookForwardMissingSenderEmitsEmptyHeader(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: no sender\r\nMessage-ID: <fwd-nosender@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "", raw)

	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	cap := newForwardCapture()
	srv := cap.serve()
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_, _, headers, _ := cap.snapshot()
	// The header is present but empty, so a receiver that requires an envelope
	// sender fails closed rather than falling back to the MIME From header.
	if vals := headers.Values(app.HeaderEnvelopeFrom); len(vals) != 1 || vals[0] != "" {
		t.Fatalf("missing sender header %#v", vals)
	}
}

func TestWebhookRetryKeepsEnvelopeMetadata(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.WebhookRetryWindow = time.Hour
	ctx := context.Background()
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: retry\r\nMessage-ID: <fwd-retry@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "retry-sender@outside.test", raw)

	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	var secondFrom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 2 {
			secondFrom = r.Header.Get(app.HeaderEnvelopeFrom)
		}
		if attempts == 1 {
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Force the retry to be due now instead of waiting for the backoff.
	now := time.Now().UTC()
	if err := svc.Store.RecordWebhookDelivery(ctx, cl.ID, receivedEventID(t, svc, u.AccountID, box.ID), false, "boom", now.Add(-time.Second), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
	if secondFrom != "retry-sender%40outside.test" {
		t.Fatalf("retry envelope-from %q", secondFrom)
	}
}

// receivedEventID returns the id of the message.received event for an inbox.
func receivedEventID(t *testing.T, svc *app.Service, accountID, inboxID string) int64 {
	t.Helper()
	evs, err := svc.Store.ListEvents(context.Background(), model.Principal{AccountID: accountID, Admin: true}, 0, inboxID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Type == "message.received" {
			return ev.ID
		}
	}
	t.Fatal("no message.received event")
	return 0
}

func TestWebhookSpamSkipsWithoutNetworkAndReleaseDelivers(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: spam skip\r\nMessage-ID: <fwd-spam@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "spam-sender@outside.test", raw)

	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "fork", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	// Move the message to Spam (as the MX policy or a human would).
	msgs, err := svc.Store.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list %v %#v", err, msgs)
	}
	spamMsg := msgs[0]
	if _, ev, err := svc.Store.SetMessageSpam(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, spamMsg.ID, true); err != nil {
		t.Fatal(err)
	} else if ev == nil {
		t.Fatal("expected a spam state change event")
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	// Drains the received event and the spam transition with no network call.
	if err := w.RunOnce(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spam RunOnce err=%v, want not found", err)
	}
	if calls != 0 {
		t.Fatalf("spam delivered over the network: calls=%d", calls)
	}
	// The cursor advanced past both events.
	got, err := svc.Store.GetWebhookClient(ctx, u.AccountID, cl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAckEventID == 0 {
		t.Fatal("cursor did not advance past skipped spam events")
	}

	// Release from Spam: the release transition and the healthy next message
	// both become deliverable.
	if _, _, err := svc.Store.SetMessageSpam(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, spamMsg.ID, false); err != nil {
		t.Fatal(err)
	}
	healthyRaw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: healthy next\r\nMessage-ID: <fwd-healthy@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "healthy@outside.test", healthyRaw)

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("release RunOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("release/healthy calls=%d, want 1", calls)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("healthy RunOnce: %v", err)
	}
	if calls != 2 {
		t.Fatalf("healthy calls=%d, want 2", calls)
	}
}

func TestWebhookDeletedMessageDoesNotStall(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: deleted\r\nMessage-ID: <fwd-deleted@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	messageID, _ := seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "s@outside.test", raw)

	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Store.DeleteMessage(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, messageID); err != nil {
		t.Fatal(err)
	}
	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted RunOnce err=%v, want not found", err)
	}
	if calls != 0 {
		t.Fatalf("deleted message attempted delivery: calls=%d", calls)
	}
}

// TestWebhookDeliveryIsScopedToOwnInbox proves a delivery worker never crosses
// an account or inbox boundary: each webhook client only receives events from
// its own bound inbox, and a client in another account is untouched.
func TestWebhookDeliveryIsScopedToOwnInbox(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	second, err := svc.Store.CreateInbox(ctx, u.AccountID, dom.ID, "sales", "Sales")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := svc.EncryptSecret([]byte("tok"))

	var inboxOne, inboxTwo []string
	srvOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		inboxOne = append(inboxOne, string(b))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srvOne.Close()
	srvTwo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		inboxTwo = append(inboxTwo, string(b))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srvTwo.Close()

	clOne, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "one", srvOne.URL, "notify", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	clTwo, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, second.ID, "two", srvTwo.URL, "notify", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	// A client in another account must not see this account's events.
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	otherDom, err := svc.Store.CreateDomain(ctx, other.AccountID, "b.test")
	if err != nil {
		t.Fatal(err)
	}
	otherBox, err := svc.Store.CreateInbox(ctx, other.AccountID, otherDom.ID, "shared", "Shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateWebhookClient(ctx, other.AccountID, otherBox.ID, "other", srvTwo.URL, "notify", "bearer", enc); err != nil {
		t.Fatal(err)
	}
	_ = clTwo

	// One message to each of this account's inboxes.
	rawA := "From: a@outside.test\r\nTo: " + box.Address + "\r\nSubject: A\r\nMessage-ID: <iso-a@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nA"
	rawB := "From: b@outside.test\r\nTo: " + second.Address + "\r\nSubject: B\r\nMessage-ID: <iso-b@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nB"
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	for i, tc := range []struct{ rcpt, raw string }{{box.Address, rawA}, {second.Address, rawB}} {
		if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "iso-"+string(rune('a'+i)), tc.rcpt, tc.raw)); err != nil {
			t.Fatal(err)
		}
	}

	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srvOne.Client())
	// Deliver both clients' single events; each client only sees its own inbox.
	for i := 0; i < 2; i++ {
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if err := w.RunOnce(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queue drained: %v", err)
	}
	if len(inboxOne) != 1 || !strings.Contains(inboxOne[0], "\"inbox_id\":\""+box.ID+"\"") {
		t.Fatalf("inbox one bodies %#v", inboxOne)
	}
	if len(inboxTwo) != 1 || !strings.Contains(inboxTwo[0], "\"inbox_id\":\""+second.ID+"\"") {
		t.Fatalf("inbox two bodies %#v", inboxTwo)
	}
	_ = clOne
	_ = clTwo
}

// TestWebhookRetrySkippedWhenMessageBecomesSpam covers the queue race: an event
// that failed and is awaiting a retry is terminally skipped (no network) once
// its message moves to Spam, so the retry does not deliver quarantined mail.
func TestWebhookRetrySkippedWhenMessageBecomesSpam(t *testing.T) {
	svc, u, dom, box := testService(t)
	svc.Config.WebhookRetryWindow = time.Hour
	ctx := context.Background()
	raw := "From: Header <header@outside.test>\r\nTo: " + box.Address + "\r\nSubject: retry spam\r\nMessage-ID: <retry-spam@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, "s@outside.test", raw)

	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "later", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	// First attempt fails and schedules a retry for this event.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("first: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first call count=%d", calls)
	}
	// Move the message to Spam, then make the retry due.
	msgs, err := svc.Store.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list %v %#v", err, msgs)
	}
	if _, _, err := svc.Store.SetMessageSpam(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, msgs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, ev := range receivedEvents(t, svc, u.AccountID, box.ID) {
		if err := svc.Store.RecordWebhookDelivery(ctx, cl.ID, ev, false, "boom", now.Add(-time.Second), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// The retry is skipped without a second network call.
	if err := w.RunOnce(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retry RunOnce err=%v, want not found", err)
	}
	if calls != 1 {
		t.Fatalf("spam retry attempted delivery: calls=%d", calls)
	}
}

// receivedEvents returns the message.received event ids for an inbox.
func receivedEvents(t *testing.T, svc *app.Service, accountID, inboxID string) []int64 {
	t.Helper()
	evs, err := svc.Store.ListEvents(context.Background(), model.Principal{AccountID: accountID, Admin: true}, 0, inboxID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, ev := range evs {
		if ev.Type == "message.received" {
			out = append(out, ev.ID)
		}
	}
	return out
}

// TestWebhookForwardContractFixture exports a synthetic request fixture for the
// joint MailMoose/Billbot wire-contract check when BILLBOT_CONTRACT_FIXTURE is
// set to a file path whose parent directory exists. It is skipped otherwise, so
// ordinary test runs never write files.
func TestWebhookForwardContractFixture(t *testing.T) {
	out := os.Getenv("BILLBOT_CONTRACT_FIXTURE")
	if out == "" {
		t.Skip("BILLBOT_CONTRACT_FIXTURE not set")
	}
	if _, err := os.Stat(filepath.Dir(out)); err != nil {
		t.Fatalf("fixture parent directory must exist: %v", err)
	}

	svc, u, dom, box := testService(t)
	ctx := context.Background()
	const sender = "Invoice + Co <billing+ap@outside.test>"
	raw := "From: Invoice Co <billing+ap@outside.test>\r\nTo: " + box.Address + "\r\nSubject: Invoice INV-1\r\nMessage-ID: <fixture@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nsynthetic fixture body"
	seedForwardMessage(t, svc, u.AccountID, dom.ID, box.Address, sender, raw)

	const token = "fixture-bearer-token"
	enc, _ := svc.EncryptSecret([]byte(token))
	cl, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	cap := newForwardCapture()
	srv := cap.serve()
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, u.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	w := app.NewWebhookWorker(svc)
	w.SetHTTPClient(srv.Client())
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	body, contentType, headers, auth := cap.snapshot()

	fixture := map[string]any{
		"method":       "POST",
		"content_type": contentType,
		"headers":      headers,
		"body_base64":  base64.StdEncoding.EncodeToString(body),
		"auth_header":  auth,
		"bearer_token": token,
		"note":         "synthetic MailMoose forward webhook request; do not use as live data",
	}
	blob, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote contract fixture to %s", out)
}
