package config_test

import (
	"testing"

	"gatehouse-mail/internal/config"
)

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
	t.Setenv("MX_EDGE_KEYS", "edge-1:secret")
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
	t.Setenv("MX_EDGE_KEYS", "edge1:secret1, edge2:secret2, malformed")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXEdgeKeys["edge1"] != "secret1" || cfg.MXEdgeKeys["edge2"] != "secret2" {
		t.Fatalf("edge keys %+v", cfg.MXEdgeKeys)
	}
	if _, ok := cfg.MXEdgeKeys["malformed"]; ok {
		t.Fatal("malformed entry should be ignored")
	}
}
