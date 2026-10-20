package dialmx_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/dialmx"
)

// env applies a complete valid environment, then the caller overrides one key.
func loadWith(t *testing.T, overrides map[string]string) (dialmx.Config, error) {
	t.Helper()
	base := map[string]string{
		"DIALMX_MODE":                        "shared",
		"DIALMX_TLS_CERT":                    "/tls/cert.pem",
		"DIALMX_TLS_KEY":                     "/tls/key.pem",
		"MX_MAX_MESSAGE_BYTES":               "1048576",
		"MX_STAGING_BYTES":                   "2097152",
		"MX_MAX_RECIPIENTS":                  "100",
		"MX_MAX_CONNECTIONS":                 "64",
		"MX_MAX_DOMAINS_PER_CONNECTION":      "128",
		"MX_MAX_TRANSACTIONS_PER_CONNECTION": "16",
		"MX_MAX_TRANSACTIONS_PER_DOMAIN":     "8",
		"MX_AUTH_TIMEOUT_SECONDS":            "10",
		"MX_RESOLVE_TIMEOUT_SECONDS":         "10",
		"MX_INGEST_TIMEOUT_SECONDS":          "180",
		"MX_REVALIDATE_SECONDS":              "240",
		"MX_READ_TIMEOUT_SECONDS":            "60",
		"MX_WRITE_TIMEOUT_SECONDS":           "60",
		"MX_DATA_TIMEOUT_SECONDS":            "300",
		"MX_DNS_TIMEOUT_SECONDS":             "10",
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range overrides {
		t.Setenv(k, v)
	}
	// Clear variables a previous test may have set.
	t.Setenv("MX_TLS_CERT", overrides["MX_TLS_CERT"])
	t.Setenv("MX_TLS_KEY", overrides["MX_TLS_KEY"])
	return dialmx.Load()
}

func TestLoadAcceptsValidConfig(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Receiver.SMTP.MaxMessageBytes != 1<<20 {
		t.Fatalf("message bound not propagated to receiver SMTP: %d", cfg.Receiver.SMTP.MaxMessageBytes)
	}
	if cfg.ListenAddr == "" {
		t.Fatal("listen address missing")
	}
}

func TestLoadRejectsBadNumbers(t *testing.T) {
	for name, overrides := range map[string]map[string]string{
		"message too small":  {"MX_MAX_MESSAGE_BYTES": "1048575"},
		"staging too small":  {"MX_STAGING_BYTES": "1048576"},
		"zero recipients":    {"MX_MAX_RECIPIENTS": "0"},
		"zero connections":   {"MX_MAX_CONNECTIONS": "0"},
		"zero domains":       {"MX_MAX_DOMAINS_PER_CONNECTION": "0"},
		"zero tx per conn":   {"MX_MAX_TRANSACTIONS_PER_CONNECTION": "0"},
		"zero tx per domain": {"MX_MAX_TRANSACTIONS_PER_DOMAIN": "0"},
		"negative staging":   {"MX_STAGING_BYTES": "-1"},
		"invalid number":     {"MX_MAX_CONNECTIONS": "nope"},
		"late renewal":       {"MX_REVALIDATE_SECONDS": "300"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadWith(t, overrides); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadRejectsNonPositiveTimeouts(t *testing.T) {
	for _, key := range []string{
		"MX_AUTH_TIMEOUT_SECONDS", "MX_RESOLVE_TIMEOUT_SECONDS", "MX_INGEST_TIMEOUT_SECONDS",
		"MX_REVALIDATE_SECONDS", "MX_READ_TIMEOUT_SECONDS", "MX_WRITE_TIMEOUT_SECONDS",
		"MX_DATA_TIMEOUT_SECONDS", "MX_DNS_TIMEOUT_SECONDS",
	} {
		t.Run(key, func(t *testing.T) {
			if _, err := loadWith(t, map[string]string{key: "0"}); err == nil {
				t.Fatalf("expected %s=0 to be rejected", key)
			}
		})
	}
}

func TestLoadRejectsUnpairedCertificates(t *testing.T) {
	if _, err := loadWith(t, map[string]string{"DIALMX_TLS_CERT": ""}); err == nil || !strings.Contains(err.Error(), "DIALMX_TLS") {
		t.Fatalf("expected unpaired listener cert error, got %v", err)
	}
	if _, err := loadWith(t, map[string]string{"MX_TLS_CERT": "cert.pem"}); err == nil {
		t.Fatal("expected unpaired MX cert error")
	}
}

func TestLoadRejectsTransactionLimitAboveConnections(t *testing.T) {
	if _, err := loadWith(t, map[string]string{"MX_MAX_TRANSACTIONS_PER_CONNECTION": "200", "MX_MAX_CONNECTIONS": "64"}); err == nil {
		t.Fatal("expected per-connection transaction cap above connection cap to be rejected")
	}
}

func TestLoadRequiresListenerCertificates(t *testing.T) {
	t.Setenv("DIALMX_MODE", "shared")
	t.Setenv("DIALMX_TLS_CERT", "")
	t.Setenv("DIALMX_TLS_KEY", "")
	t.Setenv("DIALMX_TRUSTED_PROXIES", "")
	if _, err := dialmx.Load(); err == nil {
		t.Fatal("expected missing listener certificates to be rejected")
	}
}

func TestSharedCleartextRequiresTrustedProxies(t *testing.T) {
	// No cert pair and no allowlist: strict shared TLS is retained.
	if _, err := loadWith(t, map[string]string{"DIALMX_TLS_CERT": "", "DIALMX_TLS_KEY": "", "DIALMX_TRUSTED_PROXIES": ""}); err == nil {
		t.Fatal("expected shared cleartext without trusted proxies to be rejected")
	}
	// A non-empty allowlist admits a cleartext shared listener.
	cfg, err := loadWith(t, map[string]string{"DIALMX_TLS_CERT": "", "DIALMX_TLS_KEY": "", "DIALMX_TRUSTED_PROXIES": "10.1.1.18"})
	if err != nil {
		t.Fatalf("shared cleartext with trusted proxies rejected: %v", err)
	}
	if len(cfg.Receiver.TrustedProxies) != 1 {
		t.Fatalf("trusted proxies not propagated: %+v", cfg.Receiver.TrustedProxies)
	}
}

func TestLoadRejectsBadTrustedProxies(t *testing.T) {
	for name, value := range map[string]string{
		"garbage":     "not-an-ip",
		"too wide v4": "10.0.0.0/1",
		"too wide v6": "2001:db8::/1",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWith(t, map[string]string{"DIALMX_TLS_CERT": "", "DIALMX_TLS_KEY": "", "DIALMX_TRUSTED_PROXIES": value})
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestSharedCleartextPerSourceLimits(t *testing.T) {
	base := map[string]string{"DIALMX_TLS_CERT": "", "DIALMX_TLS_KEY": "", "DIALMX_TRUSTED_PROXIES": "10.1.1.18"}
	cfg, err := loadWith(t, base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Receiver.MaxConnsPerIP != 16 || cfg.Receiver.ConnWindowMax != 128 || cfg.Receiver.MaxAuthConcurrent != 16 || cfg.Receiver.AuthWindowMax != 256 {
		t.Fatalf("per-source defaults not applied: %+v", cfg.Receiver)
	}
	over := map[string]string{}
	for k, v := range base {
		over[k] = v
	}
	over["MX_PER_IP_CONN_LIMIT"] = "4"
	over["MX_PER_IP_AUTH_CONCURRENT"] = "2"
	cfg, err = loadWith(t, over)
	if err != nil {
		t.Fatalf("load override: %v", err)
	}
	if cfg.Receiver.MaxConnsPerIP != 4 || cfg.Receiver.MaxAuthConcurrent != 2 {
		t.Fatalf("per-source overrides not applied: %+v", cfg.Receiver)
	}
	for _, key := range []string{"MX_PER_IP_CONN_LIMIT", "MX_PER_IP_CONN_WINDOW_MAX", "MX_PER_IP_AUTH_CONCURRENT", "MX_PER_IP_AUTH_WINDOW_MAX"} {
		t.Run(key, func(t *testing.T) {
			bad := map[string]string{}
			for k, v := range base {
				bad[k] = v
			}
			bad[key] = "0"
			if _, err := loadWith(t, bad); err == nil {
				t.Fatalf("expected %s=0 to be rejected", key)
			}
		})
	}
}

func TestSingleModeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		valid  bool
	}{
		{"default cleartext", map[string]string{"DIALMX_MODE": "", "DIALMX_CORE_KEY": "private-key", "DIALMX_TLS_CERT": "", "DIALMX_TLS_KEY": ""}, true},
		{"TLS", map[string]string{"DIALMX_MODE": "single", "DIALMX_CORE_KEY": "private-key"}, true},
		{"missing key", map[string]string{"DIALMX_MODE": "single", "DIALMX_CORE_KEY": ""}, false},
		{"unknown mode", map[string]string{"DIALMX_MODE": "other"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, tc.values)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if tc.valid && cfg.Receiver.Mode != "single" {
				t.Fatalf("mode=%q", cfg.Receiver.Mode)
			}
		})
	}
}

// TestLoadBrowserRedirect proves the optional browser landing redirect accepts
// only a bounded https target and is carried onto the receiver configuration.
func TestLoadBrowserRedirect(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{"DIALMX_BROWSER_REDIRECT_URL": "https://github.com/dellarb/mailmoose"})
	if err != nil {
		t.Fatalf("valid redirect rejected: %v", err)
	}
	if cfg.Receiver.BrowserRedirectURL != "https://github.com/dellarb/mailmoose" {
		t.Fatalf("redirect not carried: %q", cfg.Receiver.BrowserRedirectURL)
	}
	for _, bad := range []string{"http://example.com", "https://example.com/#frag", "not-a-url", "https://"} {
		if _, err := loadWith(t, map[string]string{"DIALMX_BROWSER_REDIRECT_URL": bad}); err == nil {
			t.Fatalf("redirect %q accepted", bad)
		}
	}
}
