package config_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
)

// testEdgeSecret is a fixed bearer secret for private MX fixtures.
const testEdgeSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMXDefaultsDisabled(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXMode != config.MXOff || cfg.MXReceiveEnabled || cfg.MXEmbedded {
		t.Fatalf("expected MX off, got mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
	if cfg.MXCoreKey != "" || cfg.MXReceiverURL != "" {
		t.Fatal("private MX connection should default empty")
	}
	if cfg.MXReceiptRetention.Hours() != 7*24 {
		t.Fatalf("receipt retention %v", cfg.MXReceiptRetention)
	}
}

func TestMXEmbeddedAutoEmbeds(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("embedded MX should provision its key automatically: %v", err)
	}
	if cfg.MXMode != config.MXLocal || !cfg.MXReceiveEnabled || !cfg.MXEmbedded {
		t.Fatalf("embedded mode not derived: mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
}

func TestMXRemoteRequiresKeys(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", "")
	t.Setenv("MX_RECEIVER_URL", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("remote MX should require receiver URL and key")
	}
}

func TestMXRemoteDoesNotEmbed(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", testEdgeSecret)
	t.Setenv("MX_RECEIVER_URL", "http://receiver:8443")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXMode != config.MXRemote || !cfg.MXReceiveEnabled || cfg.MXEmbedded {
		t.Fatalf("remote mode wrong: mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
}

func TestMXInvalidModeRejected(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "sidecar")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for an unknown MX_ENABLE value")
	}
}

func TestMXEmbeddedRejectsZeroUID(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	t.Setenv("MX_UID", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for MX_UID=0 in embedded mode")
	}
}

func TestMXReceiverURLValidation(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", testEdgeSecret)
	for _, bad := range []string{"ftp://receiver", "http://user@receiver", "http://receiver/path", "http://receiver?x=1", "http://receiver#fragment"} {
		t.Setenv("MX_RECEIVER_URL", bad)
		if _, err := config.Load(); err == nil {
			t.Fatalf("invalid receiver URL %q accepted", bad)
		}
	}
}

func TestBaseURLValidation(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "")
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
	t.Setenv("MX_ENABLE", "")
	t.Setenv("BASE_URL", "http://mail.example.test")
	t.Setenv("FORCE_HTTPS", "true")
	if _, err := config.Load(); err == nil {
		t.Fatal("FORCE_HTTPS with http BASE_URL accepted")
	}
}
