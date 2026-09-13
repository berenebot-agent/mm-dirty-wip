package config_test

import (
	"testing"

	"gatehouse-mail/internal/config"
)

func TestMXEmbeddedDefaults(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_EMBEDDED", "")
	t.Setenv("MX_RECEIVE_ENABLED", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXEmbedded {
		t.Fatal("MX_EMBEDDED should default off")
	}
	if cfg.MXUID != 65533 || cfg.MXGID != 65533 {
		t.Fatalf("MX_UID/MX_GID defaults = %d/%d", cfg.MXUID, cfg.MXGID)
	}
}

func TestMXEmbeddedAllowsNoKeys(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "true")
	t.Setenv("MX_EMBEDDED", "true")
	t.Setenv("MX_EDGE_KEYS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("embedded MX should not require MX_EDGE_KEYS: %v", err)
	}
	if !cfg.MXEmbedded || !cfg.MXReceiveEnabled {
		t.Fatalf("embedded flags %+v", cfg)
	}
}

func TestMXEmbeddedRejectsZeroUID(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "true")
	t.Setenv("MX_EMBEDDED", "true")
	t.Setenv("MX_UID", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for MX_UID=0")
	}
}

func TestMXSidecarStillRequiresKeys(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_RECEIVE_ENABLED", "true")
	t.Setenv("MX_EMBEDDED", "false")
	t.Setenv("MX_EDGE_KEYS", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("sidecar MX should require MX_EDGE_KEYS")
	}
}
