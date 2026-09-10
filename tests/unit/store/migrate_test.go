package store_test

import (
	"context"
	"database/sql"
	"testing"

	"gatehouse-mail/internal/store"
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

func markerCount(t *testing.T, st *store.Store, version string) int {
	t.Helper()
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM schema_migrations WHERE version=?`, version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateFresh(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, v := range []string{"001", "002", "003", "004", "005", "006", "007", "008", "009", "010", "011", "012", "013", "014"} {
		if n := markerCount(t, st, v); n != 1 {
			t.Fatalf("migration %s marker count %d", v, n)
		}
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info('messages') WHERE name='claim_owner'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("claim_owner column missing after migrate")
	}
	// The retired credential tables must not exist on a fresh database.
	for _, table := range []string{"outbound_credentials", "inbound_credentials"} {
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("retired table %s still exists after fresh migrate", table)
		}
	}
	// The new per-domain config tables must exist.
	for _, table := range []string{"domain_sending_configs", "domain_receiving_configs"} {
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("config table %s missing after fresh migrate", table)
		}
	}
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_foreign_key_list('domain_sending_configs')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// A two-column composite foreign key is reported as two rows.
	if n != 2 {
		t.Fatalf("domain_sending_configs foreign key columns = %d, want 2", n)
	}
}
