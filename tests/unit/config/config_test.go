package config_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
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

func TestTrustProxyHeadersRefused(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("TRUST_PROXY_HEADERS", "true")
	if _, err := config.Load(); err == nil {
		t.Fatal("TRUST_PROXY_HEADERS=true must be refused: it trusts every caller")
	}
}

func TestTrustedProxiesCatchAllRefused(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	for _, v := range []string{"0.0.0.0/0", "::/0"} {
		t.Setenv("TRUSTED_PROXIES", v)
		if _, err := config.Load(); err == nil {
			t.Fatalf("TRUSTED_PROXIES=%s must be refused: it trusts every caller", v)
		}
	}
}

// A prefix wider than the floor is refused even though it is not /0: a /1 is
// not a proxy address in any real topology and still covers ordinary clients.
// This is the gap the retest recorded as an open item (the original guard
// checked each entry's prefix length only for == 0).
func TestTrustedProxiesTooWideRefused(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	for _, v := range []string{"0.0.0.0/1", "10.0.0.0/1", "::/1", "192.0.0.0/2"} {
		t.Setenv("TRUSTED_PROXIES", v)
		if _, err := config.Load(); err == nil {
			t.Fatalf("TRUSTED_PROXIES=%s must be refused: wider than the accepted floor", v)
		}
	}
}

// The floor must not refuse legitimate proxy entries. /8 is the narrowest
// accepted, and everything narrower (including the live deployment's bare IP)
// must keep loading.
func TestTrustedProxiesAtOrNarrowerThanFloorAccepted(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	// 0.0.0.0/8 == a /8 prefix; 255.0.0.0/8 is the narrowest accepted.
	t.Setenv("TRUSTED_PROXIES", "255.0.0.0/8")
	if _, err := config.Load(); err != nil {
		t.Fatalf("/8 must be accepted (it is the floor): %v", err)
	}
	t.Setenv("TRUSTED_PROXIES", "203.0.113.10")
	if _, err := config.Load(); err != nil {
		t.Fatalf("bare IP must be accepted: %v", err)
	}
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/9")
	if _, err := config.Load(); err != nil {
		t.Fatalf("/9 must be accepted: %v", err)
	}
}
