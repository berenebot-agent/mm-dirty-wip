package config

import "testing"

const testKey = "01234567890123456789012345678901"

func TestInboundListenAddr(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", ":8081")

	t.Setenv("INBOUND_LISTEN_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InboundListenAddr != "" {
		t.Fatalf("InboundListenAddr = %q, want empty", cfg.InboundListenAddr)
	}

	t.Setenv("INBOUND_LISTEN_ADDR", ":8082")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InboundListenAddr != ":8082" {
		t.Fatalf("InboundListenAddr = %q, want :8082", cfg.InboundListenAddr)
	}
}

func TestInboundListenAddrMustDifferFromMain(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", ":8081")
	t.Setenv("INBOUND_LISTEN_ADDR", ":8081")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when INBOUND_LISTEN_ADDR equals LISTEN_ADDR")
	}
}
