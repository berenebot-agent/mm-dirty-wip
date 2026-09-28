package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
)

func clearAdminEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_EMAIL", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("ADMIN_EMAIL_FILE", "")
	t.Setenv("ADMIN_PASSWORD_FILE", "")
	t.Setenv("ADMIN_ACCOUNT_NAME", "")
}

func TestAdminFromEnv(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminEmail != "admin@example.com" {
		t.Fatalf("AdminEmail = %q", cfg.AdminEmail)
	}
	if cfg.AdminPassword != "correct-horse-battery-staple" {
		t.Fatalf("AdminPassword = %q", cfg.AdminPassword)
	}
	if cfg.AdminAccountName != "MailMoose" {
		t.Fatalf("AdminAccountName = %q, want MailMoose", cfg.AdminAccountName)
	}
}

func TestAdminAccountName(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	t.Setenv("ADMIN_ACCOUNT_NAME", "Acme")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAccountName != "Acme" {
		t.Fatalf("AdminAccountName = %q, want Acme", cfg.AdminAccountName)
	}
}

func TestAdminUnset(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminEmail != "" || cfg.AdminPassword != "" {
		t.Fatal("unset admin must stay empty so the stored login is preserved")
	}
	if cfg.AdminAccountName != "MailMoose" {
		t.Fatalf("AdminAccountName = %q, want MailMoose", cfg.AdminAccountName)
	}
}

func TestAdminFileSecrets(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	dir := t.TempDir()
	emailPath := filepath.Join(dir, "email")
	pwPath := filepath.Join(dir, "password")
	if err := os.WriteFile(emailPath, []byte("admin@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pwPath, []byte("correct-horse-battery-staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_EMAIL_FILE", emailPath)
	t.Setenv("ADMIN_PASSWORD_FILE", pwPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminEmail != "admin@example.com" {
		t.Fatalf("AdminEmail = %q, want trimmed file value", cfg.AdminEmail)
	}
	if cfg.AdminPassword != "correct-horse-battery-staple" {
		t.Fatalf("AdminPassword = %q, want trimmed file value", cfg.AdminPassword)
	}
}

func TestAdminBothFormsError(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	dir := t.TempDir()
	emailPath := filepath.Join(dir, "email")
	if err := os.WriteFile(emailPath, []byte("admin@example.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	t.Setenv("ADMIN_EMAIL_FILE", emailPath)
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when both the value and _FILE form are set")
	}
}

func TestAdminPartialError(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for email without password")
	}
	if !strings.Contains(err.Error(), "must be supplied together") {
		t.Fatalf("error = %v", err)
	}
	clearAdminEnv(t)
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for password without email")
	}
}

func TestAdminInvalidEmail(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL", "not-an-address")
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for an invalid email")
	}
}

func TestAdminWeakPassword(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	t.Setenv("ADMIN_PASSWORD", "short")
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error for a weak password")
	}
	if strings.Contains(err.Error(), "short") {
		t.Fatalf("error must not include the password: %v", err)
	}
}

func TestAdminMissingFileFails(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	clearAdminEnv(t)
	t.Setenv("ADMIN_EMAIL_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("ADMIN_PASSWORD", "correct-horse-battery-staple")
	if _, err := config.Load(); err == nil {
		t.Fatal("an explicitly configured but unreadable *_FILE must fail startup, not silently fall back")
	}
}
