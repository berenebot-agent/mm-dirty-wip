package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestKeySessionResolvesToKeyScope verifies a browser session derived from an
// API key resolves to a principal with exactly the key's mailbox bindings and
// never an admin/system-admin role.
func TestKeySessionResolvesToKeyScope(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "root@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	k, plain, err := s.CreateAPIKey(ctx, u.AccountID, "agent", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if k.Admin {
		t.Fatal("fixture key unexpectedly admin")
	}
	if _, err := s.APIKeyPrincipal(ctx, plain); err != nil {
		t.Fatalf("key not resolvable: %v", err)
	}

	tok, csrf, err := s.CreateKeySession(ctx, k.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if csrf == "" {
		t.Fatal("key session missing csrf token")
	}
	p, gotCSRF, err := s.SessionPrincipal(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Admin || p.SystemAdmin {
		t.Fatalf("key session carried admin: %#v", p)
	}
	if p.APIKeyID != k.ID || p.UserID != "" {
		t.Fatalf("key session identity = %#v", p)
	}
	if p.MailboxRoles[box.ID] != "owner" {
		t.Fatalf("key session roles = %#v", p.MailboxRoles)
	}
	if gotCSRF != csrf {
		t.Fatalf("csrf mismatch: %q vs %q", gotCSRF, csrf)
	}
}

// TestKeySessionRefusedAfterRevoke verifies a live key session is refused once
// the underlying key is revoked.
func TestKeySessionRefusedAfterRevoke(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "root@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := s.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	k, _, _ := s.CreateAPIKey(ctx, u.AccountID, "agent", false, map[string]string{box.ID: "read"})
	tok, _, err := s.CreateKeySession(ctx, k.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, u.AccountID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionPrincipal(ctx, tok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked key session still resolves: %v", err)
	}
}

// TestDeleteKeySessionsForClient verifies revoke/rotate teardown drops every
// browser session minted from a key.
func TestDeleteKeySessionsForClient(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, _ := s.SyncSystemAdmin(ctx, "MailMoose", "root@example.com", "correct horse battery staple", 50<<20)
	d, _ := s.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := s.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	k, _, _ := s.CreateAPIKey(ctx, u.AccountID, "agent", false, map[string]string{box.ID: "owner"})
	first, _, _ := s.CreateKeySession(ctx, k.ID, time.Hour)
	second, _, _ := s.CreateKeySession(ctx, k.ID, time.Hour)
	s.DeleteKeySessionsForClient(ctx, k.ID)
	for _, tok := range []string{first, second} {
		if _, _, err := s.SessionPrincipal(ctx, tok); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("key session survived teardown: %v", err)
		}
	}
}

// TestKeySessionRejectsAdminKey verifies an admin key (which carries no mailbox
// bindings) cannot resolve to a browser session principal.
func TestKeySessionRejectsAdminKey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, _ := s.SyncSystemAdmin(ctx, "MailMoose", "root@example.com", "correct horse battery staple", 50<<20)
	k, _, err := s.CreateAPIKey(ctx, u.AccountID, "full", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreateKeySession(ctx, k.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionPrincipal(ctx, tok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admin key session resolved: %v", err)
	}
}
