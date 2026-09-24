package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestListEventsRedactsAssistantFieldsForReadPrincipal covers the scope-parity
// fix: a read-only principal must not receive the assistant-scoped approval
// fields on the events feed, while an assistant/owner still does.
func TestListEventsRedactsAssistantFieldsForReadPrincipal(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	_, ownerKey, err := s.CreateAPIKey(ctx, u.AccountID, "owner", false, map[string]string{b[0].ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.APIKeyPrincipal(ctx, ownerKey)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(ctx, owner, model.Draft{InboxID: b[0].ID, Subject: "s", Text: "b", To: []string{"x@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CreateSendRequest(ctx, owner, d.ID, "hash"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RejectSendRequest(ctx, owner, d.ID, "secret feedback", model.DecisionMethodAPI); err != nil {
		t.Fatal(err)
	}

	_, readKey, err := s.CreateAPIKey(ctx, u.AccountID, "read", false, map[string]string{b[0].ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.APIKeyPrincipal(ctx, readKey)
	if err != nil {
		t.Fatal(err)
	}
	readEvents, err := s.ListEvents(ctx, read, 0, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range readEvents {
		if e.Type != model.EventDraftRejected {
			continue
		}
		found = true
		for _, k := range []string{"feedback", "decision_actor", "decision_method", "approver_email"} {
			if _, ok := e.Payload[k]; ok {
				t.Fatalf("read principal received assistant field %q: %#v", k, e.Payload)
			}
		}
	}
	if !found {
		t.Fatal("no draft.rejected event returned to the read principal")
	}

	_, asstKey, err := s.CreateAPIKey(ctx, u.AccountID, "assistant", false, map[string]string{b[0].ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	asst, err := s.APIKeyPrincipal(ctx, asstKey)
	if err != nil {
		t.Fatal(err)
	}
	asstEvents, err := s.ListEvents(ctx, asst, 0, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, e := range asstEvents {
		if e.Type != model.EventDraftRejected {
			continue
		}
		found = true
		if e.Payload["feedback"] != "secret feedback" {
			t.Fatalf("assistant feedback = %v, want secret feedback", e.Payload["feedback"])
		}
		if _, ok := e.Payload["decision_actor"]; !ok {
			t.Fatalf("assistant missing decision_actor: %#v", e.Payload)
		}
	}
	if !found {
		t.Fatal("no draft.rejected event returned to the assistant principal")
	}
}
