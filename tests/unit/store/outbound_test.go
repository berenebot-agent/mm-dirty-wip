package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
)

func TestDomainOutboundCredentialResolution(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)

	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}

	a, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "A", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	// Saving a credential must not assign it to any domain.
	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("saving a credential must not auto-assign: %v", err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); err != nil || got.ID != a.ID {
		t.Fatalf("domain credential: %v %q", err, got.ID)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != a.ID {
		t.Fatalf("get domain: %v %+v", err, dom)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, "out_missing"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown credential should be rejected, got %v", err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("cleared domain should have no provider: %v", err)
	}

	// Deleting a credential clears the domain assignment via the foreign key.
	b, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "B", "smtp", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if dom, err = s.GetDomain(ctx, u.AccountID, d.ID); err != nil || dom.OutboundCredentialID != "" {
		t.Fatalf("domain credential should clear on delete: %v %+v", err, dom)
	}

	// Resolution via an existing message uses the message's domain.
	c, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "C", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	msg, _, err := s.CommitOutbound(ctx, OutboundRecord{Inbox: boxes[0], Provider: c.Provider, RFCMessageID: "<m@test>", From: model.Address{Address: boxes[0].Address}, To: []string{"x@outside.test"}, Subject: "hi", Text: "body", RawPath: "messages/x.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OutboundCredentialForMessage(ctx, u.AccountID, msg.ID); err != nil || got.ID != c.ID {
		t.Fatalf("for message: %v %q", err, got.ID)
	}

	// HoldPending defers without counting an attempt.
	if err = s.HoldPending(ctx, u.AccountID, msg.ID, "no outbound provider configured for this domain", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	held, err := s.GetMessageByID(ctx, u.AccountID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "pending" || held.Attempts != 0 || held.LastError == "" {
		t.Fatalf("held message: status=%q attempts=%d err=%q", held.Status, held.Attempts, held.LastError)
	}
	if err = s.HoldPending(ctx, u.AccountID, "msg_missing", "x", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hold unknown: %v", err)
	}
}

// TestMigration008RebuildsAccounts simulates a pre-008 database and verifies the
// table rebuild drops the account default column while preserving rows and
// foreign keys.
func TestMigration008RebuildsAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.db")
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	now := nowText()
	for _, m := range []struct{ version, sql string }{
		{"001", migration001},
		{"002", migration002},
		{"003", migration003},
		{"004", migration004},
		{"005", migration005},
		{"006", migration006},
		{"007", migration007},
	} {
		if _, err = db.Exec(m.sql); err != nil {
			t.Fatalf("apply %s: %v", m.version, err)
		}
		if _, err = db.Exec(`INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(?,?)`, m.version, now); err != nil {
			t.Fatalf("record %s: %v", m.version, err)
		}
	}
	seed := []string{
		`INSERT INTO accounts(id,name,storage_quota_bytes,storage_used_bytes,created_at) VALUES('acc_1','A',1000,10,'` + now + `')`,
		`INSERT INTO outbound_credentials(id,account_id,name,provider,encrypted_config,created_at,updated_at) VALUES('out_1','acc_1','Primary','brevo','enc','` + now + `','` + now + `')`,
		`UPDATE accounts SET active_outbound_credential_id='out_1' WHERE id='acc_1'`,
		`INSERT INTO domains(id,account_id,name,created_at) VALUES('dom_1','acc_1','example.com','` + now + `')`,
		`INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,enabled,created_at) VALUES('inb_1','acc_1','dom_1','hermes','Hermes',1,'` + now + `')`,
	}
	for _, q := range seed {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open after upgrade: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	var cols int
	if err = s.read.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('accounts') WHERE name='active_outbound_credential_id'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatalf("active_outbound_credential_id column should be dropped")
	}
	acc, err := s.GetAccount(ctx, "acc_1")
	if err != nil || acc.Name != "A" || acc.StorageUsedBytes != 10 {
		t.Fatalf("account lost in rebuild: %v %+v", err, acc)
	}
	dom, err := s.GetDomain(ctx, "acc_1", "dom_1")
	if err != nil || dom.Name != "example.com" {
		t.Fatalf("domain lost in rebuild: %v %+v", err, dom)
	}
	if _, err = s.GetInboxInternal(ctx, "acc_1", "inb_1"); err != nil {
		t.Fatalf("inbox lost in rebuild: %v", err)
	}
	// Foreign keys must still be enforced after the rebuild.
	if err = s.SetDomainOutboundCredential(ctx, "acc_1", "dom_1", "out_missing"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign key validation broken after rebuild: %v", err)
	}
}

func TestMigrationReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}
