package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// setInboxQuota writes a cap directly so the test can exercise enforcement
// without the HTTP layer.
func setInboxQuota(t *testing.T, s *store.Store, accountID, inboxID string, quota *int64) {
	t.Helper()
	if err := s.SetInboxStorageQuota(context.Background(), accountID, inboxID, quota); err != nil {
		t.Fatal(err)
	}
}

// TestInboxQuotaEnforcedIndependentlyOfAccount proves a per-inbox cap rejects
// inbound and outbound mail that the account quota would otherwise accept, and
// that an uncapped sibling inbox is unaffected.
func TestInboxQuotaEnforcedIndependentlyOfAccount(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box, other := b[0], b[1]

	// Cap the first inbox at 50 bytes; the account quota is far larger.
	quota := int64(50)
	setInboxQuota(t, s, u.AccountID, box.ID, &quota)

	rec := inbound(box, "iq-1", "<iq-1@test>", "", nil, "Big", "x")
	rec.SizeBytes = 100
	if _, _, _, err := s.CommitInbound(ctx, rec); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("inbound over inbox cap: %v", err)
	}
	out := store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<iq-out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100, SentAt: time.Now().UTC()}
	if _, _, err := s.CommitOutbound(ctx, out); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("outbound over inbox cap: %v", err)
	}

	// A message that fits the cap is accepted and charges the inbox counter.
	small := inbound(box, "iq-2", "<iq-2@test>", "", nil, "Small", "x")
	small.SizeBytes = 40
	if _, _, _, err := s.CommitInbound(ctx, small); err != nil {
		t.Fatalf("inbound within inbox cap: %v", err)
	}
	used, err := s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 40 {
		t.Fatalf("inbox used = %d, want 40", used)
	}
	// The next 20 bytes would exceed 50.
	more := inbound(box, "iq-3", "<iq-3@test>", "", nil, "More", "x")
	more.SizeBytes = 20
	if _, _, _, err := s.CommitInbound(ctx, more); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("inbound crossing inbox cap: %v", err)
	}

	// The uncapped sibling accepts mail that the capped inbox rejected.
	free := inbound(other, "iq-4", "<iq-4@test>", "", nil, "Free", "x")
	free.SizeBytes = 100
	if _, _, _, err := s.CommitInbound(ctx, free); err != nil {
		t.Fatalf("uncapped inbox should accept: %v", err)
	}
}

// TestInboxQuotaRefundedOnPurge proves purging a message returns its bytes to
// the inbox counter so the cap can be reused.
func TestInboxQuotaRefundedOnPurge(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	quota := int64(50)
	setInboxQuota(t, s, u.AccountID, box.ID, &quota)

	rec := inbound(box, "rf-1", "<rf-1@test>", "", nil, "Keep", "x")
	rec.SizeBytes = 40
	m, _, _, err := s.CommitInbound(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Trash then purge, requiring Owner.
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, _, err := s.TrashMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	used, err := s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Fatalf("inbox used after purge = %d, want 0", used)
	}
	// The freed capacity accepts a fresh 40-byte message again.
	again := inbound(box, "rf-2", "<rf-2@test>", "", nil, "Again", "x")
	again.SizeBytes = 40
	if _, _, _, err := s.CommitInbound(ctx, again); err != nil {
		t.Fatalf("inbound after refund: %v", err)
	}
}

// TestInboxQuotaChargesDrafts proves a draft body counts against its inbox cap.
func TestInboxQuotaChargesDrafts(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	quota := int64(30)
	setInboxQuota(t, s, u.AccountID, box.ID, &quota)

	p := model.Principal{AccountID: u.AccountID, Admin: true}
	body := make([]byte, 30)
	for i := range body {
		body[i] = 'a'
	}
	if _, err := s.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, Subject: "d", Text: string(body)}); err != nil {
		t.Fatalf("draft within cap: %v", err)
	}
	used, err := s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 30 {
		t.Fatalf("inbox used with draft = %d, want 30", used)
	}
	// A further draft body would exceed the cap.
	if _, err := s.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, Subject: "d2", Text: "x"}); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("draft over inbox cap: %v", err)
	}
}

// TestInboxQuotaLazyInitFromSum proves an inbox row whose counter is NULL (as
// an upgraded install would have) is initialized from its existing stored bytes
// on the first adjustment, then enforced.
func TestInboxQuotaLazyInitFromSum(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	// Simulate a pre-migration inbox: write a message through the normal path,
	// then NULL the counter out from under it.
	rec := inbound(box, "lazy-1", "<lazy-1@test>", "", nil, "Legacy", "x")
	rec.SizeBytes = 40
	if _, _, _, err := s.CommitInbound(ctx, rec); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=NULL WHERE id=?`, box.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The display read falls back to the SUM without persisting.
	used, err := s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 40 {
		t.Fatalf("lazy display used = %d, want 40", used)
	}

	// Set a cap below existing usage plus a new message; the check must see the
	// SUM (40) and reject.
	quota := int64(50)
	setInboxQuota(t, s, u.AccountID, box.ID, &quota)
	more := inbound(box, "lazy-2", "<lazy-2@test>", "", nil, "More", "x")
	more.SizeBytes = 20
	if _, _, _, err := s.CommitInbound(ctx, more); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("lazy init should have rejected: %v", err)
	}
	// After a successful small write the counter is persisted and authoritative.
	tiny := inbound(box, "lazy-3", "<lazy-3@test>", "", nil, "Tiny", "x")
	tiny.SizeBytes = 5
	if _, _, _, err := s.CommitInbound(ctx, tiny); err != nil {
		t.Fatal(err)
	}
	used, err = s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 45 {
		t.Fatalf("inbox used after init+write = %d, want 45", used)
	}
}

// TestSetInboxStorageQuotaValidation proves negative caps are rejected and a
// nil cap clears the limit.
func TestSetInboxStorageQuotaValidation(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	negative := int64(-1)
	if err := s.SetInboxStorageQuota(ctx, u.AccountID, box.ID, &negative); err == nil {
		t.Fatal("negative quota must be rejected")
	}
	quota := int64(100)
	setInboxQuota(t, s, u.AccountID, box.ID, &quota)
	got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageQuotaBytes == nil || *got.StorageQuotaBytes != 100 {
		t.Fatalf("quota = %v, want 100", got.StorageQuotaBytes)
	}
	if err := s.SetInboxStorageQuota(ctx, u.AccountID, box.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageQuotaBytes != nil {
		t.Fatalf("quota after clear = %v, want nil", got.StorageQuotaBytes)
	}
}

// TestInboxQuotaMigrationFreshAndUpgrade proves migration 047 adds the nullable
// inbox storage columns on both a fresh database and an upgrade from the v012
// fixture, and that the upgraded counter starts NULL so lazy init can run.
func TestInboxQuotaMigrationFreshAndUpgrade(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		st, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if n := markerCount(t, st, "047"); n != 1 {
			t.Fatalf("migration 047 marker count %d", n)
		}
		if !columnPresent(t, st, "inboxes", "storage_quota_bytes") || !columnPresent(t, st, "inboxes", "storage_used_bytes") {
			t.Fatal("inbox quota columns missing after fresh migrate")
		}
	})
	t.Run("upgrade-v012", func(t *testing.T) {
		dir := newV012(t)
		seedV012(t, dir)
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if n := markerCount(t, st, "047"); n != 1 {
			t.Fatalf("migration 047 marker count %d", n)
		}
		db := rawDB(t, st.Path())
		defer db.Close()
		var used sql.NullInt64
		if err := db.QueryRowContext(context.Background(), `SELECT storage_used_bytes FROM inboxes WHERE id='inbA'`).Scan(&used); err != nil {
			t.Fatal(err)
		}
		if used.Valid {
			t.Fatalf("upgraded inbox counter should be NULL, got %d", used.Int64)
		}
	})
}

// TestInboxQuotaPurgeFromUninitializedCounter proves purging a message from an
// inbox whose counter is still NULL (a pre-migration inbox) initializes it
// first, so the refund does not clobber the remaining usage to zero.
func TestInboxQuotaPurgeFromUninitializedCounter(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	// Two messages, then NULL the counter to simulate a pre-migration inbox.
	for i, d := range []string{"u1", "u2"} {
		rec := inbound(box, d, "<"+d+"@test>", "", nil, "S", "x")
		rec.SizeBytes = 30
		_ = i
		if _, _, _, err := s.CommitInbound(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=NULL WHERE id=?`, box.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	msgs, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if _, _, err := s.TrashMessage(ctx, p, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, p, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	used, err := s.InboxStorageUsed(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 30 {
		t.Fatalf("inbox used after purge from NULL counter = %d, want 30", used)
	}
}
