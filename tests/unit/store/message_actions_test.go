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
