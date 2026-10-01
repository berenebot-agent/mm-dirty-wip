package config_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/config"
)

// testEdgeSecret is a fixed bearer secret for private MX fixtures.
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
	if cfg.MXCoreKey != "" || cfg.MXReceiverURL != "" {
		t.Fatal("private MX connection should default empty")
	}
	if cfg.MXReceiptRetention.Hours() != 7*24 {
		t.Fatalf("receipt retention %v", cfg.MXReceiptRetention)
	}
}

func TestMXEmbeddedAutoEmbeds(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("embedded MX should provision its key automatically: %v", err)
	}
	if cfg.MXMode != config.MXLocal || !cfg.MXReceiveEnabled || !cfg.MXEmbedded {
		t.Fatalf("embedded mode not derived: mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
}

// TestMXRemotePartialEnvDoesNotBlockLoad verifies that a partial or stale MX
// environment never fails startup: after the settings are persisted the
// environment is irrelevant, so Load must accept it and the importer is the only
// place that validates it.
func TestMXRemotePartialEnvDoesNotBlockLoad(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", "")
	t.Setenv("MX_RECEIVER_URL", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("partial remote env must not block startup: %v", err)
	}
	if !cfg.MXImport.Set || cfg.MXImport.Mode != "remote" || cfg.MXImport.CoreKey != "" {
		t.Fatalf("import = %+v", cfg.MXImport)
	}
}

// TestMXImportIncludedTunables verifies the one-time import captures the legacy
// included SMTP tunables, leaving unset values at zero for the child defaults.
func TestMXImportIncludedTunables(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	t.Setenv("MX_HOSTNAME", "legacy.mx")
	t.Setenv("MX_MAX_MESSAGE_BYTES", "2097152")
	t.Setenv("MX_STAGING_BYTES", "5242880")
	t.Setenv("MX_MAX_RECIPIENTS", "40")
	t.Setenv("MX_MAX_CONNECTIONS", "90")
	t.Setenv("MX_REQUIRE_TLS", "true")
	t.Setenv("MX_VERIFY_DKIM", "false")
	t.Setenv("MX_DNS_RESOLVER", "1.1.1.1:53")
	t.Setenv("MX_DNS_TIMEOUT_SECONDS", "9")
	t.Setenv("MX_READ_TIMEOUT_SECONDS", "11")
	t.Setenv("MX_WRITE_TIMEOUT_SECONDS", "12")
	t.Setenv("MX_DATA_TIMEOUT_SECONDS", "300")
	t.Setenv("MX_TLS_CERT", "/etc/mx/cert.pem")
	t.Setenv("MX_TLS_KEY", "/etc/mx/key.pem")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	imp := cfg.MXImport
	if imp.Hostname != "legacy.mx" || imp.MaxMessageBytes != 2<<20 || imp.MaxStagingBytes != 5<<20 ||
		imp.MaxRecipients != 40 || imp.MaxConnections != 90 {
		t.Fatalf("limits = %+v", imp)
	}
	if imp.RequireTLS == nil || !*imp.RequireTLS || imp.VerifyDKIM == nil || *imp.VerifyDKIM {
		t.Fatalf("toggles = %+v", imp)
	}
	if imp.DNSResolver != "1.1.1.1:53" || imp.DNSTimeoutSeconds != 9 || imp.ReadTimeoutSeconds != 11 ||
		imp.WriteTimeoutSeconds != 12 || imp.DataTimeoutSeconds != 300 {
		t.Fatalf("dns/timeouts = %+v", imp)
	}
	if imp.TLSCertFile != "/etc/mx/cert.pem" || imp.TLSKeyFile != "/etc/mx/key.pem" {
		t.Fatalf("tls paths = %+v", imp)
	}
}

// TestMXImportUnsetIncludedTunablesAreZero verifies unset legacy tunables stay
// zero so the child defaults apply, rather than being populated with env
// defaults that would override the child.
func TestMXImportUnsetIncludedTunablesAreZero(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	imp := cfg.MXImport
	if imp.Hostname != "" || imp.MaxMessageBytes != 0 || imp.MaxStagingBytes != 0 || imp.MaxRecipients != 0 ||
		imp.MaxConnections != 0 || imp.RequireTLS != nil || imp.DNSResolver != "" || imp.TLSCertFile != "" {
		t.Fatalf("unset tunables should be zero: %+v", imp)
	}
}

func TestMXRemoteDoesNotEmbed(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", testEdgeSecret)
	t.Setenv("MX_RECEIVER_URL", "http://receiver:8443")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXMode != config.MXRemote || !cfg.MXReceiveEnabled || cfg.MXEmbedded {
		t.Fatalf("remote mode wrong: mode=%q enabled=%v embedded=%v", cfg.MXMode, cfg.MXReceiveEnabled, cfg.MXEmbedded)
	}
}

func TestMXImportDerivation(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MXImport.Set {
		t.Fatalf("no MX_ENABLE should set no import: %+v", cfg.MXImport)
	}

	t.Setenv("MX_ENABLE", "true")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MXImport.Set || cfg.MXImport.Mode != "included" || cfg.MXImport.CoreKey != "" {
		t.Fatalf("included import = %+v", cfg.MXImport)
	}

	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("MX_RECEIVER_URL", "https://receiver.example/")
	t.Setenv("DIALMX_CORE_KEY", testEdgeSecret)
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MXImport.Set || cfg.MXImport.Mode != "remote" || cfg.MXImport.ReceiverURL != "https://receiver.example" || cfg.MXImport.CoreKey != testEdgeSecret {
		t.Fatalf("remote import = %+v", cfg.MXImport)
	}
}

// TestMXReceiverURLIsNotValidatedAtLoad verifies Load no longer rejects an
// invalid legacy URL; the importer validates it only when it actually imports.
func TestMXReceiverURLIsNotValidatedAtLoad(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", testKey)
	t.Setenv("MX_ENABLE", "remote")
	t.Setenv("DIALMX_CORE_KEY", testEdgeSecret)
	t.Setenv("MX_RECEIVER_URL", "ftp://receiver")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load must not reject a legacy receiver URL: %v", err)
	}
	if cfg.MXImport.ReceiverURL != "ftp://receiver" {
		t.Fatalf("import URL = %q", cfg.MXImport.ReceiverURL)
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
