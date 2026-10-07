package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestAccountMXReceiverStoreLifecycle covers the per-account receiver row's
// create, CAS update, conflict, clear and the in-use / listing helpers.
func TestAccountMXReceiverStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateAccountAndAdmin(ctx, "A", "admin@a.test", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetAccountMXReceiver(ctx, u.AccountID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured get = %v", err)
	}

	created, err := s.SaveAccountMXReceiverCAS(ctx, u.AccountID, "https://mx.a.test", "enc-secret", "enc-config", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.ReceiverURL != "https://mx.a.test" {
		t.Fatalf("created = %+v", created)
	}
	// A zero-version save must not overwrite an existing row.
	if _, err := s.SaveAccountMXReceiverCAS(ctx, u.AccountID, "https://other", "x", "y", store.ConfigVersion{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create over existing = %v", err)
	}
	// A stale revision conflicts.
	if _, err := s.SaveAccountMXReceiverCAS(ctx, u.AccountID, "https://other", "x", "y", store.ConfigVersion{Revision: created.Revision - 1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision = %v", err)
	}

	// The URL uniqueness helper excludes the calling account.
	urls, err := s.ListAccountMXReceiverURLs(ctx, "")
	if err != nil || len(urls) != 1 || urls[0] != "https://mx.a.test" {
		t.Fatalf("urls = %v, %v", urls, err)
	}
	if urls, _ := s.ListAccountMXReceiverURLs(ctx, u.AccountID); len(urls) != 0 {
		t.Fatalf("self-excluded urls = %v", urls)
	}

	// A domain routed to Remote MX makes the receiver in-use.
	d, err := s.CreateDomain(ctx, u.AccountID, "a.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "remotemx", "enc", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	if inUse, err := s.RemoteMXReceiverInUse(ctx, u.AccountID); err != nil || !inUse {
		t.Fatalf("in use = %v, %v", inUse, err)
	}
	accounts, err := s.ListRemoteMXAccounts(ctx)
	if err != nil || len(accounts) != 1 || accounts[0] != u.AccountID {
		t.Fatalf("accounts = %v, %v", accounts, err)
	}

	// Clearing drops the routing and encrypted fields but keeps the row.
	if err := s.ClearAccountMXReceiverCAS(ctx, u.AccountID, store.ConfigVersion{Revision: created.Revision}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GetAccountMXReceiver(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ReceiverURL != "" || cleared.EncryptedSecret != "" || cleared.EncryptedConfig != "" {
		t.Fatalf("cleared = %+v", cleared)
	}
	if cleared.Revision != created.Revision+1 {
		t.Fatalf("revision after clear = %d, want %d", cleared.Revision, created.Revision+1)
	}
}
