package dbschema_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/dbschema"
	"github.com/dellarb/mailmoose/internal/store"
)

const stamp = "2026-10-18T00:00:00Z"

func openDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("fixture SQL failed: %v\n%s", err, query)
	}
}

func scalar(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func fresh(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	return dir, openDB(t, dir)
}

// snapshot compares every table, including migration metadata, FTS shadow data
// and sequence high-water marks.
func snapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT name,sql FROM sqlite_schema WHERE type='table' AND (substr(name,1,7)<>'sqlite_' OR name='sqlite_sequence') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables [][2]string
	for rows.Next() {
		var table [2]string
		if err := rows.Scan(&table[0], &table[1]); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	out := map[string][]string{}
	for _, table := range tables {
		name := table[0]
		columns, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, name)
		if err != nil {
			t.Fatal(err)
		}
		var projection []string
		if !strings.Contains(strings.ToUpper(table[1]), "WITHOUT ROWID") {
			projection = append(projection, "rowid")
		}
		for columns.Next() {
			var column string
			if err := columns.Scan(&column); err != nil {
				t.Fatal(err)
			}
			projection = append(projection, `"`+column+`"`)
		}
		if err := columns.Err(); err != nil {
			t.Fatal(err)
		}
		columns.Close()
		data, err := db.Query(`SELECT ` + strings.Join(projection, ",") + ` FROM "` + name + `"`)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = []string{}
		for data.Next() {
			values := make([]any, len(projection))
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := data.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = append(out[name], string(encoded))
		}
		if err := data.Err(); err != nil {
			t.Fatal(err)
		}
		data.Close()
		sort.Strings(out[name])
	}
	return out
}

func basic(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('a','Developer',1000000,?)`, stamp)
	exec(t, db, `INSERT INTO domains(id,account_id,name,created_at) VALUES('d','a','example.test',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(rowid,id,account_id,domain_id,local_part,created_at) VALUES(77,'i','a','d','robot',?)`, stamp)
}

func TestFreshBaselineAndRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := openDB(t, dir)
	if err := dbschema.Validate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db, `SELECT count(*) FROM schema_migrations`) != 2 || scalar(t, db, `PRAGMA user_version`) != 1 {
		t.Fatal("fresh database did not get exactly the public baseline")
	}
	if scalar(t, db, `SELECT count(*) FROM sqlite_schema WHERE name IN ('api_keys','api_key_mailbox_roles','hermes_connections','external_aliases','one_accounts','system_settings','outbound_credentials','inbound_credentials','idx_inboxes_id_account')`) != 0 {
		t.Fatal("fresh baseline recreated retired objects")
	}
	var applied string
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, db)
	db.Close()
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db = openDB(t, dir)
	if !reflect.DeepEqual(before, snapshot(t, db)) {
		t.Fatal("restart changed stored data")
	}
	var after string
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations`).Scan(&after); err != nil || applied != after {
		t.Fatal("restart rewrote migration metadata", err)
	}
	if err := dbschema.Validate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

func TestRestartPreservesActiveData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir, db := fresh(t)
	basic(t, db)
	password, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("01234567890123456789012345678901")
	sealed, err := cryptox.EncryptWithAAD(key, []byte(`{"api_key":"fixture-secret"}`), []byte("cfg:a:d"))
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO users(id,account_id,email,password_hash,is_admin,created_at) VALUES('u','a','owner@example.test',?,1,?)`, password, stamp)
	exec(t, db, `INSERT INTO sessions VALUES(?,'u','csrf','2099-01-01T00:00:00Z',?)`, auth.HashToken("human-session"), stamp)
	exec(t, db, `INSERT INTO webauthn_credentials(id,user_id,credential_id,public_key,sign_count,backup_eligible,backup_state,created_at) VALUES('passkey','u',x'010203',x'040506',9,1,1,?)`, stamp)
	for _, id := range []string{"key", "revoked"} {
		exec(t, db, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,'a','api_key',?,?)`, id, id, stamp)
		exec(t, db, `INSERT INTO client_api_keys(client_id,key_prefix,key_hash) VALUES(?,?,?)`, id, "mmm_test", auth.HashToken(id+"-token"))
	}
	exec(t, db, `UPDATE clients SET revoked_at=? WHERE id='revoked'`, stamp)
	exec(t, db, `INSERT INTO client_inbox_bindings VALUES('key','i','owner',1)`)
	exec(t, db, `UPDATE domains SET catch_all_inbox_id='i' WHERE id='d'`)
	exec(t, db, `UPDATE accounts SET mailer_inbox_id='i',storage_used_bytes=4321 WHERE id='a'`)
	exec(t, db, `INSERT INTO key_sessions VALUES(?,'key','key-csrf','2099-01-01T00:00:00Z',?)`, auth.HashToken("key-session"), stamp)
	for _, pair := range [][2]string{{"relay", "hermes"}, {"retyped", "openclaw"}} {
		exec(t, db, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,'a',?,?,?)`, pair[0], pair[1], pair[0], stamp)
		exec(t, db, `INSERT INTO client_push(client_id,inbox_id,gateway_id,secret_encrypted,delivery_key_encrypted,last_ack_event_id) VALUES(?,'i',?,?,?,1)`, pair[0], pair[0]+"-gateway", sealed, sealed)
	}
	exec(t, db, `INSERT INTO domain_sending_configs(id,domain_id,account_id,provider,encrypted_config,created_at,updated_at) VALUES('sending','d','a','mailgun',?,?,?)`, sealed, stamp, stamp)
	exec(t, db, `INSERT INTO domain_receiving_configs(id,domain_id,account_id,provider,encrypted_config,created_at,updated_at) VALUES('receiving','d','a','mailgun',?,?,?)`, sealed, stamp, stamp)
	exec(t, db, `INSERT INTO mx_settings(id,mode,encrypted_secret,encrypted_config,created_at,updated_at) VALUES('mx','included',?,?,?,?)`, sealed, sealed, stamp, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,local_part,address,kind,created_at) VALUES('s','a','','remote@example.test','standalone',?)`, stamp)
	exec(t, db, `INSERT INTO inboxes(id,account_id,domain_id,local_part,storage_used_bytes,created_at) VALUES('known','a','d','known',987,?)`, stamp)
	exec(t, db, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES('thread','a','i','Preserve search',?,?)`, stamp, stamp)
	for i, state := range []string{"sent", "pending", "pending"} {
		marker := ""
		if i == 2 {
			marker = "started-claim"
		}
		exec(t, db, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,subject,raw_path,size_bytes,status,send_attempt_state,created_at) VALUES(?,'a','i','thread','outbound','Preserve search',?,?,?,?,?)`, fmt.Sprintf("message%d", i), fmt.Sprintf("messages/%d.eml", i), 40+i*10, state, marker, stamp)
	}
	exec(t, db, `INSERT INTO message_fts(message_id,account_id,inbox_id,subject,body) VALUES('message0','a','i','Preserve search','needle')`)
	exec(t, db, `INSERT INTO attachments(id,message_id,filename,part_index) VALUES('attachment','message0','report.txt',1)`)
	exec(t, db, `INSERT INTO message_labels VALUES('message0','Urgent',?)`, stamp)
	exec(t, db, `INSERT INTO drafts(id,account_id,inbox_id,text_body,html_body,created_at,updated_at) VALUES('draft','a','i','héllo','<p>ok</p>',?,?)`, stamp, stamp)
	exec(t, db, `INSERT INTO draft_attachments(id,draft_id,filename,size_bytes,raw_path,created_at) VALUES('draft-att','draft','a.txt',4,'drafts/a.txt',?)`, stamp)
	exec(t, db, `INSERT INTO draft_send_requests(id,account_id,inbox_id,draft_id,requested_at,created_at,updated_at,token_hash,notification_status) VALUES('approval','a','i','draft',?,?,?,'hashed-approval-token','uncertain')`, stamp, stamp, stamp)
	exec(t, db, `INSERT INTO outbound_workflow(id,account_id,inbox_id,request_id,kind,raw_path,status,send_attempt_state,created_at) VALUES('workflow','a','i','approval','approval_request','workflow/approval.eml','pending','workflow-claim',?)`, stamp)
	exec(t, db, `INSERT INTO assistant_handling_requests(id,account_id,inbox_id,draft_id,mode,requested_at,created_at,updated_at) VALUES('handoff','a','s','historic-draft','remote_draft',?,?,?)`, stamp, stamp, stamp)
	exec(t, db, `INSERT INTO inbox_remote_credentials(inbox_id,account_id,encrypted_imap,encrypted_smtp,created_at,updated_at) VALUES('s','a',?,?,?,?)`, sealed, sealed, stamp, stamp)
	exec(t, db, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,role,origin,created_at,updated_at) VALUES('folder','a','s','INBOX','Inbox','inbox','remote',?,?)`, stamp, stamp)
	exec(t, db, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,rfc_message_id,thread_key,subject,snippet,created_at,updated_at) VALUES('remote','a','s','INBOX','<remote@test>','remote-thread','Remote','Preview',?,?)`, stamp, stamp)
	exec(t, db, `INSERT INTO inbox_remote_labels(id,account_id,inbox_id,remote_message_id,label,created_at) VALUES('label','a','s','remote','Review',?)`, stamp)
	exec(t, db, `INSERT INTO inbox_google_connections(inbox_id,account_id,client_id,encrypted_secret,encrypted_token,expires_at) VALUES('s','a','google-client',?,?,'2099-01-01T00:00:00Z')`, sealed, sealed)
	exec(t, db, `INSERT INTO inbox_microsoft_connections VALUES('s','a','microsoft-client','tenant','identity','remote@example.test',0,?,'2099-01-01T00:00:00Z')`, sealed)
	exec(t, db, `INSERT INTO inbox_one_bindings(inbox_id,account_id,connection_id,encrypted_connection_key,encrypted_key,key_aad,fingerprint,rate_group,gmail_email) VALUES('s','a','connection',?,?,'retained-aad','fingerprint','rate-group','remote@example.test')`, sealed, sealed)
	exec(t, db, `INSERT INTO events(id,account_id,inbox_id,type,created_at) VALUES(1,'a','i','message.received',?)`, stamp)
	exec(t, db, `INSERT INTO events(id,account_id,inbox_id,type,created_at) VALUES(500,'a','i','message.received',?)`, stamp)
	exec(t, db, `DELETE FROM events WHERE id=500`)
	remoteBytes := len("INBOX<remote@test>[]remote-thread[][]RemotePreview[]") + 96 + len("INBOXInboxinbox{}") + 64 + len("Review") + 32
	exec(t, db, `UPDATE inboxes SET storage_used_bytes=169 WHERE id='i'`)
	exec(t, db, `UPDATE inboxes SET storage_used_bytes=? WHERE id='s'`, remoteBytes)
	files := map[string]string{"encryption_key": "fixture-key-file", "messages/0.eml": "original MIME", "messages/1.eml": "queued MIME", "messages/2.eml": "uncertain MIME", "drafts/a.txt": "data", "workflow/approval.eml": "approval MIME"}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, db)
	db.Close()
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	db = openDB(t, dir)
	if after := snapshot(t, db); !reflect.DeepEqual(before, after) {
		for table, rows := range before {
			if !reflect.DeepEqual(rows, after[table]) {
				t.Errorf("stored rows changed in %s", table)
			}
		}
		t.Fatal("restart changed stored data")
	}
	if err := dbschema.Validate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := dbschema.ForeignKeyCheck(ctx, db); err != nil {
		t.Fatal(err)
	}
	if got := scalar(t, db, `SELECT storage_used_bytes FROM inboxes WHERE id='i'`); got != 169 {
		t.Fatalf("local byte accounting=%d, want 169", got)
	}
	if got := scalar(t, db, `SELECT storage_used_bytes FROM inboxes WHERE id='s'`); got != int64(remoteBytes) {
		t.Fatalf("remote byte accounting=%d, want %d", got, remoteBytes)
	}
	if scalar(t, db, `SELECT storage_used_bytes FROM inboxes WHERE id='known'`) != 987 || scalar(t, db, `SELECT storage_used_bytes FROM accounts WHERE id='a'`) != 4321 {
		t.Fatal("known counters changed")
	}
	if scalar(t, db, `SELECT count(*) FROM message_fts WHERE message_fts MATCH 'needle'`) != 1 {
		t.Fatal("search history lost")
	}
	for name, body := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != body {
			t.Fatalf("file %s changed: %v", name, err)
		}
	}
	var encrypted string
	if err := db.QueryRow(`SELECT encrypted_config FROM domain_sending_configs`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plaintext, err := cryptox.DecryptWithAAD(key, encrypted, []byte("cfg:a:d"))
	if err != nil || string(plaintext) != `{"api_key":"fixture-secret"}` {
		t.Fatal("encrypted provider configuration became unreadable", err)
	}
	db.Close()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.APIKeyPrincipal(ctx, "key-token")
	if err != nil || p.MailboxRoles["i"] != "owner" || !p.DomainSend["i"] {
		t.Fatalf("key scope/domain grant not preserved: %+v %v", p, err)
	}
	if _, err := s.APIKeyPrincipal(ctx, "revoked-token"); err == nil {
		t.Fatal("revoked credential was resurrected")
	}
	if !auth.CheckPassword(password, "correct horse battery staple") {
		t.Fatal("password no longer verifies")
	}
	if _, err := s.AuthenticateUser(ctx, "owner@example.test", "correct horse battery staple"); err != nil {
		t.Fatal("human login after restart failed", err)
	}
	if principal, csrf, err := s.SessionPrincipal(ctx, "human-session"); err != nil || principal.UserID != "u" || csrf != "csrf" {
		t.Fatal("human session after restart failed", err)
	}
	db = openDB(t, dir)
	exec(t, db, `INSERT INTO events(account_id,inbox_id,type,created_at) VALUES('a','i','message.received',?)`, stamp)
	if scalar(t, db, `SELECT max(id) FROM events`) != 501 {
		t.Fatal("event cursor high-water mark was reset")
	}
	if _, err := db.Exec(`UPDATE inboxes SET storage_used_bytes=NULL WHERE id='i'`); err == nil {
		t.Fatal("public counter still permits NULL")
	}
	exec(t, db, `DELETE FROM message_fts WHERE message_id='message0'`)
	if scalar(t, db, `SELECT count(*) FROM message_fts WHERE message_fts MATCH 'needle'`) != 0 {
		t.Fatal("FTS deletion no longer works")
	}
}

func TestApplicationRejectsUnknownOrPartialDatabases(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]string{
		"missing baseline marker":  `DELETE FROM schema_migrations`,
		"unknown future migration": `INSERT INTO schema_migrations VALUES('003','future')`,
		"unknown lineage":          `PRAGMA user_version=99`,
		"unsupported lineage":      `PRAGMA user_version=0`,
		"malformed marker":         `UPDATE schema_migrations SET version='bad' WHERE version='001'`,
		"extra table":              `CREATE TABLE unexpected(payload TEXT)`,
		"partial column":           `ALTER TABLE inbox_one_bindings DROP COLUMN failure_count`,
		"missing table":            `DROP TABLE key_sessions`,
		"wrong index predicate":    `DROP INDEX idx_send_requests_issued_token; CREATE INDEX idx_send_requests_issued_token ON draft_send_requests(token_hash) WHERE token_hash=''`,
		"view impersonation":       `DROP TABLE key_sessions; CREATE VIEW key_sessions AS SELECT 'x' AS id_hash`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			db := openDB(t, dir)
			exec(t, db, mutate)
			before, err := dbschema.Fingerprint(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			if unexpected, err := store.Open(dir); err == nil {
				unexpected.Close()
				t.Fatal("application accepted a partial/unknown database")
			}
			after, err := dbschema.Fingerprint(context.Background(), db)
			if err != nil || before != after {
				t.Fatal("rejection modified schema", err)
			}
		})
	}
	t.Run("no metadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		db := openDB(t, dir)
		exec(t, db, `CREATE TABLE unidentified(payload TEXT)`)
		if s, err := store.Open(dir); err == nil {
			s.Close()
			t.Fatal("application treated an unknown database as fresh")
		}
		if scalar(t, db, `SELECT count(*) FROM sqlite_schema WHERE name='accounts'`) != 0 {
			t.Fatal("baseline was run against an unknown database")
		}
	})
}
