package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gatehouse-mail/internal/store"
)

// TestDomainConfigSaveAfterRemovalConflicts covers a concurrent removal: the
// caller holds a version for a config that is then deleted. The stale save must
// conflict rather than silently resurrecting the removed config.
func TestDomainConfigSaveAfterRemovalConflicts(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	first, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "enc-1", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	stale := store.ConfigVersion{ID: first.ID, Revision: first.Revision}
	if _, err = s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "enc-2", stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("save after removal err=%v, want ErrConflict", err)
	}
	if _, err = s.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("removed config was resurrected: %v", err)
	}
}

// TestDomainConfigSendingReceivingIsolation verifies the two per-domain config
// slots are independent: deleting or rotating one never touches the other.
func TestDomainConfigSendingReceivingIsolation(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	sending, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "send-1", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	receiving, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "resend", "recv-1", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if sending.ID == receiving.ID {
		t.Fatalf("sending and receiving share an id: %q", sending.ID)
	}
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if err != nil || got.ID != receiving.ID || got.EncryptedConfig != "recv-1" {
		t.Fatalf("receiving config changed by sending delete: %+v err=%v", got, err)
	}
	// A cross-slot version token must not be accepted as a sending update.
	if _, err = s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "send-2", store.ConfigVersion{ID: receiving.ID, Revision: receiving.Revision}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-slot version err=%v, want ErrConflict", err)
	}
}

// TestMigrationConfigCopiesRotateIndependently verifies that the per-domain
// copies made by migration 013 are genuinely independent rows: rotating one
// domain's config leaves the sibling's encrypted bytes untouched.
func TestMigrationConfigCopiesRotateIndependently(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	a, err := st.GetDomainSendingConfig(ctx, "acc1", "domA")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.GetDomainSendingConfig(ctx, "acc1", "domB")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.SaveDomainSendingConfig(ctx, "acc1", "domA", "smtp", "rotated-A", store.ConfigVersion{ID: a.ID, Revision: a.Revision}); err != nil {
		t.Fatal(err)
	}
	gotB, err := st.GetDomainSendingConfig(ctx, "acc1", "domB")
	if err != nil {
		t.Fatal(err)
	}
	if gotB.EncryptedConfig != b.EncryptedConfig || gotB.Revision != b.Revision {
		t.Fatalf("sibling config changed by rotation: before=%+v after=%+v", b, gotB)
	}
}

// TestDomainConfigFKRejectsMismatchedAccount checks the composite foreign key
// at the storage boundary: a config row cannot name a domain owned by a
// different account.
func TestDomainConfigFKRejectsMismatchedAccount(t *testing.T) {
	ctx := context.Background()
	s, _, d, _ := testStore(t)
	other, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, s.Path())
	defer db.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO domain_sending_configs(id,domain_id,account_id,provider,encrypted_config,revision,created_at,updated_at) VALUES('dsc_bad',?,?,?,?,1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, d.ID, other.AccountID, "brevo", "enc")
	if err == nil {
		t.Fatal("expected the composite foreign key to reject a mismatched account")
	}
}

// TestMigration013MarkerRecovery verifies a lost 013 marker is re-recorded from
// the already-applied schema without resurrecting the retired tables.
func TestMigration013MarkerRecovery(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, dir+"/inbox.db")
	if _, err := db.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version='013'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen after marker loss: %v", err)
	}
	defer st2.Close()
	if n := queryInt(t, st2, `SELECT count(*) FROM schema_migrations WHERE version='013'`); n != 1 {
		t.Fatalf("013 marker not recovered: %d", n)
	}
	if n := queryInt(t, st2, `SELECT count(*) FROM sqlite_master WHERE name='outbound_credentials'`); n != 0 {
		t.Fatalf("recovery resurrected outbound_credentials")
	}
}

// TestDomainConfigConcurrentSaveOneWins proves the CAS contract under
// concurrency: two writers that read the same revision cannot both commit; one
// wins and the other gets ErrConflict without corrupting the stored config.
func TestDomainConfigConcurrentSaveOneWins(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "seed", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	base, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	ver := store.ConfigVersion{ID: base.ID, Revision: base.Revision}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "writer", ver)
		}(i)
	}
	wg.Wait()
	okay, conflict := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			okay++
		case errors.Is(err, store.ErrConflict):
			conflict++
		default:
			t.Fatalf("unexpected save error: %v", err)
		}
	}
	if okay != 1 || conflict != 1 {
		t.Fatalf("concurrent saves okay=%d conflict=%d, want 1/1", okay, conflict)
	}
	got, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil || got.Revision != base.Revision+1 {
		t.Fatalf("final revision %+v err=%v, want %d", got, err, base.Revision+1)
	}
}

// TestMigration013FinalForeignKeysClean proves the rebuilt schema has no foreign
// key violations after upgrade and that enforcement is active on a fresh
// connection, i.e. the composite config FK and the log's domain FK are valid.
func TestMigration013FinalForeignKeysClean(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	db := rawDB(t, st.Path())
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after migration 013")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Enforcement is on for an independent connection (the rawDB default), so a
	// child row naming an unknown domain must be rejected.
	if _, err = db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO domain_receiving_configs(id,domain_id,account_id,provider,encrypted_config,revision,created_at,updated_at) VALUES('drc_bad','dom_missing','acc1','resend','x',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Fatal("expected the composite foreign key to reject an unknown domain")
	}
}
