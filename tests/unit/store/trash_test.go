package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestTrashRestorePurgeLifecycle covers the durable Trash contract: delete
// hides but retains, restore returns, and purge erases and reclaims storage.
func TestTrashRestorePurgeLifecycle(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	m, _, _, err := s.CommitInbound(ctx, inbound(box, "trash-1", "<t1@test>", "", nil, "Trash me", "body"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}

	// Trash hides but retains.
	got, ev, err := s.TrashMessage(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Type != model.EventMessageTrashed {
		t.Fatalf("trash event %#v", ev)
	}
	if got.DeletedAt == nil {
		t.Fatal("expected deleted_at set")
	}
	ordinary, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) != 0 {
		t.Fatalf("trashed message still visible: %#v", ordinary)
	}
	trashed, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Trashed: true})
	if err != nil || len(trashed) != 1 {
		t.Fatalf("trash list %v %#v", err, trashed)
	}
	if n, _ := s.CountTrash(ctx, p, box.ID); n != 1 {
		t.Fatalf("trash count %d", n)
	}
	// Trashing again is a no-op.
	if _, ev2, err := s.TrashMessage(ctx, p, m.ID); err != nil || ev2 != nil {
		t.Fatalf("re-trash %v %#v", err, ev2)
	}
	// Storage is retained while trashed.
	mid, _ := s.GetAccount(ctx, u.AccountID)
	if mid.StorageUsedBytes != before.StorageUsedBytes {
		t.Fatalf("storage changed on trash: %d -> %d", before.StorageUsedBytes, mid.StorageUsedBytes)
	}

	// Restore returns it.
	restored, ev, err := s.RestoreMessage(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Type != model.EventMessageRestored || restored.DeletedAt != nil {
		t.Fatalf("restore %#v %#v", restored, ev)
	}
	if ordinary, _ = s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID}); len(ordinary) != 1 {
		t.Fatalf("restored message not visible: %#v", ordinary)
	}

	// A message must be trashed before it can be purged.
	if _, _, _, err := s.PurgeMessage(ctx, p, m.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("purge untrashed err=%v", err)
	}

	// Trash then purge erases and reclaims storage.
	if _, _, err := s.TrashMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	path, size, pev, err := s.PurgeMessage(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pev.Type != model.EventMessagePurged || size != m.SizeBytes || path != m.RawPath {
		t.Fatalf("purge result path=%q size=%d ev=%#v", path, size, pev)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged message still present: %v", err)
	}
	after, _ := s.GetAccount(ctx, u.AccountID)
	if after.StorageUsedBytes != before.StorageUsedBytes-m.SizeBytes {
		t.Fatalf("storage not reclaimed: %d -> %d", before.StorageUsedBytes, after.StorageUsedBytes)
	}
}

// TestTrashRequiresAssistantAndPurgeRequiresAssistant checks the role gates:
// trashing and purging both require at least Assistant (Read is refused), since
// the only difference between Assistant and Owner is sending.
func TestTrashRequiresAssistantAndPurgeRequiresAssistant(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "trash-2", "<t2@test>", "", nil, "s", "b"))
	if err != nil {
		t.Fatal(err)
	}
	read := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, _, err := s.TrashMessage(ctx, read, m.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read trash err=%v", err)
	}
	assistant := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "assistant"}}
	if _, _, err := s.TrashMessage(ctx, assistant, m.ID); err != nil {
		t.Fatalf("assistant trash err=%v", err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, read, m.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read purge err=%v", err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, assistant, m.ID); err != nil {
		t.Fatalf("assistant purge err=%v", err)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged message still present: %v", err)
	}
}

// TestEmptyTrash purges every trashed message in an inbox and returns their raw
// paths, leaving untrashed mail intact.
func TestEmptyTrash(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	a, _, _, err := s.CommitInbound(ctx, inbound(box, "et-1", "<et1@test>", "", nil, "a", "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := s.CommitInbound(ctx, inbound(box, "et-2", "<et2@test>", "", nil, "b", "b"))
	if err != nil {
		t.Fatal(err)
	}
	keep, _, _, err := s.CommitInbound(ctx, inbound(box, "et-3", "<et3@test>", "", nil, "keep", "keep"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := s.TrashMessage(ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}
	paths, events, err := s.EmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || len(events) != 2 {
		t.Fatalf("empty trash paths=%v events=%d", paths, len(events))
	}
	if n, _ := s.CountTrash(ctx, p, box.ID); n != 0 {
		t.Fatalf("trash not empty: %d", n)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, keep.ID); err != nil {
		t.Fatalf("untrashed message removed: %v", err)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged message present: %v", err)
	}
}

// TestPurgeExpiredTrash verifies the retention sweep honors the per-account
// window and skips accounts configured to keep trash forever (0).
func TestPurgeExpiredTrash(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "ret-1", "<r1@test>", "", nil, "s", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrashMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}

	// 0 disables the sweep: nothing is purged.
	if err := s.SetTrashRetention(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	if paths, err := s.PurgeExpiredTrash(ctx); err != nil || len(paths) != 0 {
		t.Fatalf("retention 0 purged %v err=%v", paths, err)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, m.ID); err != nil {
		t.Fatalf("message purged with retention 0: %v", err)
	}

	// Default 30 days retains a freshly-trashed message.
	if err := s.SetTrashRetention(ctx, p, 30); err != nil {
		t.Fatal(err)
	}
	if paths, err := s.PurgeExpiredTrash(ctx); err != nil || len(paths) != 0 {
		t.Fatalf("fresh trash purged %v err=%v", paths, err)
	}

	// Backdate the trash timestamp beyond the window.
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id=?`, "2000-01-01T00:00:00Z", m.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	paths, err := s.PurgeExpiredTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expired trash not purged: %v", paths)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired message present: %v", err)
	}
}

// TestTrashRetentionSetting covers the account preference round-trip and
// validation.
func TestTrashRetentionSetting(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if days, err := s.GetTrashRetention(ctx, p); err != nil || days != 0 {
		t.Fatalf("default retention %d err=%v", days, err)
	}
	if err := s.SetTrashRetention(ctx, p, 7); err != nil {
		t.Fatal(err)
	}
	if days, _ := s.GetTrashRetention(ctx, p); days != 7 {
		t.Fatalf("retention %d", days)
	}
	if err := s.SetTrashRetention(ctx, p, -1); err == nil {
		t.Fatal("negative retention accepted")
	}
}

// TestInboxTrashRetentionOverride covers the per-inbox override: an unset
// override reads back as nil (inherit), a set value round-trips, a value of 0
// is distinct from unset, and clearing returns to inherit.
func TestInboxTrashRetentionOverride(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]

	// New inboxes inherit: the override is absent.
	got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrashRetentionDays != nil {
		t.Fatalf("new inbox override = %v, want nil", *got.TrashRetentionDays)
	}

	// Set 0: a present pointer to zero, distinct from inherit.
	zero := 0
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, &zero); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.TrashRetentionDays == nil || *got.TrashRetentionDays != 0 {
		t.Fatalf("override after set 0 = %v", got.TrashRetentionDays)
	}

	// Set a positive value.
	days := 14
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, &days); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.TrashRetentionDays == nil || *got.TrashRetentionDays != 14 {
		t.Fatalf("override after set 14 = %v", got.TrashRetentionDays)
	}

	// Clear back to inherit.
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.TrashRetentionDays != nil {
		t.Fatalf("override after clear = %v", *got.TrashRetentionDays)
	}

	// Negative is rejected, and an unknown inbox is not found.
	neg := -1
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, &neg); err == nil {
		t.Fatal("negative override accepted")
	}
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, "inbox_missing", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown inbox err = %v", err)
	}
}

// TestPurgeExpiredTrashInboxOverride verifies the sweep uses the per-inbox
// override when set: it can both shorten (purge sooner) and lengthen (keep
// longer) the effective window, and an inbox override of 0 keeps its trash even
// when the account auto-purges.
func TestPurgeExpiredTrashInboxOverride(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box, other := boxes[0], boxes[1]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	// Account default 30 days; two messages trashed and backdated 10 days.
	if err := s.SetTrashRetention(ctx, p, 30); err != nil {
		t.Fatal(err)
	}
	mk := func(box model.Inbox, id string) model.Message {
		m, _, _, err := s.CommitInbound(ctx, inbound(box, id, "<"+id+"@test>", "", nil, "s", "b"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.TrashMessage(ctx, p, m.ID); err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := mk(box, "ovr-a")
	b := mk(other, "ovr-b")
	// Backdate ~100 days: inside the account's 30-day window, but well inside a
	// 3650-day inbox override.
	backdate := time.Now().UTC().AddDate(0, 0, -100).Format("2006-01-02T15:04:05Z")
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id IN (?,?)`, backdate, a.ID, b.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	// No override yet: both are far past the 30-day window and purge.
	if paths, err := s.PurgeExpiredTrash(ctx); err != nil || len(paths) != 2 {
		t.Fatalf("baseline purge paths=%v err=%v", paths, err)
	}

	// A longer per-inbox override keeps that inbox's old trash.
	c := mk(box, "ovr-c")
	d := mk(other, "ovr-d")
	db = rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id IN (?,?)`, backdate, c.ID, d.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	keep := 3650
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, &keep); err != nil {
		t.Fatal(err)
	}
	paths, err := s.PurgeExpiredTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("override purge paths=%v, want only the non-overridden inbox", paths)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, c.ID); err != nil {
		t.Fatalf("overridden inbox trash purged: %v", err)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-overridden inbox trash retained: %v", err)
	}

	// An override of 0 keeps this inbox's trash even though the account
	// auto-purges.
	e := mk(box, "ovr-e")
	db = rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id=?`, backdate, e.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	zero := 0
	if err := s.SetInboxTrashRetention(ctx, u.AccountID, box.ID, &zero); err != nil {
		t.Fatal(err)
	}
	if paths, err := s.PurgeExpiredTrash(ctx); err != nil || len(paths) != 0 {
		t.Fatalf("zero override purge paths=%v err=%v", paths, err)
	}
	if _, err := s.GetMessageByID(ctx, u.AccountID, e.ID); err != nil {
		t.Fatalf("zero override purged trash: %v", err)
	}
}
