package dbschema_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/dbschema"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestSentPlacementBackfillFilesStandaloneInbox proves the Sent-placement
// backfill covers connected (standalone) inboxes as well as domain inboxes, and
// that re-running it on a database that already carries its marker is the
// supported way to harden an existing deployment: the backfill changes data
// only, so the pinned schema hash never moves.
func TestSentPlacementBackfillFilesStandaloneInbox(t *testing.T) {
	t.Parallel()
	dir, db := fresh(t)
	exec(t, db, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('a','Developer',1000000,?)`, stamp)
	exec(t, db, `INSERT INTO domains(id,account_id,name,created_at) VALUES('d','a','example.test',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,domain_id,local_part,kind,created_at) VALUES('s','a','d','agent@remote.test','standalone',?)`, stamp)
	exec(t, db, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,role,is_system,origin,created_at,updated_at) VALUES('fld_sent','a','s','Sent','Sent','sent',1,'local',?,?)`, stamp, stamp)
	exec(t, db, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES('t1','a','s','Hi',?,?)`, stamp, stamp)
	exec(t, db, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,status,from_address,to_json,envelope_to_json,received_at,created_at) VALUES('m1','a','s','t1','outbound','sent','agent@remote.test','[]','[]',?,?)`, stamp, stamp)
	db.Close()

	// Drop the data-only marker and start once: the hardened statement re-runs.
	reopen := openDB(t, dir)
	exec(t, reopen, `DELETE FROM schema_migrations WHERE version IN ('003','004')`)
	reopen.Close()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	db = openDB(t, dir)
	defer db.Close()
	if got := scalar(t, db, `SELECT count(*) FROM messages WHERE id='m1' AND mailbox_id='fld_sent'`); got != 1 {
		t.Fatal("a standalone inbox's completed send was not filed in its Sent folder")
	}
	if got := scalar(t, db, `SELECT count(*) FROM schema_migrations`); got != 4 {
		t.Fatalf("migration markers = %d, want the full public lineage", got)
	}
	if err := dbschema.Validate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
