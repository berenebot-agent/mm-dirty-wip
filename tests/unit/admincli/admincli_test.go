package admincli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/admincli"
	"gatehouse-mail/internal/auth"
	"gatehouse-mail/internal/store"
)

func TestMain(m *testing.M) {
	auth.SetIterationsForTest(1000)
	os.Exit(m.Run())
}

// seedInstance creates an on-disk instance with one administrator and returns
// its data directory. The store is closed before returning so the command can
// open it independently.
func seedInstance(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	session, _, err := st.CreateSession(context.Background(), u.ID, time.Hour)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, session
}

func writePasswordFile(t *testing.T, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResetPasswordWithFile(t *testing.T) {
	dir, oldSession := seedInstance(t)
	pwFile := writePasswordFile(t, "brand-new-password")
	var stdout, stderr bytes.Buffer
	code := admincli.Run([]string{"reset-password", "admin@example.com", "--password-file", pwFile}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Password updated successfully.") {
		t.Fatalf("missing success message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "All existing sessions have been revoked.") {
		t.Fatalf("missing revocation message: %q", stdout.String())
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.AuthenticateUser(ctx, "admin@example.com", "correct horse battery staple"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old password still valid: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "admin@example.com", "brand-new-password"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	if _, _, err := st.SessionPrincipal(ctx, oldSession); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived reset: %v", err)
	}
	if n, err := st.CountAuditKind(ctx, "admin.password_reset"); err != nil || n != 1 {
		t.Fatalf("audit rows = %d, %v; want 1", n, err)
	}
}

func TestResetPasswordUnknownUser(t *testing.T) {
	seedInstance(t)
	pwFile := writePasswordFile(t, "brand-new-password")
	var stdout, stderr bytes.Buffer
	code := admincli.Run([]string{"reset-password", "nobody@example.com", "--password-file", pwFile}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no account found") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestResetPasswordWeakPassword(t *testing.T) {
	seedInstance(t)
	pwFile := writePasswordFile(t, "short")
	var stdout, stderr bytes.Buffer
	code := admincli.Run([]string{"reset-password", "admin@example.com", "--password-file", pwFile}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "at least") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestResetPasswordRefusesNonInteractive(t *testing.T) {
	seedInstance(t)
	var stdout, stderr bytes.Buffer
	code := admincli.Run([]string{"reset-password", "admin@example.com"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "non-interactive") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRevokeAPIKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := st.CreateAPIKey(context.Background(), u.AccountID, "machine", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	var stdout, stderr bytes.Buffer
	code := admincli.Run([]string{"revoke-api-keys", "admin@example.com"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.APIKeyPrincipal(context.Background(), plain); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("API key still valid: %v", err)
	}
}
