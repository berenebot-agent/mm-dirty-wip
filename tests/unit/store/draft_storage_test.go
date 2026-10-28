package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestMovingDraftTransfersAttachmentQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	source, dest := boxes[0], boxes[1]
	d, err := s.CreateDraft(ctx, p, model.Draft{InboxID: source.ID, Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "large.bin", Size: 100, RawPath: "drafts/large.bin"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	quota := int64(50)
	if err := s.SetInboxStorageQuota(ctx, u.AccountID, dest.ID, &quota); err != nil {
		t.Fatal(err)
	}
	d.InboxID, d.FromAddress = dest.ID, dest.Address
	if _, err := s.UpdateDraft(ctx, p, d); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("move bypassed attachment quota: %v", err)
	}
	assertUsed := func(sourceBytes, destBytes int64) {
		t.Helper()
		for id, want := range map[string]int64{source.ID: sourceBytes, dest.ID: destBytes} {
			box, err := s.GetInboxInternal(ctx, u.AccountID, id)
			if err != nil || box.StorageUsedBytes != want {
				t.Fatalf("inbox %s usage=%d want=%d err=%v", id, box.StorageUsedBytes, want, err)
			}
		}
	}
	assertUsed(104, 0)
	if err := s.SetInboxStorageQuota(ctx, u.AccountID, dest.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDraft(ctx, p, d); err != nil {
		t.Fatal(err)
	}
	assertUsed(0, 104)
	after, err := s.GetAccount(ctx, u.AccountID)
	if err != nil || after.StorageUsedBytes != before.StorageUsedBytes {
		t.Fatalf("move changed account usage: %+v err=%v", after, err)
	}
	if _, err := s.DeleteDraftCascade(ctx, p, d.ID); err != nil {
		t.Fatal(err)
	}
	assertUsed(0, 0)
}

func TestDraftStorageAccounting(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
