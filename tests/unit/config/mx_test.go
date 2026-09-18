package config_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
)

// testEdgeSecret is a fixed 32-byte (256-bit) hex secret for MX fixtures.
// Operator MX_EDGE_KEYS entries must meet the mxwire entropy bar.
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
	if len(cfg.MXEdgeKeys) != 0 {
		t.Fatal("MX edge keys should default empty")
	}
	if cfg.MXReceiptRetention.Hours() != 7*24 {
		t.Fatalf("receipt retention %v", cfg.MXReceiptRetention)
	}
}

func TestMXEmbeddedAutoEmbeds(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	t.Setenv("MX_EDGE_KEYS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("embedded MX should not require MX_EDGE_KEYS: %v", err)
	}
	if cfg.MXMode != config.MXLocal || !cfg.MXReceiveEnabled || !cfg.MXEmbedded {
		t.Fatalf("embedded mode not derived: mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
}

func TestMXRemoteRequiresKeys(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("MX_EDGE_KEYS", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("remote MX should require MX_EDGE_KEYS")
	}
}

func TestMXRemoteDoesNotEmbed(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("MX_EDGE_KEYS", "edge-1:"+testEdgeSecret)
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

func TestMXEdgeKeysParsed(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("MX_EDGE_KEYS", "edge1:"+testEdgeSecret+", edge2:"+testEdgeSecret+", malformed")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXEdgeKeys["edge1"] != testEdgeSecret || cfg.MXEdgeKeys["edge2"] != testEdgeSecret {
		t.Fatalf("edge keys %+v", cfg.MXEdgeKeys)
	}
	if _, ok := cfg.MXEdgeKeys["malformed"]; ok {
		t.Fatal("malformed entry should be ignored")
	}
}

func TestMXEdgeKeysRejectWeakSecret(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	for _, weak := range []string{"secret", "short", strings.Repeat("a", 31)} {
		t.Setenv("MX_EDGE_KEYS", "edge-1:"+weak)
		if _, err := config.Load(); err == nil {
			t.Fatalf("weak MX_EDGE_KEYS secret %q accepted", weak)
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
