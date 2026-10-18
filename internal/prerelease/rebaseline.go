// Package prerelease implements the explicit, offline 080-to-public cutover.
// It is linked only into the maintenance utility, never the application server.
package prerelease

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/dbschema"
	_ "github.com/dellarb/mailmoose/internal/sqlite3driver"
)

// LegacySchemaHash was captured from a genuinely migrated 080 database at
// 9b6ae04. Tests reconstruct that exact schema and check this independent pin.
const LegacySchemaHash = "0794907d2e1fe917e50468bcc67460d5365d857317076f1ebed4df9a062effb8"

// Run validates an existing database and, only with apply, converts it in place.
// The operator must stop every application writer and back up all of /data first.
// No encryption key, configuration import, file cleanup or network is involved.
func Run(ctx context.Context, dataDir string, apply bool) (converted bool, result error) {
	path, err := filepath.Abs(filepath.Join(dataDir, "inbox.db"))
	if err != nil {
		return false, err
	}
	if strings.Contains(path, "?") {
		return false, fmt.Errorf("database path cannot contain '?' (SQLite driver does not support URI options)")
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("existing inbox.db required: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("inbox.db is not a regular file")
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if apply {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return false, err
		}
		defer func() {
			if _, err := conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON"); err != nil {
				result = fmt.Errorf("restore foreign key enforcement (converted=%t): %w", converted, err)
			}
		}()
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var format int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return false, err
	}
	if format == dbschema.Format {
		if err := dbschema.Validate(ctx, tx); err != nil {
			return false, err
		}
		return false, integrity(ctx, tx)
	}
	if format != 0 {
		return false, fmt.Errorf("unsupported database lineage %d", format)
	}
	if err := preflight(ctx, tx); err != nil {
		return false, err
	}
	if !apply {
		return false, nil
	}
	if err := convert(ctx, tx); err != nil {
		return false, err
	}
	if err := dbschema.Validate(ctx, tx); err != nil {
		return false, err
	}
	if err := integrity(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func preflight(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return fmt.Errorf("complete pre-release migration state 001–080 required: %w", err)
	}
	count := 0
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return err
		}
		count++
		if count > 80 || version != fmt.Sprintf("%03d", count) {
			rows.Close()
			return fmt.Errorf("expected exact pre-release sequence 001–080; unexpected marker %q", version)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if count != 80 {
		return fmt.Errorf("expected all 80 pre-release migration markers, found %d", count)
	}
	if err := dbschema.CheckHash(ctx, tx, LegacySchemaHash); err != nil {
		return err
	}
	if err := integrity(ctx, tx); err != nil {
		return err
	}
	checks := []struct{ description, query string }{
		{"account storage counters", `SELECT count(*) FROM accounts WHERE typeof(storage_used_bytes)<>'integer' OR storage_used_bytes<0`},
		{"inbox storage counters", `SELECT count(*) FROM inboxes WHERE storage_used_bytes IS NOT NULL AND (typeof(storage_used_bytes)<>'integer' OR storage_used_bytes<0)`},
		{"API-key credentials", `SELECT count(*) FROM clients c LEFT JOIN client_api_keys k ON k.client_id=c.id WHERE (c.type='api_key')<>(k.client_id IS NOT NULL)`},
		{"legacy API-key identities", `SELECT count(*) FROM api_keys a LEFT JOIN clients c ON c.id=a.id WHERE c.id IS NULL OR c.type<>'api_key' OR c.account_id<>a.account_id`},
		{"mailbox permissions", `SELECT count(*) FROM client_inbox_bindings b JOIN clients c ON c.id=b.client_id JOIN inboxes i ON i.id=b.inbox_id WHERE c.account_id<>i.account_id`},
		{"connector ownership", `SELECT count(*) FROM client_push p JOIN clients c ON c.id=p.client_id JOIN inboxes i ON i.id=p.inbox_id WHERE c.account_id<>i.account_id OR c.type NOT IN ('hermes','openclaw','webhook')`},
		{"password format (reset unsupported passwords before cutover)", `SELECT count(*) FROM users WHERE password_auth_enabled=1 AND substr(password_hash,1,10)<>'$argon2id$'`},
	}
	for _, check := range checks {
		var bad int
		if err := tx.QueryRowContext(ctx, check.query).Scan(&bad); err != nil {
			return err
		}
		if bad != 0 {
			return fmt.Errorf("pre-release consistency check failed: %s; repair explicitly before cutover", check.description)
		}
	}
	// Only live ciphertext is checked: retired Hermes copies may be stale.
	rows, err = tx.QueryContext(ctx, `SELECT m.name,p.name FROM sqlite_schema m JOIN pragma_table_info(m.name) p WHERE m.type='table' AND m.name<>'hermes_connections' AND (p.name LIKE '%encrypted%' OR p.name='dkim_private_key')`)
	if err != nil {
		return err
	}
	var columns [][2]string
	for rows.Next() {
		var column [2]string
		if err := rows.Scan(&column[0], &column[1]); err != nil {
			rows.Close()
			return err
		}
		columns = append(columns, column)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range columns {
		var bad int
		q := `SELECT count(*) FROM "` + column[0] + `" WHERE "` + column[1] + `"<>'' AND substr("` + column[1] + `",1,3)<>'v2.'`
		if err := tx.QueryRowContext(ctx, q).Scan(&bad); err != nil {
			return err
		}
		if bad != 0 {
			return fmt.Errorf("unsupported ciphertext in %s.%s; use the supported pre-release format before cutover", column[0], column[1])
		}
	}
	return nil
}

func integrity(ctx context.Context, tx *sql.Tx) error {
	var check string
	if err := tx.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("SQLite integrity check failed; restore a consistent backup")
	}
	return dbschema.ForeignKeyCheck(ctx, tx)
}

func convert(ctx context.Context, tx *sql.Tx) error {
	// Frozen 080 accounting: preserve known counters; materialize only unknown ones.
	if _, err := tx.ExecContext(ctx, normalizeUsage); err != nil {
		return err
	}
	var invalid int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE storage_used_bytes IS NULL OR typeof(storage_used_bytes)<>'integer' OR storage_used_bytes<0`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("cannot materialize valid inbox storage counters; no changes committed")
	}
	start := strings.Index(dbschema.Baseline, `CREATE TABLE "inboxes" (`)
	if start < 0 {
		return fmt.Errorf("public inbox schema missing")
	}
	end := strings.Index(dbschema.Baseline[start:], ";")
	if end < 0 {
		return fmt.Errorf("public inbox schema incomplete")
	}
	ddl := strings.Replace(dbschema.Baseline[start:start+end], `CREATE TABLE "inboxes"`, `CREATE TABLE inboxes_public`, 1)
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('inboxes') ORDER BY cid`)
	if err != nil {
		return err
	}
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		columns = append(columns, `"`+name+`"`)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	names := "rowid," + strings.Join(columns, ",")
	if _, err := tx.ExecContext(ctx, `INSERT INTO inboxes_public (`+names+`) SELECT `+names+` FROM inboxes`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DROP TABLE inboxes;
ALTER TABLE inboxes_public RENAME TO inboxes;
CREATE INDEX idx_inboxes_account ON inboxes(account_id);
CREATE INDEX idx_inboxes_authoring_mode ON inboxes(account_id,authoring_mode);
CREATE UNIQUE INDEX idx_inboxes_standalone_address ON inboxes(account_id,address COLLATE NOCASE) WHERE kind='standalone' AND address<>'';
DROP TABLE api_key_mailbox_roles;
DROP TABLE api_keys;
DROP TABLE hermes_connections;
ALTER TABLE assistant_handling_requests DROP COLUMN notification_status;
ALTER TABLE assistant_handling_requests DROP COLUMN notification_message_id;
DELETE FROM schema_migrations;
`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES('001',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", dbschema.Format))
	return err
}

const normalizeUsage = `UPDATE inboxes SET storage_used_bytes=
 (SELECT COALESCE(SUM(size_bytes),0) FROM messages WHERE account_id=inboxes.account_id AND inbox_id=inboxes.id)
+(SELECT COALESCE(SUM(LENGTH(CAST(text_body AS BLOB))+LENGTH(CAST(html_body AS BLOB))),0) FROM drafts WHERE account_id=inboxes.account_id AND inbox_id=inboxes.id)
+(SELECT COALESCE(SUM(a.size_bytes),0) FROM draft_attachments a JOIN drafts d ON d.id=a.draft_id WHERE d.account_id=inboxes.account_id AND d.inbox_id=inboxes.id)
+(SELECT COALESCE(SUM(LENGTH(CAST(folder_path AS BLOB))+LENGTH(CAST(rfc_message_id AS BLOB))+LENGTH(CAST(in_reply_to AS BLOB))+LENGTH(CAST(references_json AS BLOB))+LENGTH(CAST(thread_key AS BLOB))+LENGTH(CAST(from_name AS BLOB))+LENGTH(CAST(from_address AS BLOB))+LENGTH(CAST(to_json AS BLOB))+LENGTH(CAST(cc_json AS BLOB))+LENGTH(CAST(subject AS BLOB))+LENGTH(CAST(snippet AS BLOB))+LENGTH(CAST(flags_json AS BLOB))+96),0) FROM inbox_remote_messages WHERE account_id=inboxes.account_id AND inbox_id=inboxes.id)
+(SELECT COALESCE(SUM(LENGTH(CAST(path AS BLOB))+LENGTH(CAST(name AS BLOB))+LENGTH(CAST(parent_path AS BLOB))+LENGTH(CAST(role AS BLOB))+LENGTH(CAST(remote_metadata_json AS BLOB))+64),0) FROM inbox_folders WHERE account_id=inboxes.account_id AND inbox_id=inboxes.id AND origin='remote')
+(SELECT COALESCE(SUM(LENGTH(CAST(label AS BLOB))+32),0) FROM inbox_remote_labels WHERE account_id=inboxes.account_id AND inbox_id=inboxes.id)
WHERE storage_used_bytes IS NULL;`
