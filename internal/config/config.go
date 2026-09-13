package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// InboundAddr is the fixed address of the dedicated inbound webhook listener.
// It always runs alongside the main listener and serves only the authenticated
// provider ingest routes plus /healthz.
const InboundAddr = ":8082"

type Config struct {
	ListenAddr          string
	BaseURL             string
	DataDir             string
	Mode                string
	AllowRegistration   bool
	TrustProxyHeaders   bool
	TrustedProxies      []netip.Prefix
	AppEncryptionKey    string
	AdminBootstrapToken string
	MaxMessageBytes     int64
	DefaultQuotaBytes   int64
	SessionTTL          time.Duration
	RelayRequireBearer  bool
	LoginLimitPerMinute int
	SendLimitPerMinute  int
	// AllowPrivateOutbound disables the public-routable destination check for
	// outbound transports in self-hosted mode. It is always ignored in hosted
	// mode, where destinations must be public. It exists for operators who
	// intentionally send through a private gateway or local relay.
	AllowPrivateOutbound bool
	InboundConcurrency   int
	MaxMultipartParts    int
	MaxMIMEDepth         int
	MaxMIMEParts         int
	BodyReadTimeout      time.Duration
	// ApprovalExpiryHours bounds how long an external email approval request
	// stays valid. Zero disables expiry (the token lives until decided or
	// cancelled).
	ApprovalExpiryHours int
	// MXReceiveEnabled turns on the optional direct-SMTP (MX) ingress endpoints
	// on the inbound listener. App-only deployments leave it off.
	MXReceiveEnabled bool
	// MXEdgeKeys maps an operator edge key ID to its HMAC secret. MX requests
	// are authenticated by key ID, not by a self-reported edge name. Overlapping
	// keys are accepted so a credential can be rotated without downtime.
	MXEdgeKeys map[string]string
	// MXSignatureSkew bounds how old a signed MX request may be.
	MXSignatureSkew time.Duration
	// MXReceiptRetention is how long a durable MX delivery receipt is kept. It
	// must cover the supported sender retry window and expected outage recovery.
	MXReceiptRetention time.Duration
	// InboundTLSCertFile/InboundTLSKeyFile optionally serve the inbound listener
	// over TLS, so a remote MX edge can reach it over verified TLS.
	InboundTLSCertFile string
	InboundTLSKeyFile  string
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:           env("LISTEN_ADDR", ":8081"),
		BaseURL:              strings.TrimRight(env("BASE_URL", "http://localhost:8081"), "/"),
		DataDir:              env("DATA_DIR", "/data"),
		Mode:                 strings.ToLower(env("MODE", "selfhosted")),
		AllowRegistration:    envBool("ALLOW_REGISTRATION", false),
		TrustProxyHeaders:    envBool("TRUST_PROXY_HEADERS", false),
		AppEncryptionKey:     strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY")),
		AdminBootstrapToken:  strings.TrimSpace(os.Getenv("ADMIN_BOOTSTRAP_TOKEN")),
		MaxMessageBytes:      envInt64("MAX_MESSAGE_BYTES", 30<<20),
		DefaultQuotaBytes:    envInt64("DEFAULT_STORAGE_QUOTA_BYTES", 100<<20),
		SessionTTL:           time.Duration(envInt("SESSION_TTL_HOURS", 24*14)) * time.Hour,
		RelayRequireBearer:   envBool("RELAY_REQUIRE_CALLER_AUTH", false),
		LoginLimitPerMinute:  envInt("LOGIN_LIMIT_PER_MINUTE", 10),
		SendLimitPerMinute:   envInt("SEND_LIMIT_PER_MINUTE", 60),
		AllowPrivateOutbound: envBool("ALLOW_PRIVATE_OUTBOUND", false),
		InboundConcurrency:   envInt("INBOUND_CONCURRENCY", 32),
		MaxMultipartParts:    envInt("MAX_MULTIPART_PARTS", 64),
		MaxMIMEDepth:         envInt("MAX_MIME_DEPTH", 8),
		MaxMIMEParts:         envInt("MAX_MIME_PARTS", 256),
		BodyReadTimeout:      time.Duration(envInt("BODY_READ_TIMEOUT_SECONDS", 30)) * time.Second,
		ApprovalExpiryHours:  envInt("APPROVAL_EXPIRY_HOURS", 48),
		MXReceiveEnabled:     envBool("MX_RECEIVE_ENABLED", false),
		MXEdgeKeys:           parseEdgeKeys(env("MX_EDGE_KEYS", "")),
		MXSignatureSkew:      time.Duration(envInt("MX_SIGNATURE_SKEW_SECONDS", 600)) * time.Second,
		MXReceiptRetention:   time.Duration(envInt("MX_RECEIPT_RETENTION_HOURS", 7*24)) * time.Hour,
		InboundTLSCertFile:   strings.TrimSpace(os.Getenv("INBOUND_TLS_CERT_FILE")),
		InboundTLSKeyFile:    strings.TrimSpace(os.Getenv("INBOUND_TLS_KEY_FILE")),
	}
	if cfg.AppEncryptionKey == "" {
		return Config{}, fmt.Errorf("APP_ENCRYPTION_KEY is required")
	}
	if cfg.Mode != "selfhosted" && cfg.Mode != "hosted" {
		return Config{}, fmt.Errorf("MODE must be selfhosted or hosted")
	}
	if cfg.ListenAddr == InboundAddr {
		return Config{}, fmt.Errorf("LISTEN_ADDR must differ from the inbound listener %s", InboundAddr)
	}
	if cfg.MaxMessageBytes < 1<<20 {
		return Config{}, fmt.Errorf("MAX_MESSAGE_BYTES is too small")
	}
	if cfg.InboundConcurrency < 1 {
		return Config{}, fmt.Errorf("INBOUND_CONCURRENCY must be at least 1")
	}
	if cfg.MaxMultipartParts < 1 || cfg.MaxMIMEDepth < 1 || cfg.MaxMIMEParts < 1 {
		return Config{}, fmt.Errorf("MIME/multipart limits must be at least 1")
	}
	if cfg.ApprovalExpiryHours < 0 {
		return Config{}, fmt.Errorf("APPROVAL_EXPIRY_HOURS must be zero or greater")
	}
	if cfg.MXReceiveEnabled && len(cfg.MXEdgeKeys) == 0 {
		return Config{}, fmt.Errorf("MX_RECEIVE_ENABLED requires at least one MX_EDGE_KEYS entry")
	}
	if cfg.MXSignatureSkew < 0 {
		return Config{}, fmt.Errorf("MX_SIGNATURE_SKEW_SECONDS must be zero or greater")
	}
	if cfg.MXReceiptRetention <= 0 {
		return Config{}, fmt.Errorf("MX_RECEIPT_RETENTION_HOURS must be positive")
	}
	if (cfg.InboundTLSCertFile == "") != (cfg.InboundTLSKeyFile == "") {
		return Config{}, fmt.Errorf("INBOUND_TLS_CERT_FILE and INBOUND_TLS_KEY_FILE must be set together")
	}
	proxies, err := parseTrustedProxies(env("TRUSTED_PROXIES", ""))
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = proxies
	return cfg, nil
}

// parseEdgeKeys parses MX_EDGE_KEYS, a comma-separated list of
// "key_id:secret" pairs. Both halves are required; a malformed entry is
// ignored so one bad pair cannot silently disable the rest. The secret is never
// logged.
func parseEdgeKeys(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, secret, ok := strings.Cut(part, ":")
		id = strings.TrimSpace(id)
		secret = strings.TrimSpace(secret)
		if !ok || id == "" || secret == "" {
			continue
		}
		out[id] = secret
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseTrustedProxies parses a comma-separated list of IP addresses or CIDR
// networks into prefixes. A bare IP is treated as a /32 or /128.
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if addr, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXIES entry %q: %w", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// IsTrustedProxy reports whether the given remote address (host:port) is a
// configured trusted proxy. When TRUSTED_PROXIES is empty it falls back to the
// legacy TRUST_PROXY_HEADERS bool (trust everything) for backward compatibility.
func (c Config) IsTrustedProxy(remoteAddr string) bool {
	if len(c.TrustedProxies) == 0 {
		return c.TrustProxyHeaders
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	for _, p := range c.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// RequirePublicOutbound reports whether outbound transports must resolve only
// public-routable destinations. Hosted mode always enforces it; self-hosted
// mode enforces it unless the operator explicitly opts out.
func (c Config) RequirePublicOutbound() bool {
	return c.Mode == "hosted" || !c.AllowPrivateOutbound
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func envBool(name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}
func envInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return v
}
func envInt64(name string, fallback int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return fallback
	}
	return v
}
