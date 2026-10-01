package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestMXSettingsLifecycle covers the singleton configuration's create, CAS
// update, conflict, redaction-relevant storage and clear semantics.
func TestMXSettingsLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	if _, err := s.GetMXSettings(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("uninitialized GetMXSettings = %v", err)
	}
	initialized, err := s.MXSettingsInitialized(ctx)
	if err != nil || initialized {
		t.Fatalf("initialized = %v, %v", initialized, err)
	}

	created, err := s.SaveMXSettingsCAS(ctx, store.MXModeRemote, "https://receiver.example", "enc-secret", "enc-config", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.Mode != store.MXModeRemote || created.ReceiverURL != "https://receiver.example" {
		t.Fatalf("created = %+v", created)
	}

	// A zero-version save must not overwrite an existing row.
	if _, err := s.SaveMXSettingsCAS(ctx, store.MXModeIncluded, "", "", "", store.ConfigVersion{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create over existing = %v", err)
	}

	updated, err := s.SaveMXSettingsCAS(ctx, store.MXModeIncluded, "", "enc-secret-2", "enc-config-2", store.ConfigVersion{ID: store.MXSettingsID, Revision: created.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Mode != store.MXModeIncluded {
		t.Fatalf("updated = %+v", updated)
	}

	// A stale revision conflicts.
	if _, err := s.SaveMXSettingsCAS(ctx, store.MXModeRemote, "https://other", "x", "y", store.ConfigVersion{Revision: created.Revision}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision = %v", err)
	}

	stored, err := s.GetMXSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedSecret != "enc-secret-2" || stored.EncryptedConfig != "enc-config-2" {
		t.Fatalf("stored secret/config = %q/%q", stored.EncryptedSecret, stored.EncryptedConfig)
	}
	if initialized, err = s.MXSettingsInitialized(ctx); err != nil || !initialized {
		t.Fatalf("initialized after save = %v, %v", initialized, err)
	}

	// Clearing drops the routing and encrypted fields but keeps the row, so the
	// one-time environment import never re-fires.
	if err := s.ClearMXSettingsCAS(ctx, store.ConfigVersion{Revision: stored.Revision}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GetMXSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Mode != "" || cleared.ReceiverURL != "" || cleared.EncryptedSecret != "" || cleared.EncryptedConfig != "" {
		t.Fatalf("cleared = %+v", cleared)
	}
	if cleared.Revision != stored.Revision+1 {
		t.Fatalf("revision after clear = %d, want %d", cleared.Revision, stored.Revision+1)
	}
	if initialized, err = s.MXSettingsInitialized(ctx); err != nil || !initialized {
		t.Fatalf("initialized after clear = %v, %v", initialized, err)
	}

	// Clearing an already-empty configuration is a no-op and stays conflict-free.
	if err := s.ClearMXSettingsCAS(ctx, store.ConfigVersion{Revision: cleared.Revision}); err != nil {
		t.Fatalf("second clear = %v", err)
	}
	// A stale clear conflicts.
	if err := s.ClearMXSettingsCAS(ctx, store.ConfigVersion{Revision: 1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale clear = %v", err)
	}
}
