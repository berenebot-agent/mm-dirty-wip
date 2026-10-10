package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestUpdateMessageStateEmitsEvent proves an actual read-state change commits a
// message.state_changed event carrying the old and new values, that a repeat set
// to the same value is a no-op returning no event, and that the event is visible
// through the durable event list (so a reconnecting UI can catch up).
func TestUpdateMessageStateEmitsEvent(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	box := boxes[0]

	m, _, _, err := s.CommitInbound(ctx, inbound(box, "state-evt", "<state-evt@test>", "", nil, "hello", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Read {
		t.Fatal("a freshly received message must start unread")
	}

	read := true
	ev, err := s.UpdateMessageState(ctx, p, m.ID, &read)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil {
		t.Fatal("the first read-state change must emit an event")
	}
	if ev.Type != model.EventMessageStateChanged {
		t.Fatalf("event type = %q, want %q", ev.Type, model.EventMessageStateChanged)
	}
	if got := ev.Payload["new"]; got != true {
		t.Fatalf("event new = %#v, want true", got)
	}
	if got := ev.Payload["old"]; got != false {
		t.Fatalf("event old = %#v, want false", got)
	}

	// Idempotent: setting the same value again produces no event.
	if ev2, err := s.UpdateMessageState(ctx, p, m.ID, &read); err != nil {
		t.Fatal(err)
	} else if ev2 != nil {
		t.Fatalf("no-op read-state set must not emit an event, got %#v", ev2)
	}

	// A nil read pointer is also a no-op.
	if ev3, err := s.UpdateMessageState(ctx, p, m.ID, nil); err != nil {
		t.Fatal(err)
	} else if ev3 != nil {
		t.Fatalf("nil read must not emit an event, got %#v", ev3)
	}

	// The event is durable and reachable by cursor.
	events, err := s.ListEvents(ctx, p, 0, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == model.EventMessageStateChanged && e.EntityID == m.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("message.state_changed event not present in durable history: %#v", events)
	}
}
