package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// ambiguousTransport models a provider send whose outcome is unknown: the
// request may have been accepted. It must be recorded terminally, with no retry
// scheduled, so a non-idempotent provider cannot be sent the same mail twice.
type ambiguousTransport struct{}

func (ambiguousTransport) Name() string                          { return "ambiguousprov" }
func (ambiguousTransport) Description() string                   { return "Ambiguous test provider" }
func (ambiguousTransport) ConfigFields() []transport.ConfigField { return nil }
func (ambiguousTransport) Send(context.Context, map[string]any, transport.OutboundMessage) (transport.OutboundResult, error) {
	return transport.OutboundResult{}, &transport.AmbiguousError{Err: context.DeadlineExceeded}
}

func init() { transport.RegisterOutbound(ambiguousTransport{}) }

func TestDeliverAmbiguousErrorIsTerminal(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, dom.ID, "ambiguousprov", map[string]any{})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Ambig", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err == nil {
		t.Fatal("expected a delivery error")
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "failed" {
		t.Fatalf("status %q, want failed (ambiguous must be terminal)", m.Status)
	}
	if m.Attempts != 1 {
		t.Fatalf("attempts %d, want 1 (no retry)", m.Attempts)
	}
	if m.NextRetry != "" {
		t.Fatalf("next_retry %q, want empty (no retry scheduled)", m.NextRetry)
	}
}

// testServiceConcurrency builds a service whose outbox runs n senders, so a
// test can observe concurrent delivery.
func testServiceConcurrency(t *testing.T, n int) (*app.Service, model.User, model.Domain, model.Inbox) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 10, SendLimitPerMinute: 60, OutboundConcurrency: n}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	return svc, u, d, b
}

// TestOutboxWorkerDeliversConcurrently proves the sender pool delivers more than
// one message at a time: with several senders and a provider that holds each
// request open, more than one request must be in flight simultaneously.
func TestOutboxWorkerDeliversConcurrently(t *testing.T) {
	const concurrency = 5
	const messages = 10
	svc, u, dom, box := testServiceConcurrency(t, concurrency)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}

	var inFlight, maxInFlight atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<concurrent-out>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	ids := make([]string, 0, messages)
	for i := 0; i < messages; i++ {
		res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "batch", Text: "hi"}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Message.ID)
	}

	w := app.NewOutboxWorker(svc, nil)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	defer w.Stop()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		allSent := true
		for _, id := range ids {
			m, err := svc.Store.GetMessageByID(ctx, u.AccountID, id)
			if err != nil {
				t.Fatal(err)
			}
			if m.Status != "sent" {
				allSent = false
				break
			}
		}
		if allSent {
			if got := maxInFlight.Load(); got < 2 {
				t.Fatalf("max concurrent deliveries = %d, want >= 2 with %d senders", got, concurrency)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("batch not fully delivered; maxInFlight=%d", maxInFlight.Load())
}
