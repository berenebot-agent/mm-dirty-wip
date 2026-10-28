package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dellarb/mammothmail/internal/store"
)

// TestDomainConfigSaveAfterRemovalConflicts covers a concurrent removal: the
// caller holds a version for a config that is then deleted. The stale save must
// conflict rather than silently resurrecting the removed config.
func TestDomainConfigSaveAfterRemovalConflicts(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestDomainConfigFKRejectsMismatchedAccount checks the composite foreign key
// at the storage boundary: a config row cannot name a domain owned by a
// different account.
func TestDomainConfigFKRejectsMismatchedAccount(t *testing.T) {
	t.Parallel()
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

// TestDomainConfigConcurrentSaveOneWins proves the CAS contract under
// concurrency: two writers that read the same revision cannot both commit; one
// wins and the other gets ErrConflict without corrupting the stored config.
func TestDomainConfigConcurrentSaveOneWins(t *testing.T) {
	t.Parallel()
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
