package config

import "testing"

const testKey = "01234567890123456789012345678901"

func TestDefaultListeners(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8081" {
		t.Fatalf("ListenAddr = %q, want :8081", cfg.ListenAddr)
	}
	if InboundAddr != ":8082" {
		t.Fatalf("InboundAddr = %q, want :8082", InboundAddr)
	}
}

func TestListenAddrMustDifferFromInbound(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", InboundAddr)
	if _, err := Load(); err == nil {
		t.Fatal("expected error when LISTEN_ADDR equals the inbound listener")
	}
}
