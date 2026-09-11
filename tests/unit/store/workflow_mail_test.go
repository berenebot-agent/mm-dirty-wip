package store_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestUpdateInboxMissingReturnsNotFound(t *testing.T) {
	s, u, _, _ := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if err := s.UpdateInbox(context.Background(), p, "inb_missing", "name", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing inbox update err=%v, want ErrNotFound", err)
	}
}

// TestInternalWorkflowMailHiddenFromReads covers the workflow-mail flag: a
// message carrying an approval token is queued in the inbox but must never be
// reachable through any principal-facing read surface, while the provider and
// worker can still reach it by id.
func TestInternalWorkflowMailHiddenFromReads(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	admin := model.Principal{AccountID: u.AccountID, Admin: true}

	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "", RFCMessageID: "<wf@test>",
		From: model.Address{Address: box.Address}, To: []string{"approver@outside.test"},
		Subject: "Approval required: proposal", Text: "token-value", RawPath: "messages/wf.eml",
		SizeBytes: 42, Internal: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetMessage(ctx, admin, m.ID); err == nil {
		t.Fatal("internal message readable through principal GetMessage")
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, m.ID); err != nil {
		t.Fatalf("worker by-id lookup failed: %v", err)
	}
	if msgs, err := s.ListMessages(ctx, admin, store.MessageFilter{InboxID: box.ID}); err != nil || len(msgs) != 0 {
		t.Fatalf("ListMessages leaked internal mail: err=%v msgs=%#v", err, msgs)
	}
	if found, err := s.SearchMessages(ctx, admin, "token-value", box.ID, 10); err != nil || len(found) != 0 {
		t.Fatalf("search leaked internal mail: err=%v found=%#v", err, found)
	}
	if sizes, err := s.MessageSizesByInbox(ctx, admin); err != nil || sizes[box.ID] != 0 {
		t.Fatalf("sizes leaked internal mail: err=%v sizes=%#v", err, sizes)
	}
	if outbox, err := s.ListOutbox(ctx, admin, box.ID, 10); err != nil || len(outbox) != 0 {
		t.Fatalf("outbox leaked internal mail: err=%v outbox=%#v", err, outbox)
	}

	// Delivering workflow mail emits no message.sent event, so realtime and
	// replay surfaces do not reveal it.
	_, events, err := s.MarkSent(ctx, u.AccountID, m.ID, "provider-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("internal MarkSent emitted events: %#v", events)
	}
}

// TestInternalWorkflowMailDoesNotSuppressNormalEvents guards the suppression:
// ordinary outbound mail still emits message.sent.
func TestInternalWorkflowMailDoesNotSuppressNormalEvents(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "smtp", RFCMessageID: "<normal@test>",
		From: model.Address{Address: box.Address}, To: []string{"friend@outside.test"},
		Subject: "hello", Text: "body", RawPath: "messages/n.eml", SizeBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, events, err := s.MarkSent(ctx, u.AccountID, m.ID, "provider-2", "smtp")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != "message.sent" {
		t.Fatalf("normal MarkSent events = %#v", events)
	}
}
