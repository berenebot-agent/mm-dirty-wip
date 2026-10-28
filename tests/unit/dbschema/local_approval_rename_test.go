package dbschema_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/dbschema"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestLocalApprovalRenameRewritesStoredMode proves migration 004 moves persisted
// approval modes from the old name to local_approval on both the inbox setting
// and the handling record, leaves remote_draft untouched, and changes data only.
func TestLocalApprovalRenameRewritesStoredMode(t *testing.T) {
	t.Parallel()
	dir, db := fresh(t)
	exec(t, db, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('a','Developer',1000000,?)`, stamp)
	exec(t, db, `INSERT INTO domains(id,account_id,name,created_at) VALUES('d','a','example.test',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,domain_id,local_part,kind,authoring_mode,created_at) VALUES('dom','a','d','hermes@example.test','domain','mailmoose_approval',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,domain_id,local_part,kind,authoring_mode,created_at) VALUES('sa','a','d','agent@remote.test','standalone','remote_draft',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,domain_id,local_part,kind,authoring_mode,created_at) VALUES('sa2','a','d','ops@remote.test','standalone','mailmoose_approval',?)`, stamp)
	exec(t, db, `INSERT INTO assistant_handling_requests(id,account_id,inbox_id,draft_id,mode,requested_at,created_at,updated_at) VALUES('h1','a','sa2','dr1','mailmoose_approval',?,?,?)`, stamp, stamp, stamp)
	db.Close()

	// Drop the approval-mode marker and start once: the rewrite statement re-runs.
	reopen := openDB(t, dir)
	exec(t, reopen, `DELETE FROM schema_migrations WHERE version='004'`)
	reopen.Close()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	db = openDB(t, dir)
	defer db.Close()
	if got := scalar(t, db, `SELECT count(*) FROM inboxes WHERE id='dom' AND authoring_mode='local_approval'`); got != 1 {
		t.Fatal("domain inbox approval mode was not renamed")
	}
	if got := scalar(t, db, `SELECT count(*) FROM inboxes WHERE id='sa2' AND authoring_mode='local_approval'`); got != 1 {
		t.Fatal("standalone inbox approval mode was not renamed")
	}
	if got := scalar(t, db, `SELECT count(*) FROM inboxes WHERE id='sa' AND authoring_mode='remote_draft'`); got != 1 {
		t.Fatal("remote_draft inbox was changed by the rename")
	}
	if got := scalar(t, db, `SELECT count(*) FROM assistant_handling_requests WHERE id='h1' AND mode='local_approval'`); got != 1 {
		t.Fatal("handling record mode was not renamed")
	}
	if got := scalar(t, db, `SELECT count(*) FROM inboxes WHERE authoring_mode='mailmoose_approval'`); got != 0 {
		t.Fatal("old approval mode remains on an inbox")
	}
	if got := scalar(t, db, `SELECT count(*) FROM schema_migrations`); got != 4 {
		t.Fatalf("migration markers = %d, want the full public lineage", got)
	}
	if err := dbschema.Validate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
