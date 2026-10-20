package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
)

// slowFailTransport models a provider whose send outlives the delivery's
// outcome-write budget: it blocks, then returns a transient error. Before the
// fix the outcome context was created before the send and had already expired,
// so the failed attempt was never recorded, attempts stayed at zero, and the
// message looped forever on the claim lease.
type slowFailTransport struct{}

func (slowFailTransport) Name() string                          { return "slowfailprov" }
func (slowFailTransport) Description() string                   { return "Slow failing test provider" }
func (slowFailTransport) ConfigFields() []transport.ConfigField { return nil }
func (slowFailTransport) Send(ctx context.Context, _ map[string]any, _ transport.OutboundMessage) (transport.OutboundResult, error) {
	select {
	case <-time.After(250 * time.Millisecond):
		return transport.OutboundResult{}, errors.New("remote stalled")
	case <-ctx.Done():
		return transport.OutboundResult{}, ctx.Err()
	}
}

func init() { transport.RegisterOutbound(slowFailTransport{}) }

// TestDeliverRecordsOutcomeAfterSlowSend proves that a send which takes longer
// than the outcome-write budget still records its failure: attempts advances and
// last_error is set, instead of leaving the message pending with no outcome and
// the sending log stuck on "sending".
func TestDeliverRecordsOutcomeAfterSlowSend(t *testing.T) {
	app.SetOutcomeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() { app.SetOutcomeTimeoutForTest(15 * time.Second) })

	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, dom.ID, "slowfailprov", map[string]any{})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "slow", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err == nil {
		t.Fatal("expected delivery to fail")
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Attempts < 1 {
		t.Fatalf("attempts = %d, want >= 1", m.Attempts)
	}
	if m.LastError == "" {
		t.Fatal("last_error not recorded")
	}
	if m.Status != "pending" {
		t.Fatalf("status = %q, want pending", m.Status)
	}
}
