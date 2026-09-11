package config_test

import (
	"testing"

	"gatehouse-mail/internal/config"
)

const testKey = "01234567890123456789012345678901"

func TestDefaultListeners(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8081" {
		t.Fatalf("ListenAddr = %q, want :8081", cfg.ListenAddr)
	}
	if config.InboundAddr != ":8082" {
		t.Fatalf("config.InboundAddr = %q, want :8082", config.InboundAddr)
	}
}

func TestListenAddrMustDifferFromInbound(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("LISTEN_ADDR", config.InboundAddr)
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when LISTEN_ADDR equals the inbound listener")
	}
}

func TestRequirePublicOutboundByDefault(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("ALLOW_PRIVATE_OUTBOUND", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequirePublicOutbound() {
		t.Fatal("self-hosted must require public outbound by default")
	}
	t.Setenv("ALLOW_PRIVATE_OUTBOUND", "true")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequirePublicOutbound() {
		t.Fatal("self-hosted opt-out must disable the requirement")
	}
	// Hosted mode ignores the opt-out.
	t.Setenv("MODE", "hosted")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequirePublicOutbound() {
		t.Fatal("hosted mode must always require public outbound")
	}
}

func TestTrustedProxiesParsing(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("TRUSTED_PROXIES", "10.0.0.1,192.168.1.0/24,2001:db8::/32")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 3 {
		t.Fatalf("got %d proxies", len(cfg.TrustedProxies))
	}
	if !cfg.IsTrustedProxy("10.0.0.1:1234") {
		t.Fatal("exact IP should be trusted")
	}
	if !cfg.IsTrustedProxy("192.168.1.55:80") {
		t.Fatal("CIDR member should be trusted")
	}
	if cfg.IsTrustedProxy("192.168.2.1:80") {
		t.Fatal("non-member should not be trusted")
	}
	if cfg.IsTrustedProxy("8.8.8.8:53") {
		t.Fatal("untrusted peer should not be trusted")
	}
}

func TestTrustedProxiesBareIPAndHostCIDR(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("TRUSTED_PROXIES", "203.0.113.10,10.2.2.0/24")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsTrustedProxy("203.0.113.10:443") {
		t.Fatal("bare IP should be treated as /32")
	}
	if cfg.IsTrustedProxy("10.1.1.19:443") {
		t.Fatal("bare IP should not match a neighbouring host")
	}
	if !cfg.IsTrustedProxy("10.2.2.7:443") {
		t.Fatal("host CIDR should match")
	}
}

func TestTrustedProxiesInvalid(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("TRUSTED_PROXIES", "not-a-cidr")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for invalid TRUSTED_PROXIES")
	}
}

func TestTrustProxyHeadersFallback(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsTrustedProxy("1.2.3.4:80") {
		t.Fatal("legacy TRUST_PROXY_HEADERS should trust all peers")
	}
}
