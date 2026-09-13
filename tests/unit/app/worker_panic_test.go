package app_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/transport"
)

// panicTransport is a deliberately faulty outbound provider. Its Send panics,
// modelling a poison message that trips a bug in a transport adapter.
type panicTransport struct{}

func (panicTransport) Name() string        { return "panicprov" }
func (panicTransport) Description() string { return "Panicking test provider" }
func (panicTransport) ConfigFields() []transport.ConfigField {
	return nil
}
func (panicTransport) Send(context.Context, map[string]any, transport.OutboundMessage) (transport.OutboundResult, error) {
	panic("boom from transport")
}

func init() { transport.RegisterOutbound(panicTransport{}) }

// TestOutboxWorkerSurvivesDeliveryPanic verifies that a panic while delivering
// one message is contained: the worker stays up, the poison message is recorded
// as a failed attempt (so it backs off rather than re-panicking hot), and a
// healthy message queued behind it still delivers.
func TestOutboxWorkerSurvivesDeliveryPanic(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	// Domain 1: the poison provider.
	seedSending(t, svc, u.AccountID, dom.ID, "panicprov", map[string]any{})
	poison, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "poison", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Domain 2: a healthy provider on its own domain/inbox.
	dom2, err := svc.Store.CreateDomain(ctx, u.AccountID, "example2.com")
	if err != nil {
		t.Fatal(err)
	}
	box2, err := svc.Store.CreateInbox(ctx, u.AccountID, dom2.ID, "healthy", "Healthy")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<healthy-out>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom2.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	healthy, err := svc.Send(ctx, p, app.SendInput{InboxID: box2.ID, To: []string{"friend@example.net"}, Subject: "healthy", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}

	w := app.NewOutboxWorker(svc, nil)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	defer w.Stop()

	// The healthy message must still be delivered despite the poison message
	// being claimed first and panicking.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := svc.Store.GetMessageByID(ctx, u.AccountID, healthy.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if h.Status == "sent" {
			// The poison message was recorded as a failed attempt, not lost.
			poisonMsg, err := svc.Store.GetMessageByID(ctx, u.AccountID, poison.Message.ID)
			if err != nil {
				t.Fatal(err)
			}
			if poisonMsg.Attempts < 1 {
				t.Fatalf("poison attempts = %d, want >= 1", poisonMsg.Attempts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("healthy message not delivered after poison panic; calls=%d", calls.Load())
}
