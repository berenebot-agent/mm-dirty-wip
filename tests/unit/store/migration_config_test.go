package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gatehouse-mail/internal/store"
)

// newV012 creates a temporary data directory whose inbox.db is the full
// migration-012 schema snapshot, with foreign keys left at the sqlite default.
func newV012(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sqlBytes, err := os.ReadFile(filepath.Join("testdata", "schema_v012.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), string(sqlBytes)); err != nil {
		db.Close()
		t.Fatalf("apply v012 fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// seedV012 inserts an account with two domains sharing one outbound credential
// (and one inbound credential), an unassigned credential of each kind, an inbox
// with an allowed-senders list, a message, and two delivery-log attempts (one
// linked to the message, one orphaned).
func seedV012(t *testing.T, dir string) {
	t.Helper()
	db := rawDB(t, filepath.Join(dir, "inbox.db"))
	defer db.Close()
	stmts := []string{
		`INSERT INTO accounts(id,name,storage_quota_bytes,storage_used_bytes,created_at) VALUES('acc1','A',100000,42,'2026-01-01T00:00:00Z')`,
		`INSERT INTO outbound_credentials(id,account_id,name,provider,encrypted_config,created_at,updated_at) VALUES
			('outX','acc1','X','brevo','ENC-SHARED','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z'),
			('outUnused','acc1','U','smtp','ENC-UNUSED','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO inbound_credentials(id,account_id,provider,name,encrypted_config,created_at,updated_at) VALUES
			('inP','acc1','resend','P','ENC-IN','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z'),
			('inUnused','acc1','mailgun','U','ENC-UNUSED-IN','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO domains(id,account_id,name,created_at,outbound_credential_id,inbound_credential_id) VALUES
			('domA','acc1','a.test','2026-01-01T00:00:00Z','outX','inP'),
			('domB','acc1','b.test','2026-01-01T00:00:00Z','outX','inP'),
			('domC','acc1','c.test','2026-01-01T00:00:00Z',NULL,NULL)`,
		`INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,enabled,allowed_senders_json,created_at,outbound_credential_id) VALUES
			('inbA','acc1','domA','box','Box',1,'["*@ok.test"]','2026-01-01T00:00:00Z','outX')`,
		`INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES('thrA','acc1','inbA','s','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,subject,text_body,created_at,status) VALUES
			('msgA','acc1','inbA','thrA','outbound','brevo','','s','body','2026-01-01T00:00:00Z','sent')`,
		`INSERT INTO outbound_delivery_log(account_id,credential_id,provider,message_id,attempt,status,created_at) VALUES
			('acc1','outX','brevo','msgA',1,'sent','2026-01-01T00:00:00Z'),
			('acc1','outX','brevo',NULL,2,'failed','2026-01-01T00:00:00Z')`,
		`INSERT INTO message_fts(message_id,account_id,inbox_id,subject,from_address,recipients,body,attachment_names) VALUES('msgA','acc1','inbA','s','a@b.test','x@y.test','preserved search token','')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func tablePresent(t *testing.T, st *store.Store, name string) bool {
	t.Helper()
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func columnPresent(t *testing.T, st *store.Store, table, column string) bool {
	t.Helper()
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func queryInt(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigration013UpgradeSharedAndUnassignedConfigs(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer st.Close()

	// The retired tables are gone; the new per-domain tables exist.
	for _, gone := range []string{"outbound_credentials", "inbound_credentials"} {
		if tablePresent(t, st, gone) {
			t.Fatalf("retired table %s survived 013", gone)
		}
	}
	for _, want := range []string{"domain_sending_configs", "domain_receiving_configs"} {
		if !tablePresent(t, st, want) {
			t.Fatalf("new table %s missing after 013", want)
		}
	}
	for _, col := range [][2]string{{"domains", "outbound_credential_id"}, {"domains", "inbound_credential_id"}, {"inboxes", "outbound_credential_id"}, {"outbound_delivery_log", "credential_id"}} {
		if columnPresent(t, st, col[0], col[1]) {
			t.Fatalf("column %s.%s survived 013", col[0], col[1])
		}
	}
	if !columnPresent(t, st, "outbound_delivery_log", "domain_id") {
		t.Fatal("outbound_delivery_log.domain_id missing after 013")
	}

	// domA and domB shared one outbound credential and one inbound credential.
	// Each must have its own independent copy with the same encrypted bytes.
	if n := queryInt(t, st, `SELECT count(*) FROM domain_sending_configs WHERE account_id='acc1'`); n != 2 {
		t.Fatalf("sending config count = %d, want 2", n)
	}
	if n := queryInt(t, st, `SELECT count(DISTINCT id) FROM domain_sending_configs`); n != 2 {
		t.Fatalf("sending config ids not independent: %d", n)
	}
	if n := queryInt(t, st, `SELECT count(*) FROM domain_sending_configs WHERE encrypted_config='ENC-SHARED' AND provider='brevo'`); n != 2 {
		t.Fatalf("shared sending bytes not copied: %d", n)
	}
	if n := queryInt(t, st, `SELECT count(*) FROM domain_sending_configs WHERE domain_id='domC'`); n != 0 {
		t.Fatal("unassigned domain gained a sending config")
	}
	if n := queryInt(t, st, `SELECT count(*) FROM domain_receiving_configs WHERE account_id='acc1'`); n != 2 {
		t.Fatalf("receiving config count = %d, want 2", n)
	}
	if n := queryInt(t, st, `SELECT count(*) FROM domain_receiving_configs WHERE encrypted_config='ENC-IN' AND provider='resend'`); n != 2 {
		t.Fatalf("shared receiving bytes not copied: %d", n)
	}

	// Every attempt is kept. The message-linked row is attributed to the
	// message's domain; the orphaned row keeps a NULL domain.
	if n := queryInt(t, st, `SELECT count(*) FROM outbound_delivery_log WHERE account_id='acc1'`); n != 2 {
		t.Fatalf("delivery log rows = %d, want 2", n)
	}
	if n := queryInt(t, st, `SELECT count(*) FROM outbound_delivery_log WHERE message_id='msgA' AND domain_id='domA'`); n != 1 {
		t.Fatal("message attempt was not attributed to its domain")
	}
	if n := queryInt(t, st, `SELECT count(*) FROM outbound_delivery_log WHERE message_id IS NULL AND domain_id IS NULL`); n != 1 {
		t.Fatal("orphaned attempt lost or incorrectly attributed")
	}

	// Inbox data, including the allowlist, is preserved.
	if n := queryInt(t, st, `SELECT count(*) FROM inboxes WHERE id='inbA' AND allowed_senders_json='["*@ok.test"]'`); n != 1 {
		t.Fatal("inbox allowed_senders_json not preserved")
	}
	if !columnPresent(t, st, "inboxes", "allowed_senders_json") {
		t.Fatal("allowed_senders_json column missing")
	}

	// Storage accounting and the FTS index are untouched by 013.
	if n := queryInt(t, st, `SELECT storage_used_bytes FROM accounts WHERE id='acc1'`); n != 42 {
		t.Fatalf("storage_used_bytes = %d, want 42", n)
	}
	if n := queryInt(t, st, `SELECT count(*) FROM message_fts WHERE message_fts MATCH 'preserved'`); n != 1 {
		t.Fatal("FTS index not preserved across 013")
	}

	// The message dedup identity survives: a retried inbound delivery still
	// collapses onto the already-stored message.
	ctx := context.Background()
	inbox, err := st.GetInboxInternal(ctx, "acc1", "inbA")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.InboundRecord{Inbox: inbox, Provider: "mailgun", ProviderDeliveryID: "retry-1", EnvelopeRecipient: inbox.Address, Subject: "s", Text: "b", RawPath: "messages/r.eml", SizeBytes: 1}
	m1, _, dup, err := st.CommitInbound(ctx, rec)
	if err != nil || dup {
		t.Fatalf("post-upgrade inbound %v dup=%v", err, dup)
	}
	m2, _, dup, err := st.CommitInbound(ctx, rec)
	if err != nil || !dup || m2.ID != m1.ID {
		t.Fatalf("dedup not preserved after 013: %v dup=%v", err, dup)
	}

	// The composite foreign key is declared and the summary hydrates providers.
	var fkCount int
	db := rawDB(t, st.Path())
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_foreign_key_list('domain_sending_configs')`).Scan(&fkCount); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if fkCount != 2 {
		t.Fatalf("sending config foreign key columns = %d, want 2", fkCount)
	}
	domA, err := st.GetDomain(context.Background(), "acc1", "domA")
	if err != nil || domA.SendingProvider != "brevo" || domA.ReceivingProvider != "resend" {
		t.Fatalf("domain summary after upgrade %+v err=%v", domA, err)
	}
}

func TestMigration013ReopenDoesNotResurrectRetiredTables(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// A second boot must not run the baseline and recreate the dropped tables.
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen after upgrade: %v", err)
	}
	defer st2.Close()
	for _, gone := range []string{"outbound_credentials", "inbound_credentials"} {
		if tablePresent(t, st2, gone) {
			t.Fatalf("reopen resurrected retired table %s", gone)
		}
	}
	if n := queryInt(t, st2, `SELECT count(*) FROM schema_migrations WHERE version='013'`); n != 1 {
		t.Fatalf("013 marker count = %d, want 1", n)
	}
	if n := queryInt(t, st2, `SELECT count(*) FROM domain_sending_configs`); n != 2 {
		t.Fatalf("configs after reopen = %d, want 2", n)
	}
	if n := queryInt(t, st2, `SELECT count(*) FROM outbound_delivery_log`); n != 2 {
		t.Fatalf("log rows after reopen = %d, want 2", n)
	}
}

func TestMigration013RollsBackOnFailure(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)
	// Create an index name that migration 013 will try to create. The index is
	// on a table 013 does not rebuild, so the conflict is real and must abort
	// (and roll back) the whole migration.
	db := rawDB(t, filepath.Join(dir, "inbox.db"))
	if _, err := db.ExecContext(context.Background(), `CREATE INDEX idx_domains_id_account ON messages(created_at)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	if _, err := store.Open(dir); err == nil {
		t.Fatal("expected migration 013 to fail on the conflicting index")
	}

	db = rawDB(t, filepath.Join(dir, "inbox.db"))
	defer db.Close()
	assert := func(q string, want int) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("%s = %d, want %d", q, n, want)
		}
	}
	// The transaction rolled back: old tables and columns remain, new tables do
	// not exist and the 013 marker is absent.
	assert(`SELECT count(*) FROM sqlite_master WHERE name='outbound_credentials'`, 1)
	assert(`SELECT count(*) FROM sqlite_master WHERE name='inbound_credentials'`, 1)
	assert(`SELECT count(*) FROM sqlite_master WHERE name='domain_sending_configs'`, 0)
	assert(`SELECT count(*) FROM pragma_table_info('domains') WHERE name='outbound_credential_id'`, 1)
	assert(`SELECT count(*) FROM schema_migrations WHERE version='013'`, 0)
}

func TestMigration013PartialSchemaFailsFast(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)
	// Simulate an interrupted 013: one new table exists without a marker.
	db := rawDB(t, filepath.Join(dir, "inbox.db"))
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE domain_sending_configs(dummy TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	_, err := store.Open(dir)
	if err == nil || !strings.Contains(err.Error(), "partially applied") {
		t.Fatalf("expected partial-schema error, got %v", err)
	}
}
