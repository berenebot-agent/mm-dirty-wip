package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

func TestDialMXCredentialAccountIsolationAndDomainCascade(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	credential, created, err := s.EnsureDialMXCredential(ctx, u.AccountID, d.ID, "key1", "cipher1", "pub1")
	if err != nil || !created || credential.KeyID != "key1" {
		t.Fatalf("ensure: %+v created=%v err=%v", credential, created, err)
	}
	again, created, err := s.EnsureDialMXCredential(ctx, u.AccountID, d.ID, "ignored", "ignored", "ignored")
	if err != nil || created || again.KeyID != "key1" || again.EncryptedPrivateSeed != "cipher1" {
		t.Fatalf("ensure must preserve exact domain key: %+v created=%v err=%v", again, created, err)
	}
	if _, err := s.GetDialMXCredential(ctx, "foreign", d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account get: %v", err)
	}
	if _, err := s.RotateDialMXCredentialCAS(ctx, u.AccountID, d.ID, "key2", "cipher2", "pub2", store.ConfigVersion{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rotation without Dial MX config: %v", err)
	}
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", "encrypted-config", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotateDialMXCredentialCAS(ctx, u.AccountID, d.ID, "key2", "cipher2", "pub2", store.ConfigVersion{ID: credential.KeyID, Revision: credential.Revision})
	if err != nil || rotated.KeyID != "key2" {
		t.Fatalf("rotation: %+v err=%v", rotated, err)
	}
	if _, err := s.RotateDialMXCredentialCAS(ctx, u.AccountID, d.ID, "key3", "cipher3", "pub3", store.ConfigVersion{ID: credential.KeyID, Revision: credential.Revision}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale rotation: %v", err)
	}
	if _, err := s.PurgeDomain(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDialMXCredential(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential did not cascade: %v", err)
	}
}
