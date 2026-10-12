package store_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestMessageActionsRoundRobin(t *testing.T) {
	s, u, _, boxes := testStore(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].ID < boxes[j].ID })
	enqueue := func(box model.Inbox, message string) store.MessageAction {
		t.Helper()
		a, err := s.EnqueueMessageAction(ctx, p, box.ID, message, store.MessageActionInput{Kind: "delete"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	// The oldest global backlog must not monopolize execution. Two actions
	// also target the same message to exercise ordering inside an inbox.
	a1 := enqueue(boxes[0], "a")
	a2 := enqueue(boxes[0], "a")
	a3 := enqueue(boxes[0], "other")
	b1 := enqueue(boxes[1], "b")
	b2 := enqueue(boxes[1], "b")
	c1 := enqueue(boxes[2], "c")
	cursor := ""
	for i, want := range []store.MessageAction{a1, b1, c1, a2, b2, a3} {
		got, ok, err := s.ClaimMessageActionAfterInbox(ctx, "test", cursor)
		if err != nil || !ok || got.ID != want.ID {
			t.Fatalf("turn %d: got %+v, ok=%v, err=%v; want %s", i, got, ok, err, want.ID)
		}
		cursor = got.InboxID
		if err := s.SettleMessageAction(ctx, got.ID, "done", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := s.ClaimMessageActionAfterInbox(ctx, "test", cursor); err != nil || ok {
		t.Fatalf("empty queue: ok=%v err=%v", ok, err)
	}
	// A newly populated inbox joins rotation even after an empty drain, and
	// a deleted cursor inbox need not exist for the rotation to wrap.
	joined := enqueue(boxes[3], "new")
	got, ok, err := s.ClaimMessageActionAfterInbox(ctx, "test", cursor)
	if err != nil || !ok || got.ID != joined.ID {
		t.Fatalf("new inbox: %+v %v %v", got, ok, err)
	}
	if err := s.SettleMessageAction(ctx, got.ID, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	wrapped := enqueue(boxes[0], "wrapped")
	got, ok, err = s.ClaimMessageActionAfterInbox(ctx, "test", "zzzz-nonexistent-inbox")
	if err != nil || !ok || got.ID != wrapped.ID {
		t.Fatalf("missing cursor wrap: %+v %v %v", got, ok, err)
	}
}

func TestMessageActionsRoundRobinSkipsBlockedMessages(t *testing.T) {
	for _, state := range []string{"pending", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			s, u, _, boxes := testStore(t)
			ctx := context.Background()
			p := model.Principal{AccountID: u.AccountID, Admin: true}
			sort.Slice(boxes, func(i, j int) bool { return boxes[i].ID < boxes[j].ID })
			enqueue := func(box model.Inbox, message string) store.MessageAction {
				t.Helper()
				a, err := s.EnqueueMessageAction(ctx, p, box.ID, message, store.MessageActionInput{Kind: "delete"})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			blocked := enqueue(boxes[0], "blocked")
			got, ok, err := s.ClaimMessageActionAfterInbox(ctx, "test", "")
			if err != nil || !ok || got.ID != blocked.ID {
				t.Fatalf("initial claim: %+v %v %v", got, ok, err)
			}
			if err := s.SettleMessageAction(ctx, blocked.ID, state, "", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			enqueue(boxes[0], "blocked")
			other := enqueue(boxes[1], "other")
			got, ok, err = s.ClaimMessageActionAfterInbox(ctx, "test", boxes[1].ID)
			if err != nil || !ok || got.ID != other.ID {
				t.Fatalf("blocked inbox should be skipped: %+v %v %v", got, ok, err)
			}
			if err := s.SettleMessageAction(ctx, got.ID, "done", "", time.Now()); err != nil {
				t.Fatal(err)
			}
			independent := enqueue(boxes[0], "independent")
			got, ok, err = s.ClaimMessageActionAfterInbox(ctx, "test", boxes[1].ID)
			if err != nil || !ok || got.ID != independent.ID {
				t.Fatalf("independent message blocked: %+v %v %v", got, ok, err)
			}
			if err := s.SettleMessageAction(ctx, got.ID, "done", "", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.ClaimMessageActionAfterInbox(ctx, "test", ""); err != nil || ok {
				t.Fatalf("blocked successor claimed: %v %v", ok, err)
			}
		})
	}
}

func TestMessageActionsRecheckSelectionAndRequeue(t *testing.T) {
	s, u, _, boxes := testStore(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	box := boxes[0]
	enqueue := func(message string) store.MessageAction {
		t.Helper()
		a, err := s.EnqueueMessageAction(ctx, p, box.ID, message, store.MessageActionInput{Kind: "delete"})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	failedA := enqueue("a")
	failedB := enqueue("b")
	ambiguous := enqueue("c")
	dismissed := enqueue("d")
	done := enqueue("e")
	for _, a := range []store.MessageAction{failedA, failedB, ambiguous, dismissed, done} {
		if _, ok, err := s.ClaimMessageAction(ctx, "test"); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		switch a.ID {
		case ambiguous.ID:
			if err := s.SettleMessageAction(ctx, a.ID, "ambiguous", "uncertain", time.Now()); err != nil {
				t.Fatal(err)
			}
		default:
			if err := s.SettleMessageAction(ctx, a.ID, "failed", "boom", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.SettleMessageAction(ctx, done.ID, "done", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateMessageAction(ctx, p, dismissed.ID, false); err != nil {
		t.Fatal(err)
	}
	// Only non-dismissed failed rows are recheck candidates.
	got, err := s.ListFailedActionsForRecheck(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	if !ids[failedA.ID] || !ids[failedB.ID] {
		t.Fatalf("failed actions missing from recheck: %+v", got)
	}
	if ids[ambiguous.ID] || ids[dismissed.ID] || ids[done.ID] {
		t.Fatalf("ineligible action selected: %+v", got)
	}
	// A future-dated deferral is not yet due.
	if err := s.DeferMessageActionRecheck(ctx, failedA.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListFailedActionsForRecheck(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.ID == failedA.ID {
			t.Fatal("deferred action selected too early")
		}
	}
	// Requeue returns the row to pending so the worker can claim it again.
	if err := s.RequeueMessageAction(ctx, failedB.ID); err != nil {
		t.Fatal(err)
	}
	back, err := s.GetMessageAction(ctx, p, failedB.ID)
	if err != nil || back.State != "pending" {
		t.Fatalf("requeued action: %+v %v", back, err)
	}
	// Requeue is conditional on the row still being failed: a settled row is not
	// resurrected by a stale recheck.
	if err := s.RequeueMessageAction(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	settled, err := s.GetMessageAction(ctx, p, done.ID)
	if err != nil || settled.State != "done" {
		t.Fatalf("done row resurrected: %+v %v", settled, err)
	}
	if err := s.RequeueMessageAction(ctx, dismissed.ID); err != nil {
		t.Fatal(err)
	}
	dism, err := s.GetMessageAction(ctx, p, dismissed.ID)
	if err != nil || dism.State == "pending" {
		t.Fatalf("dismissed row resurrected: %+v %v", dism, err)
	}
}
