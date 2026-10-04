package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// cred is a minimal well-formed passkey row for store-level tests. The bytes
// are arbitrary; the store does not interpret them.
func cred(name string, marker byte) model.WebAuthnCredential {
	return model.WebAuthnCredential{
		CredentialID: []byte{marker, marker, marker, marker},
		PublicKey:    []byte{marker, marker, marker, marker, marker, marker},
		Name:         name,
	}
}

func TestAddAndListWebAuthnCredentials(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateInitialAdmin(ctx, "Acme", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Laptop", 1), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Phone", 2), "none", "", true, true); err != nil {
		t.Fatal(err)
	}
	creds, err := s.WebAuthnCredentialsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d credentials, want 2", len(creds))
	}
	// A duplicate credential id must be a conflict, never a silent overwrite.
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Dup", 1), "none", "", false, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate credential err = %v, want ErrConflict", err)
	}
}

func TestWebAuthnCredentialLookupByCredentialID(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateInitialAdmin(ctx, "Acme", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Laptop", 7), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.WebAuthnCredentialByCredentialID(ctx, []byte{7, 7, 7, 7})
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != u.ID {
		t.Fatalf("lookup user = %q, want %q", got.UserID, u.ID)
	}
	if _, err := s.WebAuthnCredentialByCredentialID(ctx, []byte{9, 9, 9, 9}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing credential err = %v, want ErrNotFound", err)
	}
}

func TestDeleteWebAuthnCredentialLastMethodGuard(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateInitialAdmin(ctx, "Acme", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Only", 1), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	creds, _ := s.WebAuthnCredentialsForUser(ctx, u.ID)
	// With a password enabled the single passkey may be removed.
	if err := s.DeleteWebAuthnCredential(ctx, u.ID, creds[0].ID); err != nil {
		t.Fatalf("delete with password enabled: %v", err)
	}

	// Simulate a passkey-only user: two passkeys, then password disabled.
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Solo", 5), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Second", 6), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordAuth(ctx, u.ID, u.AccountID, false); err != nil {
		t.Fatal(err)
	}
	// Removing one passkey is fine; a second remains.
	creds, _ = s.WebAuthnCredentialsForUser(ctx, u.ID)
	if err := s.DeleteWebAuthnCredential(ctx, u.ID, creds[0].ID); err != nil {
		t.Fatalf("delete with two methods: %v", err)
	}
	// Removing the last passkey of a password-disabled account is refused.
	creds, _ = s.WebAuthnCredentialsForUser(ctx, u.ID)
	if err := s.DeleteWebAuthnCredential(ctx, u.ID, creds[0].ID); !errors.Is(err, store.ErrLastAuthMethod) {
		t.Fatalf("delete last method err = %v, want ErrLastAuthMethod", err)
	}
}

func TestPasswordlessLoginFailsPasswordPath(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateInitialAdmin(ctx, "Acme", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Laptop", 1), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordAuth(ctx, u.ID, u.AccountID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateUser(ctx, "admin@example.com", "correct horse battery staple"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("password login for passkey-only user err = %v, want ErrNotFound", err)
	}
}

// TestSyncSystemAdminPreservesPasskeys guards the break-glass invariant: the
// config-owned password rotation must never delete a sysadmin's passkeys.
func TestSyncSystemAdminPreservesPasskeys(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, _, err := s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Laptop", 1), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	// Rotate the config password.
	if _, changed, err := s.SyncSystemAdmin(ctx, "MailMoose", "admin@example.com", "another correct horse battery staple", 50<<20); err != nil || !changed {
		t.Fatalf("rotate changed=%v err=%v", changed, err)
	}
	creds, err := s.WebAuthnCredentialsForUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("passkey lost across password rotation: got %d, want 1", len(creds))
	}
	// The sysadmin keeps password login as break-glass.
	if u2, err := s.AuthenticateUser(ctx, "admin@example.com", "another correct horse battery staple"); err != nil || !u2.SystemAdmin {
		t.Fatalf("sysadmin password login after rotation: %#v, %v", u2, err)
	}
}

// TestSetPasswordAuthRefusesWithoutPasskey guards against locking an account
// out: password auth cannot be disabled until a passkey exists.
func TestSetPasswordAuthRefusesWithoutPasskey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateInitialAdmin(ctx, "Acme", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordAuth(ctx, u.ID, u.AccountID, false); !errors.Is(err, store.ErrLastAuthMethod) {
		t.Fatalf("disable without passkey err = %v, want ErrLastAuthMethod", err)
	}
	if err := s.AddWebAuthnCredential(ctx, u.ID, cred("Laptop", 1), "none", "", false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPasswordAuth(ctx, u.ID, u.AccountID, false); err != nil {
		t.Fatalf("disable with passkey: %v", err)
	}
	u2, err := s.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u2.PasswordEnabled {
		t.Fatal("password auth still enabled after disable")
	}
	// Re-enabling is always allowed.
	if err := s.SetPasswordAuth(ctx, u.ID, u.AccountID, true); err != nil {
		t.Fatal(err)
	}
}
