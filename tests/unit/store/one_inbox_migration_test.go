package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// Deliberately construct the pre-067 schema in a fresh disposable database.
func TestOneMigration067FansOutExistingCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Migration", "one-migration@example.test", "fixture-only-password", 100<<20)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	a, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "first@example.test"})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	b, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "second@example.test"})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	path := st.Path()
	st.Close()
	db := rawDB(t, path)
	for _, sql := range []string{
		`DROP TABLE inbox_one_bindings`, `DROP TABLE one_import_setups`, `DELETE FROM schema_migrations WHERE version='067'`,
		`DELETE FROM schema_migrations WHERE version='068'`,
		`CREATE TABLE one_accounts(id TEXT PRIMARY KEY,account_id TEXT NOT NULL,owner_user_id TEXT NOT NULL,encrypted_key TEXT NOT NULL,fingerprint TEXT NOT NULL,revision INTEGER NOT NULL,status TEXT NOT NULL,last_validated_at TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE inbox_one_bindings(inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,account_id TEXT NOT NULL,one_account_id TEXT NOT NULL REFERENCES one_accounts(id),connection_id TEXT NOT NULL,encrypted_connection_key TEXT NOT NULL,gmail_email TEXT NOT NULL,status TEXT NOT NULL,last_error TEXT NOT NULL,last_checked_at TEXT NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, sql); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO one_accounts VALUES('legacy-one',?,?,'legacy-key-ciphertext','legacy-fingerprint',7,'active','','','')`, u.AccountID, u.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, box := range []struct{ id, email, status string }{{a.ID, a.Address, "active"}, {b.ID, b.Address, "disconnected"}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO inbox_one_bindings VALUES(?,?,'legacy-one',?,'selector-ciphertext',?,?,'','')`, box.id, u.AccountID, box.id+"-connection", box.email, box.status); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO inbox_gmail_sync(inbox_id,account_id,history_id) VALUES(?,?,'123')`, box.id, u.AccountID); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE inboxes SET display_name=address||' · Gmail via One' WHERE id=?`, a.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE inboxes SET display_name='My custom Gmail' WHERE id=?`, b.ID); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, want := range []struct{ id, name string }{{a.ID, a.Address}, {b.ID, "My custom Gmail"}} {
		box, err := st.GetInboxInternal(ctx, u.AccountID, want.id)
		if err != nil || box.DisplayName != want.name {
			t.Fatalf("auto-name cleanup changed a custom name or retained suffix: %+v %v", box, err)
		}
	}
	for _, box := range []struct{ id, status string }{{a.ID, "active"}, {b.ID, "disconnected"}} {
		binding, err := st.OneBinding(ctx, u.AccountID, box.id)
		if err != nil || binding.EncryptedKey != "legacy-key-ciphertext" || binding.KeyAAD != "one_account:"+u.AccountID+":legacy-one" || binding.RateGroup != "legacy-one" || binding.Revision != 7 || binding.Status != box.status {
			t.Fatalf("migration lost independent binding: %+v %v", binding, err)
		}
		sync, err := st.GmailSync(ctx, u.AccountID, box.id)
		if err != nil || sync.HistoryID != "123" {
			t.Fatal("migration lost cursor", err)
		}
	}
	db = rawDB(t, path)
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='one_accounts'`).Scan(&count); err != nil || count != 0 {
		db.Close()
		t.Fatal("global credential table retained", err)
	}
	db.Close()
	if err := st.DeleteOneConnection(ctx, u.AccountID, a.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.OneBinding(ctx, u.AccountID, b.ID)
	if err != nil || remaining.EncryptedKey != "legacy-key-ciphertext" || remaining.Revision != 7 {
		t.Fatal("deleting migrated connection affected sibling", err)
	}
}
