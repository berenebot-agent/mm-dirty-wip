package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func applyRaw(t *testing.T, db *sql.DB, text string) {
	t.Helper()
	if _, err := db.Exec(text); err != nil {
		t.Fatalf("apply sql: %v", err)
	}
}

func markerCount(t *testing.T, st *Store, version string) int {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(), `SELECT count(*) FROM schema_migrations WHERE version=?`, version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateFresh(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, v := range []string{"001", "002", "003", "004", "005", "006", "007", "008", "009", "010", "011", "012"} {
		if n := markerCount(t, st, v); n != 1 {
			t.Fatalf("migration %s marker count %d", v, n)
		}
	}
	var n int
	if err := st.read.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info('messages') WHERE name='claim_owner'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("claim_owner column missing after migrate")
	}
}

// seedV9 builds a database at the pre-fix 009 schema with markers 001-008,
// simulating a real instance about to upgrade.
func seedV9(t *testing.T, path string) {
	t.Helper()
	db := rawDB(t, path)
	defer db.Close()
	for _, sqlText := range []string{migration001, migration002, migration003, migration004, migration005, migration006, migration007} {
		applyRaw(t, db, sqlText)
	}
	applyRaw(t, db, "PRAGMA foreign_keys=OFF")
	applyRaw(t, db, migration008)
	applyRaw(t, db, "PRAGMA foreign_keys=ON")
	for _, v := range []string{"001", "002", "003", "004", "005", "006", "007", "008"} {
		if _, err := db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,datetime('now'))`, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigratePartial009FailsClearly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.db")
	seedV9(t, path)
	db := rawDB(t, path)
	applyRaw(t, db, `CREATE TABLE IF NOT EXISTS inbound_credentials (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, provider TEXT NOT NULL, name TEXT NOT NULL, encrypted_config TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);`)
	applyRaw(t, db, `ALTER TABLE domains ADD COLUMN inbound_credential_id TEXT;`)
	db.Close()

	if _, err := Open(dir); err == nil {
		t.Fatal("expected a clear partial-migration error, got nil")
	}
}

func TestMigrateReconcilesCompleted009(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.db")
	seedV9(t, path)
	db := rawDB(t, path)
	applyRaw(t, db, "PRAGMA foreign_keys=OFF")
	applyRaw(t, db, migration009)
	applyRaw(t, db, "PRAGMA foreign_keys=ON")
	db.Close()

	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "009"); n != 1 {
		t.Fatalf("009 marker count %d after reconcile", n)
	}
}

func TestMigrateUpgradesFromV9(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.db")
	seedV9(t, path)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, v := range []string{"009", "010", "011", "012"} {
		if n := markerCount(t, st, v); n != 1 {
			t.Fatalf("migration %s marker count %d after upgrade", v, n)
		}
	}
}
