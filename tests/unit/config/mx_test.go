package config_test

import (
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/config"
)

// The receiver mode itself is not configured from the environment (the
// persisted mx_settings row is authoritative), so the remaining MX config surface
// is the operator tunables: receipt retention and the child's uid/gid.

func TestMXReceiptRetentionDefault(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXReceiptRetention != 7*24*time.Hour {
		t.Fatalf("receipt retention %v", cfg.MXReceiptRetention)
	}
}

func TestMXUIDMustBePositive(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_UID", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for MX_UID=0")
	}
}

func TestBaseURLValidation(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	for _, bad := range []string{
		"not-a-url",
		"://missing-scheme",
		"ftp://example.test",
		"https://user@example.test",
		"https://example.test/path",
		"https://example.test?x=1",
		"https://example.test#frag",
	} {
		t.Setenv("BASE_URL", bad)
		if _, err := config.Load(); err == nil {
			t.Fatalf("BASE_URL %q accepted", bad)
		}
	}
	t.Setenv("BASE_URL", "https://mail.example.test:8443")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseHost() != "mail.example.test:8443" {
		t.Fatalf("BaseHost = %q", cfg.BaseHost())
	}
}

func TestForceHTTPSRequiresHTTPSBaseURL(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("BASE_URL", "http://mail.example.test")
	t.Setenv("FORCE_HTTPS", "true")
	if _, err := config.Load(); err == nil {
		t.Fatal("FORCE_HTTPS with http BASE_URL accepted")
	}
}
