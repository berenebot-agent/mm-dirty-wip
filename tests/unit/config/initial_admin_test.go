package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gatehouse-mail/internal/config"
)

func clearInitialAdminEnv(t *testing.T) {
	t.Helper()
	t.Setenv("INITIAL_ADMIN_EMAIL", "")
	t.Setenv("INITIAL_ADMIN_PASSWORD", "")
	t.Setenv("INITIAL_ADMIN_EMAIL_FILE", "")
	t.Setenv("INITIAL_ADMIN_PASSWORD_FILE", "")
	t.Setenv("INITIAL_ACCOUNT_NAME", "")
}

func TestInitialAdminFromEnv(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("INITIAL_ADMIN_PASSWORD", "correct-horse-battery-staple")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitialAdminEmail != "admin@example.com" {
		t.Fatalf("InitialAdminEmail = %q", cfg.InitialAdminEmail)
	}
	if cfg.InitialAdminPassword != "correct-horse-battery-staple" {
		t.Fatalf("InitialAdminPassword = %q", cfg.InitialAdminPassword)
	}
	if cfg.InitialAccountName != "Gatehouse" {
		t.Fatalf("InitialAccountName = %q, want Gatehouse", cfg.InitialAccountName)
	}
}

func TestInitialAdminAccountName(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("INITIAL_ADMIN_PASSWORD", "correct-horse-battery-staple")
	t.Setenv("INITIAL_ACCOUNT_NAME", "Acme")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitialAccountName != "Acme" {
		t.Fatalf("InitialAccountName = %q, want Acme", cfg.InitialAccountName)
	}
}

func TestInitialAdminUnset(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitialAdminEmail != "" || cfg.InitialAdminPassword != "" {
		t.Fatal("unset initial admin must stay empty")
	}
	if cfg.InitialAccountName != "Gatehouse" {
		t.Fatalf("InitialAccountName = %q, want Gatehouse", cfg.InitialAccountName)
	}
}

func TestInitialAdminFileSecrets(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	dir := t.TempDir()
	emailPath := filepath.Join(dir, "email")
	pwPath := filepath.Join(dir, "password")
	if err := os.WriteFile(emailPath, []byte("admin@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pwPath, []byte("correct-horse-battery-staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITIAL_ADMIN_EMAIL_FILE", emailPath)
	t.Setenv("INITIAL_ADMIN_PASSWORD_FILE", pwPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InitialAdminEmail != "admin@example.com" {
		t.Fatalf("InitialAdminEmail = %q, want trimmed file value", cfg.InitialAdminEmail)
	}
	if cfg.InitialAdminPassword != "correct-horse-battery-staple" {
		t.Fatalf("InitialAdminPassword = %q, want trimmed file value", cfg.InitialAdminPassword)
	}
}

func TestInitialAdminBothFormsError(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	dir := t.TempDir()
	emailPath := filepath.Join(dir, "email")
	if err := os.WriteFile(emailPath, []byte("admin@example.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("INITIAL_ADMIN_EMAIL_FILE", emailPath)
	t.Setenv("INITIAL_ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when both the value and _FILE form are set")
	}
}

func TestInitialAdminPartialError(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@example.com")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for email without password")
	}
	if !strings.Contains(err.Error(), "must be supplied together") {
		t.Fatalf("error = %v", err)
	}
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for password without email")
	}
}

func TestInitialAdminInvalidEmail(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_EMAIL", "not-an-address")
	t.Setenv("INITIAL_ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for an invalid email")
	}
}

func TestInitialAdminWeakPassword(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearInitialAdminEnv(t)
	t.Setenv("INITIAL_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("INITIAL_ADMIN_PASSWORD", "short")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for a weak password")
	}
	if strings.Contains(err.Error(), "short") {
		t.Fatalf("error must not include the password: %v", err)
	}
}
