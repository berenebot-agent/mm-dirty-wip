package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestReplaceMessageLabelsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	m, _, _, err := s.CommitInbound(ctx, inbound(box, "lbl-1", "<lbl-1@test>", "", nil, "Label me", "body"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.ReplaceMessageLabels(ctx, p, m.ID, []string{"  Invoices ", "Unpaid", "invoices"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != model.EventMessageLabelsChanged {
		t.Fatalf("event type %q", ev.Type)
	}

	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "Invoices" || got.Labels[1] != "Unpaid" {
		t.Fatalf("labels = %#v", got.Labels)
	}

	msgs, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Limit: 10})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list %v %#v", err, msgs)
	}
	if len(msgs[0].Labels) != 2 {
		t.Fatalf("list labels %#v", msgs[0].Labels)
	}

	// An empty set clears the labels.
	if _, err = s.ReplaceMessageLabels(ctx, p, m.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if len(got.Labels) != 0 {
		t.Fatalf("labels after clear %#v", got.Labels)
	}
}

func TestMessageLabelFilterAnd(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	both, _, _, err := s.CommitInbound(ctx, inbound(box, "lbl-both", "<lbl-both@test>", "", nil, "Both", "body"))
	if err != nil {
		t.Fatal(err)
	}
	one, _, _, err := s.CommitInbound(ctx, inbound(box, "lbl-one", "<lbl-one@test>", "", nil, "One", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, p, both.ID, []string{"Alpha", "Beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, p, one.ID, []string{"Alpha"}); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Labels: []string{"alpha", "BETA"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != both.ID {
		t.Fatalf("AND filter = %#v", msgs)
	}

	msgs, err = s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Labels: []string{"alpha"}, Limit: 10})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("single label filter %v %#v", err, msgs)
	}
}

func TestListLabelsScope(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	admin := model.Principal{AccountID: u.AccountID, Admin: true}

	m0, _, _, err := s.CommitInbound(ctx, inbound(boxes[0], "scope-0", "<scope-0@test>", "", nil, "Zero", "body"))
	if err != nil {
		t.Fatal(err)
	}
	m1, _, _, err := s.CommitInbound(ctx, inbound(boxes[1], "scope-1", "<scope-1@test>", "", nil, "One", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, admin, m0.ID, []string{"Alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, admin, m1.ID, []string{"Beta"}); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListLabels(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0] != "Alpha" || all[1] != "Beta" {
		t.Fatalf("admin labels %#v", all)
	}

	_, key, err := s.CreateAPIKey(ctx, u.AccountID, "scoped", false, map[string]string{boxes[0].ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := s.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := s.ListLabels(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0] != "Alpha" {
		t.Fatalf("scoped labels %#v", visible)
	}
}

func TestMessageLabelsCascadeOnDelete(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	m, _, _, err := s.CommitInbound(ctx, inbound(box, "cascade-1", "<cascade-1@test>", "", nil, "Cascade", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, p, m.ID, []string{"Gone"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.DeleteMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	labels, err := s.ListLabels(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Fatalf("labels after delete %#v", labels)
	}
}

func TestReplaceMessageLabelsForbidden(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "forbid-1", "<forbid-1@test>", "", nil, "Forbidden", "body"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := s.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, reader, m.ID, []string{"Nope"}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read principal labelled: %v", err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, admin, m.ID, []string{"ok"}); err != nil {
		t.Fatalf("admin could not label: %v", err)
	}
}

func TestReplaceMessageLabelsInvalid(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m, _, _, err := s.CommitInbound(ctx, inbound(boxes[0], "invalid-1", "<invalid-1@test>", "", nil, "Invalid", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceMessageLabels(ctx, p, m.ID, []string{""}); err == nil {
		t.Fatal("empty label accepted")
	}
	if _, err = s.ReplaceMessageLabels(ctx, p, m.ID, []string{strings.Repeat("x", model.MaxLabelLength+1)}); err == nil {
		t.Fatal("overlong label accepted")
	}
}
