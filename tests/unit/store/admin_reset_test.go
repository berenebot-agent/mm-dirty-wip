package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
)

func TestGetUserByEmail(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	got, err := s.GetUserByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Fatalf("GetUserByEmail id = %q, want %q", got.ID, u.ID)
	}
	if _, err := s.GetUserByEmail(ctx, "missing@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown email error = %v, want ErrNotFound", err)
	}
}

func TestAdminResetPasswordRevokesSessionsAndKeepsAPIKeys(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	_, plain, err := s.CreateAPIKey(ctx, u.AccountID, "machine", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := s.CreateSession(ctx, u.ID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "new-correct-horse-staple"
	if err := s.AdminResetPassword(ctx, u.ID, newPassword); err != nil {
		t.Fatal(err)
	}
	// The old password no longer authenticates and the new one does.
	if _, err := s.AuthenticateUser(ctx, u.Email, "correct horse battery staple"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := s.AuthenticateUser(ctx, u.Email, newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	// Every browser session is revoked.
	if _, _, err := s.SessionPrincipal(ctx, session); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived reset: %v", err)
	}
	// API keys are independent machine integrations and stay valid.
	if _, err := s.APIKeyPrincipal(ctx, plain); err != nil {
		t.Fatalf("API key revoked by password reset: %v", err)
	}
	// The reset is audited.
	n, err := s.CountAuditKind(ctx, "admin.password_reset")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	// RevokeAPIKeysForAccount is a separate, explicit operation.
	if revoked, err := s.RevokeAPIKeysForAccount(ctx, u.AccountID); err != nil || revoked != 1 {
		t.Fatalf("RevokeAPIKeysForAccount = %d, %v; want 1 key", revoked, err)
	}
	if _, err := s.APIKeyPrincipal(ctx, plain); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("API key still valid after revoke: %v", err)
	}
}

func TestCreateInitialAdminIsOneShot(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CreateInitialAdmin(ctx, "A", "first@example.com", "correct horse battery staple", 50<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInitialAdmin(ctx, "B", "second@example.com", "correct horse battery staple", 50<<20); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second CreateInitialAdmin error = %v, want ErrConflict", err)
	}
}

func TestAdminResetPasswordUnknownUser(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := testStore(t)
	if err := s.AdminResetPassword(ctx, "usr_missing", "new-correct-horse-staple"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user error = %v, want ErrNotFound", err)
	}
}
