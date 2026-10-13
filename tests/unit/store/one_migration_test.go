package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
)

// Migration tests intentionally use a fresh store, not the migrated testdb
// template. Reconstruct the immediate pre-066 schema in this disposable DB.
func TestMigration066PreservesNativeGoogleGrantAndSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Migration", "migration@example.test", "fixture-only-password", 100<<20)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	box, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "google-migration@example.test"})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	if err := st.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "fixture-client", EncryptedSecret: "fixture-secret-ciphertext", EncryptedToken: "fixture-token-ciphertext", ExpiresAt: expires}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	path := st.Path()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	for _, statement := range []string{
		`UPDATE inbox_google_connections SET history_id='123',backfill_page='opaque-page',backfill_done=0,sync_generation='fixture-generation'`,
		`DROP TABLE gmail_history_pages`, `DROP TABLE inbox_one_bindings`, `DROP TABLE inbox_gmail_sync`,
		`DELETE FROM schema_migrations WHERE version='066'`,
		`DROP TABLE one_import_setups`,
		`DELETE FROM schema_migrations WHERE version='067'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	grant, err := st.GoogleConnection(ctx, u.AccountID, box.ID)
	if err != nil || grant.ClientID != "fixture-client" || grant.EncryptedSecret != "fixture-secret-ciphertext" || grant.EncryptedToken != "fixture-token-ciphertext" || !grant.ExpiresAt.Equal(expires) {
		t.Fatalf("native grant changed: %+v %v", grant, err)
	}
	sync, err := st.GmailSync(ctx, u.AccountID, box.ID)
	if err != nil || sync.HistoryID != "123" || sync.BackfillPage != "opaque-page" || sync.BackfillDone || sync.BackfillOffset != 0 {
		t.Fatalf("sync state lost: %+v %v", sync, err)
	}
	if st.IsOne(ctx, u.AccountID, box.ID) || !st.IsGoogle(ctx, u.AccountID, box.ID) {
		t.Fatal("migration changed provider")
	}
	if markerCount(t, st, "066") != 1 {
		t.Fatal("migration marker missing")
	}
}

// TestMigration070BackfillAfter proves migration 070 adds the One/Google
// backfill horizon column on a genuinely fresh database and that the store
// accessors round-trip it.
func TestMigration070BackfillAfter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if markerCount(t, st, "070") != 1 {
		t.Fatal("migration 070 marker missing")
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_gmail_sync') WHERE name='backfill_after'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("inbox_gmail_sync.backfill_after missing after 070")
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Horizon", "horizon@example.test", "fixture-only-password", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "horizon@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO inbox_gmail_sync(inbox_id,account_id,history_id) VALUES(?,?,?)`, box.ID, u.AccountID, "1"); err != nil {
		t.Fatal(err)
	}
	if after, err := st.GmailBackfillAfter(ctx, u.AccountID, box.ID); err != nil || after != "" {
		t.Fatalf("fresh horizon should be empty: %q %v", after, err)
	}
	if err := st.SetGmailBackfillAfter(ctx, u.AccountID, box.ID, "2026/01/15"); err != nil {
		t.Fatal(err)
	}
	if after, err := st.GmailBackfillAfter(ctx, u.AccountID, box.ID); err != nil || after != "2026/01/15" {
		t.Fatalf("horizon not stored: %q %v", after, err)
	}
	if _, err := st.GmailBackfillAfter(ctx, u.AccountID, "missing-inbox"); err != store.ErrNotFound {
		t.Fatalf("missing inbox should be ErrNotFound: %v", err)
	}
}
