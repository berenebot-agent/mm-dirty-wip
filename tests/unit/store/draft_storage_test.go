package store_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestDraftStorageAccounting(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	used := func() int64 {
		a, err := s.GetAccount(ctx, u.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		return a.StorageUsedBytes
	}

	base := used()
	d, err := s.CreateDraft(ctx, p, model.Draft{InboxID: b[0].ID, Text: "hello", HTML: "<p>hi</p>"})
	if err != nil {
		t.Fatal(err)
	}
	want := base + int64(len("hello")+len("<p>hi</p>"))
	if used() != want {
		t.Fatalf("after create used=%d want=%d", used(), want)
	}

	d.Text = "hello world"
	if _, err = s.UpdateDraft(ctx, p, d); err != nil {
		t.Fatal(err)
	}
	want = base + int64(len("hello world")+len("<p>hi</p>"))
	if used() != want {
		t.Fatalf("after update used=%d want=%d", used(), want)
	}

	att, err := s.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "a.txt", Size: 100, RawPath: "drafts/a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	want += 100
	if used() != want {
		t.Fatalf("after attachment used=%d want=%d", used(), want)
	}

	if _, err = s.DeleteDraftAttachment(ctx, p, d.ID, att.ID); err != nil {
		t.Fatal(err)
	}
	want -= 100
	if used() != want {
		t.Fatalf("after attachment delete used=%d want=%d", used(), want)
	}

	if _, err = s.DeleteDraftCascade(ctx, p, d.ID); err != nil {
		t.Fatal(err)
	}
	if used() != base {
		t.Fatalf("after delete used=%d want=%d", used(), base)
	}
}

func TestSendDraftConsumesWithoutDoubleCount(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	base, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(ctx, p, model.Draft{InboxID: b[0].ID, Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "a.txt", Size: 100, RawPath: "drafts/a.bin"}); err != nil {
		t.Fatal(err)
	}

	_, _, err = s.CommitOutbound(ctx, store.OutboundRecord{Inbox: b[0], Provider: "smtp", RFCMessageID: "<draft@test>",
		From: model.Address{Address: b[0].Address}, To: []string{"x@y.test"}, Subject: "s", Text: "body",
		RawPath: "messages/d.eml", SizeBytes: 500, DraftID: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDraft(ctx, p, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	after, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StorageUsedBytes != base.StorageUsedBytes+500 {
		t.Fatalf("storage used=%d want=%d (double counted?)", after.StorageUsedBytes, base.StorageUsedBytes+500)
	}
}

func TestPurgeInboxAccountsDraftBytes(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	base, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDraft(ctx, p, model.Draft{InboxID: b[0].ID, Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "a.txt", Size: 100, RawPath: "drafts/a.bin"}); err != nil {
		t.Fatal(err)
	}
	paths, err := s.PurgeInbox(ctx, u.AccountID, b[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range paths {
		if path == "drafts/a.bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("draft attachment path not returned: %#v", paths)
	}
	after, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StorageUsedBytes != base.StorageUsedBytes {
		t.Fatalf("storage used=%d want=%d", after.StorageUsedBytes, base.StorageUsedBytes)
	}
}
