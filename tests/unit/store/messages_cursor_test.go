package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestListMessagesBeforeCursor(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		m, _, _, err := s.CommitInbound(ctx, inbound(box, fmt.Sprintf("cursor-%d", i), fmt.Sprintf("<cursor-%d@test>", i), "", nil, fmt.Sprintf("subject %d", i), "body"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	idsOf := func(in []model.Message) []string {
		out := make([]string, 0, len(in))
		for _, m := range in {
			out = append(out, m.ID)
		}
		return out
	}

	first, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != ids[4] || first[1].ID != ids[3] {
		t.Fatalf("first page %v", idsOf(first))
	}
	second, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Before: first[1].ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != ids[2] || second[1].ID != ids[1] {
		t.Fatalf("second page %v", idsOf(second))
	}
	third, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Before: second[1].ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || third[0].ID != ids[0] {
		t.Fatalf("third page %v", idsOf(third))
	}
}

func TestUnreadCounts(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	for i := 0; i < 3; i++ {
		if _, _, _, err := s.CommitInbound(ctx, inbound(boxes[0], fmt.Sprintf("unread-%d", i), fmt.Sprintf("<unread-%d@test>", i), "", nil, "x", "body")); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.CommitInbound(ctx, inbound(boxes[1], "read-0", "<read-0@test>", "", nil, "y", "body")); err != nil {
		t.Fatal(err)
	}
	read := true
	msgs, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: boxes[1].ID, Limit: 1})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list %v %#v", err, msgs)
	}
	if err = s.UpdateMessageState(ctx, p, msgs[0].ID, &read, nil); err != nil {
		t.Fatal(err)
	}
	counts, err := s.UnreadCounts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if counts[boxes[0].ID] != 3 {
		t.Fatalf("inbox 0 unread = %d, want 3", counts[boxes[0].ID])
	}
	if counts[boxes[1].ID] != 0 {
		t.Fatalf("inbox 1 unread = %d, want 0", counts[boxes[1].ID])
	}
}
