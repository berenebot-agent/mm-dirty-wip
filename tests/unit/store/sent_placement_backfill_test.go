package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestUpgradeBackfillsUnfiledSentPlacements proves the Sent-placement
// migration files completed outbound mail that predates explicit filing in
// the Sent folder the reads already widen to, in the pre-upgrade shape:
// mailbox_id NULL while the Sent folder exists. The migration is replayed by
// deleting its marker, which is exactly what a database upgraded from the
// pre-003 lineage experiences at first open. A standalone inbox's placement
// is the provider's cached folder, not a managed tree, so its row keeps NULL.
//
// This test must not use the shared migrated template (testdb): the sequence
// under test is literally a migration, so it opens real empty directories
// through store.Open.
func TestUpgradeBackfillsUnfiledSentPlacements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateAccountAndAdmin(ctx, "Upgrade", "admin@example.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(ctx, u.AccountID, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateInbox(ctx, u.AccountID, d.ID, "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "agent@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	send := func(inbox model.Inbox, rfc string) model.Message {
		t.Helper()
		m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{
			Inbox: inbox, Provider: "smtp", RFCMessageID: rfc,
			From: model.Address{Address: inbox.Address}, To: []string{"recipient@example.test"},
			Subject: "Backfill " + rfc, Text: "body", RawPath: "messages/backfill.eml", SizeBytes: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.MarkSent(ctx, u.AccountID, m.ID, "<wire-"+rfc+">", "smtp", ""); err != nil {
			t.Fatal(err)
		}
		filed, err := s.GetMessage(ctx, p, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		return filed
	}
	domainSend := send(box, "<backfill-domain@test>")
	if domainSend.MailboxID == "" {
		t.Fatalf("send path did not file the send in a folder: %+v", domainSend)
	}
	standaloneSend := send(standalone, "<backfill-standalone@test>")
	if standaloneSend.MailboxID != "" {
		t.Fatalf("standalone send unexpectedly filed in %q", standaloneSend.MailboxID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reshape both completed sends to the pre-upgrade shape (no stored
	// placement) and drop the migration marker, so the reopen replays the
	// upgrade exactly as a pre-003 production database would.
	db, err := sql.Open("sqlite3", filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	stmt := `UPDATE messages SET mailbox_id=NULL WHERE id=?`
	if _, err := db.ExecContext(ctx, stmt, domainSend.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, stmt, standaloneSend.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN ('003','004')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	backfilled, err := s2.GetMessage(ctx, p, domainSend.ID)
	if err != nil {
		t.Fatal(err)
	}
	if backfilled.MailboxID != domainSend.MailboxID {
		t.Fatalf("backfill filed %q, want the original Sent folder %q", backfilled.MailboxID, domainSend.MailboxID)
	}
	untouched, err := s2.GetMessage(ctx, p, standaloneSend.ID)
	if err != nil || untouched.MailboxID != "" {
		t.Fatalf("standalone placement = %q (err %v), want untouched NULL", untouched.MailboxID, err)
	}
}
