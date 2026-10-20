package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
)

func TestDNSFallbackDefaults(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.1.1.1:53", "1.0.0.1:53", "8.8.8.8:53", "8.8.4.4:53"}
	if !reflect.DeepEqual(cfg.DNSFallbackServers, want) {
		t.Fatalf("DNSFallbackServers = %v, want %v", cfg.DNSFallbackServers, want)
	}
}

func TestDNSFallbackCustomAndOff(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "10.0.0.53, [::1]:5353")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.53:53", "[::1]:5353"}; !reflect.DeepEqual(cfg.DNSFallbackServers, want) {
		t.Fatalf("DNSFallbackServers = %v, want %v", cfg.DNSFallbackServers, want)
	}

	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "off")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DNSFallbackServers != nil {
		t.Fatalf("off should disable fallback, got %v", cfg.DNSFallbackServers)
	}
}

func TestDNSFallbackRejectsHostname(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "dns.example")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "MAILMOOSE_DNS_FALLBACK_SERVERS") {
		t.Fatalf("hostname fallback server error = %v", err)
	}
}
