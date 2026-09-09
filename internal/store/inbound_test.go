package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestInboundCredentialCRUDAndBinding(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	if _, err := s.ResolveInboundBinding(ctx, "mailgun", "x@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unconfigured resolve err=%v", err)
	}
	cred, err := s.SaveInboundCredential(ctx, u.AccountID, "", "MG", "mailgun", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetInboundCredential(ctx, u.AccountID, cred.ID); err != nil || got.Name != "MG" {
		t.Fatalf("get %+v err=%v", got, err)
	}
	if list, err := s.ListInboundCredentials(ctx, u.AccountID); err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.ResolveInboundBinding(ctx, "mailgun", "Hermes@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != u.AccountID || b.DomainID != d.ID || b.CredentialID != cred.ID || b.Recipient != "hermes@example.com" {
		t.Fatalf("binding %+v", b)
	}
	if _, err = s.ResolveInboundBinding(ctx, "cloudflare", "hermes@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong provider err=%v", err)
	}

	// Cross-account assignment is forbidden.
	u2, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDomain(ctx, u2.AccountID, "b.test")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainInboundCredential(ctx, u2.AccountID, d2.ID, cred.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-account assign err=%v", err)
	}

	// Clearing leaves the domain unconfigured.
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveInboundBinding(ctx, "mailgun", "hermes@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared resolve err=%v", err)
	}
	// Deleting a credential leaves assigned domains unconfigured.
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteInboundCredential(ctx, u.AccountID, cred.ID); err != nil {
		t.Fatal(err)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.InboundCredentialID != "" {
		t.Fatalf("domain after delete %+v err=%v", dom, err)
	}
}

func TestInboundDedupScopedByRecipient(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)
	r1 := inbound(b[0], "same-delivery", "<one@test>", "", nil, "s", "body")
	r1.EnvelopeRecipient = b[0].Address
	r2 := inbound(b[1], "same-delivery", "<two@test>", "", nil, "s", "body")
	r2.EnvelopeRecipient = b[1].Address

	m1, _, dup, err := s.CommitInbound(ctx, r1)
	if err != nil || dup {
		t.Fatalf("first %v dup=%v", err, dup)
	}
	m2, _, dup, err := s.CommitInbound(ctx, r2)
	if err != nil || dup {
		t.Fatalf("same delivery different recipient must not dedup: %v dup=%v", err, dup)
	}
	if m1.ID == m2.ID {
		t.Fatal("distinct deliveries collapsed")
	}
	m3, _, dup, err := s.CommitInbound(ctx, r1)
	if err != nil || !dup || m3.ID != m1.ID {
		t.Fatalf("same recipient retry must dedup: %v dup=%v", err, dup)
	}

	// Blocked messages use the same scoped identity.
	bm1, dup, err := s.CommitBlockedInbound(ctx, BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[0].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[0].Address})
	if err != nil || dup {
		t.Fatalf("blocked first %v dup=%v", err, dup)
	}
	_, dup, err = s.CommitBlockedInbound(ctx, BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[1].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[1].Address})
	if err != nil || dup {
		t.Fatalf("blocked different recipient %v dup=%v", err, dup)
	}
	bm2, dup, err := s.CommitBlockedInbound(ctx, BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[0].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[0].Address})
	if err != nil || !dup || bm2.ID != bm1.ID {
		t.Fatalf("blocked retry %v dup=%v", err, dup)
	}
}

// TestMigration009BackfillAndRebuild exercises the legacy-data path: it builds a
// database at schema 008, inserts a message that recorded envelope_to_json but
// no envelope_recipient, then applies migration009 and checks the backfill and
// the new scoped uniqueness.
func TestMigration009BackfillAndRebuild(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "legacy.db") + "?_foreign_keys=on&_journal_mode=WAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, m := range []string{migration001, migration002, migration003, migration004, migration005, migration006, migration007, migration008} {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("apply legacy migration: %v", err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('acc','A',1000,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO domains(id,account_id,name,created_at) VALUES('dom','acc','example.com','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO inboxes(id,account_id,domain_id,local_part,created_at) VALUES('box','acc','dom','hermes','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES('thr','acc','box','s','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,envelope_to_json,created_at) VALUES('msg','acc','box','thr','inbound','mailgun','legacy-1','["Hermes@Example.com"]','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO blocked_messages(id,account_id,inbox_id,provider,provider_delivery_id,to_json,created_at) VALUES('blk','acc','box','mailgun','legacy-2','["x@example.com"]','2026-01-01T00:00:00Z')`)

	if _, err := db.Exec(migration009); err != nil {
		t.Fatalf("migration009: %v", err)
	}
	var recipient string
	if err := db.QueryRow(`SELECT envelope_recipient FROM messages WHERE id='msg'`).Scan(&recipient); err != nil {
		t.Fatal(err)
	}
	if recipient != "Hermes@Example.com" {
		t.Fatalf("backfilled recipient %q", recipient)
	}
	var blockedRecipient string
	if err := db.QueryRow(`SELECT envelope_recipient FROM blocked_messages WHERE id='blk'`).Scan(&blockedRecipient); err != nil {
		t.Fatal(err)
	}
	if blockedRecipient != "" {
		t.Fatalf("legacy blocked recipient should stay empty, got %q", blockedRecipient)
	}
	// New scoped uniqueness accepts the same delivery for a different recipient
	// but rejects a repeat for the same one.
	exec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,envelope_recipient,created_at) VALUES('msg2','acc','box','thr','inbound','mailgun','legacy-1','other@example.com','2026-01-01T00:00:00Z')`)
	if _, err := db.Exec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,envelope_recipient,created_at) VALUES('msg3','acc','box','thr','inbound','mailgun','legacy-1','Hermes@Example.com','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("duplicate scoped delivery accepted")
	}
	// The rebuild must leave foreign keys intact.
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violations after migration009")
	}
}
