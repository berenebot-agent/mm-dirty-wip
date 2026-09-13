package config_test

import (
	"testing"

	"gatehouse-mail/internal/config"
)

func TestMXDefaultsDisabled(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXReceiveEnabled {
		t.Fatal("MX should default off")
	}
	if len(cfg.MXEdgeKeys) != 0 {
		t.Fatal("MX edge keys should default empty")
	}
	if cfg.MXReceiptRetention.Hours() != 7*24 {
		t.Fatalf("receipt retention %v", cfg.MXReceiptRetention)
	}
}

func TestMXEnabledRequiresKeys(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "true")
	t.Setenv("MX_EDGE_KEYS", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when MX enabled without edge keys")
	}
}

func TestMXEdgeKeysParsed(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "true")
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
