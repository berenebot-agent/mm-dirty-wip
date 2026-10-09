package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestDomainLogSourceLabelsBothDirections proves the two-way domain log exposes
// a "source" for every row: the receiving source snapshotted on inbound mail
// (received, blocked and consumed approval-control) and the sending credential
// label on outbound attempts.
func TestDomainLogSourceLabelsBothDirections(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]

	received := inbound(box, "src-recv", "<recv@test>", "", nil, "hello", "body")
	received.Source = "Antler: antler1.hgolabs.com"
	if _, _, _, err := s.CommitInbound(ctx, received); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.CommitBlockedInbound(ctx, store.BlockedRecord{
		AccountID: u.AccountID, InboxID: box.ID, Provider: "mx",
		Source: "Direct MX", ProviderDeliveryID: "src-block",
		EnvelopeRecipient: box.Address,
		From:              model.Address{Address: "bad@outside.test"},
		To:                []string{box.Address}, Subject: "blocked", Reason: "sender not allowed",
		SizeBytes: 10, ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecordControlMessage(ctx, store.ControlMessageRecord{
		AccountID: u.AccountID, InboxID: box.ID, Provider: "mx",
		Source: "Remote MX", ProviderDeliveryID: "src-ctl",
		EnvelopeRecipient: box.Address,
		FromAddress:       "approver@outside.test",
		RequestID:         "dsr_x", Action: "approve", Outcome: "approved",
	}); err != nil {
		t.Fatal(err)
	}

	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>",
		From: model.Address{Address: box.Address}, To: []string{"x@y.test"},
		Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100,
		ClientLabel: "My API key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkSent(ctx, u.AccountID, m.ID, "<provider-id>", "brevo"); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListDomainLog(ctx, u.AccountID, d.ID, 50, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"received": "Antler: antler1.hgolabs.com",
		"blocked":  "Direct MX",
		"approval": "Remote MX",
		"sent":     "My API key",
	}
	seen := map[string]string{}
	for _, e := range entries {
		seen[e.Kind] = e.Source
	}
	for kind, source := range want {
		if seen[kind] != source {
			t.Fatalf("kind %q source=%q, want %q (all: %v)", kind, seen[kind], source, seen)
		}
	}
}
