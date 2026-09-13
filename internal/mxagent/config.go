// Package mxagent implements the optional policy-free SMTP edge (cmd/mx). The
// edge terminates SMTP on port 25, strictly frames and stages the original
// message, computes SPF/DKIM/DMARC evidence, and hands one signed ingest
// request per accepted recipient to the core. It holds no domain/policy
// snapshot, no database access and no application encryption key: routing,
// policy, quota and durable storage all live in the core behind authenticated
// endpoints.
package mxagent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the edge's operator configuration. Secrets are read from the
// environment and never logged.
type Config struct {
	// IngestURL is the base URL of the core's inbound connector, e.g.
	// http://gatehouse:8082.
	IngestURL string
	// KeyID and Secret authenticate signed requests to the core.
	KeyID  string
	Secret string
	// EdgeName identifies this node in logs and signed metadata.
	EdgeName string
	// Hostname is the SMTP greeting hostname (MX hostname).
	Hostname string
	// ListenAddr is the SMTP listener, e.g. :2525 internally, published as :25.
	ListenAddr string
	// HealthAddr is an optional health/readiness listener, e.g. :8090. Empty
	// disables it.
	HealthAddr string
	// TLSCertFile/TLSKeyFile optionally enable STARTTLS.
	TLSCertFile string
	TLSKeyFile  string

	// Verification toggles. Core policy consumes whatever the edge supplies.
	VerifySPF   bool
	VerifyDKIM  bool
	VerifyDMARC bool

	// DNSResolver is an optional resolver address (host:port). Empty uses the
	// system resolver.
	DNSResolver string

	// Bounds.
	MaxMessageBytes int64
	MaxRecipients   int
	MaxConnections  int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	DataTimeout     time.Duration
	DNSTimeout      time.Duration
	StagingDir      string
}

func Load() (Config, error) {
	cfg := Config{
		IngestURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("GATEHOUSE_INGEST_URL")), "/"),
		KeyID:           strings.TrimSpace(os.Getenv("MX_EDGE_KEY_ID")),
		Secret:          strings.TrimSpace(os.Getenv("MX_EDGE_SECRET")),
		EdgeName:        env("MX_EDGE_NAME", "mx-1"),
		Hostname:        env("MX_HOSTNAME", "localhost"),
		ListenAddr:      env("MX_LISTEN_ADDR", ":2525"),
		HealthAddr:      env("MX_HEALTH_ADDR", ""),
		TLSCertFile:     strings.TrimSpace(os.Getenv("MX_TLS_CERT")),
		TLSKeyFile:      strings.TrimSpace(os.Getenv("MX_TLS_KEY")),
		VerifySPF:       envBool("MX_VERIFY_SPF", true),
		VerifyDKIM:      envBool("MX_VERIFY_DKIM", true),
		VerifyDMARC:     envBool("MX_VERIFY_DMARC", true),
		DNSResolver:     strings.TrimSpace(os.Getenv("MX_DNS_RESOLVER")),
		MaxMessageBytes: envInt64("MX_MAX_MESSAGE_BYTES", 30<<20),
		MaxRecipients:   envInt("MX_MAX_RECIPIENTS", 100),
		MaxConnections:  envInt("MX_MAX_CONNECTIONS", 256),
		ReadTimeout:     time.Duration(envInt("MX_READ_TIMEOUT_SECONDS", 60)) * time.Second,
		WriteTimeout:    time.Duration(envInt("MX_WRITE_TIMEOUT_SECONDS", 60)) * time.Second,
		DataTimeout:     time.Duration(envInt("MX_DATA_TIMEOUT_SECONDS", 300)) * time.Second,
		DNSTimeout:      time.Duration(envInt("MX_DNS_TIMEOUT_SECONDS", 10)) * time.Second,
		StagingDir:      env("MX_STAGING_DIR", "/tmp/gatehouse-mx"),
	}
	if cfg.IngestURL == "" {
		return Config{}, fmt.Errorf("GATEHOUSE_INGEST_URL is required")
	}
	if cfg.KeyID == "" || cfg.Secret == "" {
		return Config{}, fmt.Errorf("MX_EDGE_KEY_ID and MX_EDGE_SECRET are required")
	}
	if cfg.MaxMessageBytes < 1<<20 {
		return Config{}, fmt.Errorf("MX_MAX_MESSAGE_BYTES is too small")
	}
	if cfg.MaxRecipients < 1 || cfg.MaxConnections < 1 {
		return Config{}, fmt.Errorf("MX_MAX_RECIPIENTS and MX_MAX_CONNECTIONS must be at least 1")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return Config{}, fmt.Errorf("MX_TLS_CERT and MX_TLS_KEY must be set together")
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func envBool(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return v
	}
	return fallback
}
func envInt64(name string, fallback int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64); err == nil {
		return v
	}
	return fallback
}
