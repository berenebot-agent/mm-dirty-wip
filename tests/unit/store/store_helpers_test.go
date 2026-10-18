package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
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

// TestAccountMXReceiverURLUnique proves the partial unique index lets many
// accounts leave the receiver URL empty but rejects a duplicate non-empty URL
// case-insensitively, so a shared remote receiver stays unambiguous.
func TestAccountMXReceiverURLUnique(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('a1','A',0,'2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES('a2','B',0,'2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO account_mx_receivers(account_id,receiver_url,created_at,updated_at) VALUES('a1','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO account_mx_receivers(account_id,receiver_url,created_at,updated_at) VALUES('a2','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	mustExec(`UPDATE account_mx_receivers SET receiver_url='https://mx.example' WHERE account_id='a1'`)
	if _, err := db.ExecContext(ctx, `UPDATE account_mx_receivers SET receiver_url='https://mx.example' WHERE account_id='a2'`); err == nil {
		t.Fatal("duplicate non-empty receiver_url was accepted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE account_mx_receivers SET receiver_url='https://MX.EXAMPLE' WHERE account_id='a2'`); err == nil {
		t.Fatal("case-equivalent receiver URL was accepted")
	}
}
