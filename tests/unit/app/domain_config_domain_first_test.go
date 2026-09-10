package app_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/store"
)

// TestConfigSaveResolvesDomainBeforeProvider pins the ordering of the save
// path: domain ownership/existence is resolved before the provider schema is
// looked up, so a missing or foreign domain is ErrNotFound in both directions
// even when the requested provider is unknown. An owned domain with an unknown
// provider is still a user validation error, and no rejected save mutates any
// stored data.
func TestConfigSaveResolvesDomainBeforeProvider(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	// Account B owns another domain with valid sending and receiving configs so
	// the test can prove a rejected cross-account save leaves them untouched.
	b, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	bdom, err := svc.Store.CreateDomain(ctx, b.AccountID, "foreign.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveDomainSendingConfig(ctx, b.AccountID, bdom.ID, "brevo", map[string]any{"api_key": "b-key"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, b.AccountID, bdom.ID, "resend", map[string]any{"api_key": "b-key", "webhook_secret": "b-whsec"}, false); err != nil {
		t.Fatal(err)
	}
	bSend, err := svc.Store.GetDomainSendingConfig(ctx, b.AccountID, bdom.ID)
	if err != nil {
		t.Fatal(err)
	}
	bRecv, err := svc.Store.GetDomainReceivingConfig(ctx, b.AccountID, bdom.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown provider + missing/foreign domain -> ErrNotFound, never a 400.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, bdom.ID, "not-a-provider", map[string]any{"api_key": "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sending foreign domain unknown provider err=%v, want ErrNotFound", err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, bdom.ID, "not-a-provider", map[string]any{"api_key": "x"}, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("receiving foreign domain unknown provider err=%v, want ErrNotFound", err)
	}
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, "dom_missing", "not-a-provider", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("sending missing domain unknown provider err=%v, want ErrNotFound", err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, "dom_missing", "not-a-provider", nil, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("receiving missing domain unknown provider err=%v, want ErrNotFound", err)
	}

	// Account B's configs are byte-for-byte and revision-for-revision untouched.
	if got, err := svc.Store.GetDomainSendingConfig(ctx, b.AccountID, bdom.ID); err != nil || got.ID != bSend.ID || got.Revision != bSend.Revision || got.EncryptedConfig != bSend.EncryptedConfig {
		t.Fatalf("foreign sending config changed: %+v err=%v", got, err)
	}
	if got, err := svc.Store.GetDomainReceivingConfig(ctx, b.AccountID, bdom.ID); err != nil || got.ID != bRecv.ID || got.Revision != bRecv.Revision || got.EncryptedConfig != bRecv.EncryptedConfig {
		t.Fatalf("foreign receiving config changed: %+v err=%v", got, err)
	}

	// An owned domain with an unknown provider stays a validation error in both
	// directions, and the rejected saves leave no config behind.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "not-a-provider", nil); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("owned sending unknown provider err=%v, want ErrInvalidConfig", err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "not-a-provider", nil, false); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("owned receiving unknown provider err=%v, want ErrInvalidConfig", err)
	}
	if _, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("owned sending config unexpectedly persisted: %v", err)
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("owned receiving config unexpectedly persisted: %v", err)
	}
}
