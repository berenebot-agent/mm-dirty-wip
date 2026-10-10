package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration053RemovesExternalAliases proves that on a genuinely fresh
// database (store.Open on a real temp dir, not the shared template) migration
// 053 leaves no trace of the removed external-alias feature: the table, the
// three attribution columns and their indexes are all gone, and the marker is
// recorded exactly once.
func TestMigration053RemovesExternalAliases(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "053"); n != 1 {
		t.Fatalf("migration 053 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='external_aliases'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("external_aliases table must not exist after migration 053")
	}
	for _, col := range []struct{ table, column string }{
		{"messages", "sending_external_alias_id"},
		{"drafts", "from_external_alias_id"},
		{"outbound_delivery_log", "external_alias_id"},
	} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, col.table, col.column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s.%s must not exist after migration 053", col.table, col.column)
		}
	}
	for _, idx := range []string{"idx_messages_external_alias", "idx_outbound_log_external_alias"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("index %s must not exist after migration 053", idx)
		}
	}
}

// TestManagedAliasesSurviveExternalAliasRemoval proves the removal migration
// leaves managed-domain aliases (inbox_aliases) and ordinary mail intact, since
// only the external sending-alias feature is removed.
func TestManagedAliasesSurviveExternalAliasRemoval(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales", DisplayName: "Sales"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "sales@example.com" {
		t.Fatalf("managed aliases not preserved: %+v", got.Aliases)
	}
}
