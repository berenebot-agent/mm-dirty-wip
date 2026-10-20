// Package mxagent implements the shared policy-free SMTP edge. It stages the
// original message, computes SPF/DKIM/DMARC evidence and hands it to Delivery.
// Routing, policy, quota and durable storage live in the core.
package mxagent

import (
	"crypto/tls"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the edge's operator configuration. Secrets are read from the
// environment and never logged.
type Config struct {
	// Hostname is the SMTP greeting hostname (MX hostname).
	Hostname string
	// ListenAddr is the SMTP listener, e.g. :2525 internally, published as :25.
	ListenAddr string
	// HealthAddr is an optional health/readiness listener, e.g. :8090. Empty
	// disables it.
	HealthAddr string
	// TLSCertFile/TLSKeyFile optionally enable STARTTLS from files on disk.
	// RequireTLS refuses plaintext sessions so a deployment can guarantee
	// opportunistic senders encrypt (at the cost of bouncing those that cannot).
	TLSCertFile string
	TLSKeyFile  string
	// TLSCertificate is an in-memory STARTTLS certificate that takes precedence
	// over the file pair. The included receiver receives the PEM over the private
	// control channel and builds it here, so the child never reads the core's
	// filesystem. It is not serialised (json:"-"): it is local to the process
	// that owns it and is never sent over the wire as a struct field.
	TLSCertificate *tls.Certificate `json:"-"`
	RequireTLS     bool

	// Verification toggles. Core policy consumes whatever the edge supplies.
	VerifySPF   bool
	VerifyDKIM  bool
	VerifyDMARC bool

	// DNSResolver is an optional resolver address (host:port). Empty uses the
	// system resolver.
	DNSResolver string

	// Bounds.
	MaxMessageBytes int64
	// MaxStagingBytes caps the total bytes of messages staged in memory across
	// concurrent transactions. A new DATA that would exceed it is refused with a
	// temporary failure rather than risking an OOM kill. Staging is RAM-only:
	// the original bytes are held in memory for the duration of one transaction
	// and released as soon as the core ingest completes.
	MaxStagingBytes int64
	MaxRecipients   int
	MaxConnections  int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	DataTimeout     time.Duration
	DNSTimeout      time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Hostname:        env("MX_HOSTNAME", "localhost"),
		ListenAddr:      env("MX_LISTEN_ADDR", ":2525"),
		HealthAddr:      env("MX_HEALTH_ADDR", ""),
		TLSCertFile:     strings.TrimSpace(os.Getenv("MX_TLS_CERT")),
		TLSKeyFile:      strings.TrimSpace(os.Getenv("MX_TLS_KEY")),
		RequireTLS:      envBool("MX_REQUIRE_TLS", false),
		VerifySPF:       envBool("MX_VERIFY_SPF", true),
		VerifyDKIM:      envBool("MX_VERIFY_DKIM", true),
		VerifyDMARC:     envBool("MX_VERIFY_DMARC", true),
		DNSResolver:     strings.TrimSpace(os.Getenv("MX_DNS_RESOLVER")),
		MaxMessageBytes: envInt64("MX_MAX_MESSAGE_BYTES", 30<<20),
		MaxStagingBytes: envInt64("MX_STAGING_BYTES", 256<<20),
		MaxRecipients:   envInt("MX_MAX_RECIPIENTS", 100),
		MaxConnections:  envInt("MX_MAX_CONNECTIONS", 256),
		ReadTimeout:     time.Duration(envInt("MX_READ_TIMEOUT_SECONDS", 60)) * time.Second,
		WriteTimeout:    time.Duration(envInt("MX_WRITE_TIMEOUT_SECONDS", 60)) * time.Second,
		DataTimeout:     time.Duration(envInt("MX_DATA_TIMEOUT_SECONDS", 300)) * time.Second,
		DNSTimeout:      time.Duration(envInt("MX_DNS_TIMEOUT_SECONDS", 10)) * time.Second,
	}
	if cfg.MaxMessageBytes < 1<<20 {
		return Config{}, fmt.Errorf("MX_MAX_MESSAGE_BYTES is too small")
	}
	if cfg.MaxStagingBytes < cfg.MaxMessageBytes {
		return Config{}, fmt.Errorf("MX_STAGING_BYTES must be at least MX_MAX_MESSAGE_BYTES")
	}
	if cfg.MaxRecipients < 1 || cfg.MaxConnections < 1 {
		return Config{}, fmt.Errorf("MX_MAX_RECIPIENTS and MX_MAX_CONNECTIONS must be at least 1")
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return Config{}, fmt.Errorf("MX_TLS_CERT and MX_TLS_KEY must be set together")
	}
	if cfg.RequireTLS && cfg.TLSCertFile == "" {
		return Config{}, fmt.Errorf("MX_REQUIRE_TLS needs MX_TLS_CERT and MX_TLS_KEY so STARTTLS can be offered")
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
