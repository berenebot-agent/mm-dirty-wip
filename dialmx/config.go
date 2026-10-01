package dialmx

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// Config is the standalone Dial MX receiver configuration. The HTTPS/2 session
// listener is the receiver's own; the SMTP edge, verification toggles and DNS
// resolver reuse the operator's existing MX_* environment via mxagent.Config.
// Unlike the relay edge, the standalone receiver needs no HMAC ingest URL/secret:
// it hands Delivery straight to the in-process receiver.
type Config struct {
	ListenAddr, TLSCertFile, TLSKeyFile string
	SMTP                                mxagent.Config
	Receiver                            receiver.Config
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:  env("DIALMX_LISTEN_ADDR", ":8443"),
		TLSCertFile: strings.TrimSpace(os.Getenv("DIALMX_TLS_CERT")),
		TLSKeyFile:  strings.TrimSpace(os.Getenv("DIALMX_TLS_KEY")),
	}
	// The SMTP edge keeps the documented MX_* environment. IngestURL, KeyID and
	// Secret are deliberately left empty: the receiver wires Delivery in process.
	c.SMTP = mxagent.Config{
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
	r := &c.Receiver
	r.SMTP = c.SMTP
	r.MaxDomainsPerConnection = envInt("MX_MAX_DOMAINS_PER_CONNECTION", 128)
	r.MaxTransactionsPerConnection = envInt("MX_MAX_TRANSACTIONS_PER_CONNECTION", 16)
	r.MaxTransactions = envInt("MX_MAX_TRANSACTIONS", 128)
	r.MaxTransactionsPerDomain = envInt("MX_MAX_TRANSACTIONS_PER_DOMAIN", 8)
	r.AuthTimeout = time.Duration(envInt("MX_AUTH_TIMEOUT_SECONDS", 10)) * time.Second
	r.ResolveTimeout = time.Duration(envInt("MX_RESOLVE_TIMEOUT_SECONDS", 10)) * time.Second
	r.IngestTimeout = time.Duration(envInt("MX_INGEST_TIMEOUT_SECONDS", 180)) * time.Second
	r.RevalidateInterval = time.Duration(envInt("MX_REVALIDATE_SECONDS", 240)) * time.Second
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// validate enforces the operator surface strictly. Every bound must be present
// and positive, the staging budget must admit one oversize-detecting
// reservation (MaxStagingBytes >= MaxMessageBytes+1, matching mxagent's own
// floor), the message cap must be at least 1 MiB, and the listener certificate
// pair must be set together.
func (c *Config) validate() error {
	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return fmt.Errorf("DIALMX_TLS_CERT and DIALMX_TLS_KEY are required")
	}
	s := c.SMTP
	if s.MaxMessageBytes < 1<<20 {
		return fmt.Errorf("MX_MAX_MESSAGE_BYTES is too small")
	}
	if s.MaxStagingBytes < s.MaxMessageBytes+1 {
		return fmt.Errorf("MX_STAGING_BYTES must be at least MX_MAX_MESSAGE_BYTES+1")
	}
	if s.MaxRecipients < 1 || s.MaxConnections < 1 {
		return fmt.Errorf("MX_MAX_RECIPIENTS and MX_MAX_CONNECTIONS must be at least 1")
	}
	if s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.DataTimeout <= 0 || s.DNSTimeout <= 0 {
		return fmt.Errorf("MX timeouts must be positive")
	}
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		return fmt.Errorf("MX_TLS_CERT and MX_TLS_KEY must be set together")
	}
	if s.RequireTLS && s.TLSCertFile == "" {
		return fmt.Errorf("MX_REQUIRE_TLS needs MX_TLS_CERT and MX_TLS_KEY")
	}
	r := c.Receiver
	if r.MaxDomainsPerConnection < 1 || r.MaxTransactions < 1 || r.MaxTransactionsPerConnection < 1 || r.MaxTransactionsPerDomain < 1 {
		return fmt.Errorf("MX domain and transaction limits must be at least 1")
	}
	if r.MaxTransactionsPerConnection > s.MaxConnections {
		return fmt.Errorf("MX_MAX_TRANSACTIONS_PER_CONNECTION exceeds MX_MAX_CONNECTIONS")
	}
	if r.AuthTimeout <= 0 || r.ResolveTimeout <= 0 || r.IngestTimeout <= 0 || r.RevalidateInterval <= 0 || r.RevalidateInterval > 4*time.Minute {
		return fmt.Errorf("MX receiver timeouts must be positive")
	}
	return nil
}

func env(k, d string) string {
	if s := strings.TrimSpace(os.Getenv(k)); s != "" {
		return s
	}
	return d
}

func envBool(k string, d bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return d
}

func envInt(k string, d int) int {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return d
	}
	n, e := strconv.Atoi(raw)
	if e != nil {
		return -1
	}
	return n
}

func envInt64(k string, d int64) int64 {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return d
	}
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil {
		return -1
	}
	return n
}
