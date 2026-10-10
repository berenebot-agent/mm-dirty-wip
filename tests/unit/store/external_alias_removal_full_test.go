package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// reAddPre053ExternalAliasSchema restores the external-alias table and columns
// that migration 053 removes, so a test can seed alias-specific data on a real
// database and then let store.Open run 053 against it again. It also clears the
// 053 marker so the runner re-applies the migration.
func reAddPre053ExternalAliasSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE external_aliases (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			inbox_id TEXT NOT NULL,
			address TEXT NOT NULL COLLATE NOCASE,
			display_name TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			encrypted_config TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(inbox_id,address)
		)`,
		`ALTER TABLE messages ADD COLUMN sending_external_alias_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX idx_messages_external_alias ON messages(sending_external_alias_id)`,
		`ALTER TABLE drafts ADD COLUMN from_external_alias_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE outbound_delivery_log ADD COLUMN external_alias_id TEXT NOT NULL DEFAULT ''`,
		`DELETE FROM schema_migrations WHERE version='053'`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("re-add pre-053 schema %q: %v", q, err)
		}
	}
}

// TestMigration053RetiresExternalAliasData builds a real pre-053 database shape
// (external-alias table and columns restored on a migrated database), seeds
// alias-specific data alongside ordinary mail and sent history, then re-runs
// migration 053 and asserts:
//
//   - unsent alias drafts/attachments, their send request and approval workflow,
//     and queued alias messages (with attachments, FTS, events, idempotency) are
//     discarded;
//   - ordinary mail, managed aliases and SENT alias history survive, with the
//     sent message keeping its from_address attribution and its delivery log;
//   - account AND inbox storage counters are refunded exactly (the inbox counter
//     is initialized even though it started NULL);
//   - the orphaned thread is removed;
//   - the raw files are queued for post-commit cleanup and then unlinked;
//   - the schema is clean and PRAGMA foreign_key_check passes.
func TestMigration053RetiresExternalAliasData(t *testing.T) {
	dir := t.TempDir()
	// 1. Create a migrated database and a real account/domain/inbox + files.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	db := rawDB(t, filepath.Join(dir, "inbox.db"))
	defer db.Close()
	ctx := context.Background()
	now := "2026-01-01T00:00:00Z"
	// Restore the pre-053 external-alias shape and clear the 053 marker BEFORE
	// seeding, so the alias columns/table exist for the seed rows.
	reAddPre053ExternalAliasSchema(t, db)
	mkDir := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		return rel
	}
	queuedRaw := mkDir("messages/aa/bb/queued.eml")
	sentRaw := mkDir("messages/cc/dd/sent.eml")
	draftAtt := mkDir("drafts/ee/ff/att.bin")
	ordinaryRaw := mkDir("messages/11/22/ordinary.eml")

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO accounts(id,name,storage_quota_bytes,storage_used_bytes,created_at) VALUES('acc1','A',0,1000,?)`, now)
	mustExec(`INSERT INTO domains(id,account_id,name,created_at) VALUES('dom1','acc1','example.com',?)`, now)
	mustExec(`INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,enabled,allowed_senders_json,created_at,storage_used_bytes) VALUES('inb1','acc1','dom1','box','Box',1,'[]',?,NULL)`, now)
	mustExec(`INSERT INTO inbox_aliases(id,account_id,domain_id,inbox_id,local_part,display_name,created_at) VALUES('al1','acc1','dom1','inb1','sales','Sales',?)`, now)
	mustExec(`INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES
		('thrOrd','acc1','inb1','ordinary',?,?),
		('thrSent','acc1','inb1','sent',?,?),
		('thrAliasOnly','acc1','inb1','alias-only',?,?)`, now, now, now, now, now, now)
	// A queued alias message (never sent) with an attachment, event and idempotency.
	mustExec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,from_name,from_address,to_json,subject,text_body,raw_path,size_bytes,is_read,status,created_at,sending_external_alias_id) VALUES('msgQueued','acc1','inb1','thrAliasOnly','outbound','smtp','','<q@x>','Q','agent@ext.example','[]','queued','',?,100,1,'pending',?,'ea1')`, queuedRaw, now)
	mustExec(`INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,part_index) VALUES('attQ','msgQueued','f.bin','application/octet-stream',50,0)`)
	mustExec(`INSERT INTO events(account_id,inbox_id,type,entity_id,payload_json,created_at) VALUES('acc1','inb1','message.queued','msgQueued','{}',?)`, now)
	mustExec(`INSERT INTO outbound_idempotency(account_id,idem_key,message_id,inbox_id,result_json,status,created_at) VALUES('acc1','idem1','msgQueued','inb1','{}','done',?)`, now)
	// A SENT alias message (history) with a delivery-log attempt.
	mustExec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,from_name,from_address,to_json,subject,text_body,raw_path,size_bytes,is_read,status,created_at,sending_external_alias_id) VALUES('msgSent','acc1','inb1','thrSent','outbound','smtp','pm','<s@x>','Agent','agent@ext.example','["bob@x"]','sent','',?,200,1,'sent',?,'ea1')`, sentRaw, now)
	mustExec(`INSERT INTO outbound_delivery_log(account_id,provider,message_id,attempt,status,created_at,external_alias_id,from_address,to_json,subject) VALUES('acc1','smtp','msgSent',1,'sent',?,'ea1','agent@ext.example','["bob@x"]','sent')`, now)
	// An ordinary message (kept).
	mustExec(`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,from_name,from_address,to_json,subject,text_body,raw_path,size_bytes,is_read,status,created_at) VALUES('msgOrd','acc1','inb1','thrOrd','inbound','mailgun','pm2','<o@x>','Al','al@x','[]','ordinary','',?,10,1,'sent',?)`, ordinaryRaw, now)
	// An unsent alias draft with an attachment, a send request and a workflow job.
	mustExec(`INSERT INTO drafts(id,account_id,inbox_id,from_address,from_name,to_json,subject,text_body,html_body,status,created_at,updated_at,from_external_alias_id) VALUES('drf1','acc1','inb1','agent@ext.example','Agent','[]','draft','body','', 'draft',?,?,'ea1')`, now, now)
	mustExec(`INSERT INTO draft_attachments(id,draft_id,filename,content_type,size_bytes,raw_path,created_at) VALUES('datt1','drf1','a.bin','application/octet-stream',30,?,?)`, draftAtt, now)
	mustExec(`INSERT INTO outbound_workflow(id,account_id,inbox_id,request_id,kind,status,created_at) VALUES('wf1','acc1','inb1','','approval_request','pending',?)`, now)
	mustExec(`INSERT INTO draft_send_requests(id,account_id,inbox_id,draft_id,status,delivery_status,requested_at,created_at,updated_at,approval_workflow_id) VALUES('dsr1','acc1','inb1','drf1','pending','none',?,?,?,'wf1')`, now, now, now)
	mustExec(`INSERT INTO external_aliases(id,account_id,inbox_id,address,display_name,provider,created_at,updated_at) VALUES('ea1','acc1','inb1','agent@ext.example','Agent','smtp',?,?)`, now, now)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. Re-open so the runner re-applies 053 against the seeded pre-053 data.
	st, err = store.Open(dir)
	if err != nil {
		t.Fatalf("re-open for migration 053: %v", err)
	}
	defer st.Close()

	// 3. Assert the retired data is gone and the preserved data remains.
	db = rawDB(t, st.Path())
	defer db.Close()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='external_aliases'`); n != 0 {
		t.Errorf("external_aliases not dropped")
	}
	for _, id := range []string{"msgQueued", "drf1", "datt1", "dsr1", "wf1"} {
		if n := count(`SELECT count(*) FROM (SELECT id FROM messages UNION ALL SELECT id FROM drafts UNION ALL SELECT id FROM draft_attachments UNION ALL SELECT id FROM draft_send_requests UNION ALL SELECT id FROM outbound_workflow) WHERE id=?`, id); n != 0 {
			t.Errorf("discarded row %s still present", id)
		}
	}
	if n := count(`SELECT count(*) FROM messages WHERE id='msgSent'`); n != 1 {
		t.Errorf("sent alias message was discarded")
	}
	if n := count(`SELECT count(*) FROM outbound_delivery_log WHERE message_id='msgSent'`); n != 1 {
		t.Errorf("sent alias delivery-log history was discarded")
	}
	if n := count(`SELECT count(*) FROM messages WHERE id='msgOrd'`); n != 1 {
		t.Errorf("ordinary message was discarded")
	}
	if n := count(`SELECT count(*) FROM inbox_aliases WHERE id='al1'`); n != 1 {
		t.Errorf("managed alias was discarded")
	}
	if n := count(`SELECT count(*) FROM threads WHERE id='thrAliasOnly'`); n != 0 {
		t.Errorf("orphaned alias-only thread was not removed")
	}
	if n := count(`SELECT count(*) FROM threads WHERE id='thrOrd'`); n != 1 {
		t.Errorf("ordinary thread was removed")
	}

	// Storage: refunds = queued msg 100 + draft body 4 + draft att 30 = 134.
	var acctUsed int64
	if err := db.QueryRowContext(ctx, `SELECT storage_used_bytes FROM accounts WHERE id='acc1'`).Scan(&acctUsed); err != nil {
		t.Fatal(err)
	}
	if want := int64(1000 - 134); acctUsed != want {
		t.Errorf("account storage = %d, want %d", acctUsed, want)
	}
	// The inbox counter started NULL; it is initialized from the live SUM
	// (queued 100 + sent 200 + ordinary 10 + draft body 4 + draft att 30 = 344)
	// and then refunded the discarded 134, leaving the surviving 210.
	var inboxUsed int64
	if err := db.QueryRowContext(ctx, `SELECT storage_used_bytes FROM inboxes WHERE id='inb1'`).Scan(&inboxUsed); err != nil {
		t.Fatal(err)
	}
	if want := int64(210); inboxUsed != want {
		t.Errorf("inbox storage = %d, want %d (initialized NULL counter refunded)", inboxUsed, want)
	}

	// Files retired post-commit; the queue is drained.
	for _, rel := range []string{queuedRaw, sentRaw, draftAtt, ordinaryRaw} {
		_, serr := os.Stat(filepath.Join(dir, rel))
		switch rel {
		case queuedRaw, draftAtt:
			if serr == nil {
				t.Errorf("discarded file %s was not unlinked", rel)
			}
		default:
			if serr != nil {
				t.Errorf("preserved file %s was removed", rel)
			}
		}
	}
	if n := count(`SELECT count(*) FROM pending_file_cleanup`); n != 0 {
		t.Errorf("pending_file_cleanup not drained: %d", n)
	}

	// Schema clean and foreign keys intact.
	if n := count(`SELECT count(*) FROM pragma_table_info('messages') WHERE name='sending_external_alias_id'`); n != 0 {
		t.Errorf("messages.sending_external_alias_id not dropped")
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Errorf("foreign_key_check reported a violation after migration 053")
	}
}
