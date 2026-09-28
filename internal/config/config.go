package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// InboundAddr is the fixed address of the dedicated inbound webhook listener.
// It always runs alongside the main listener and serves only the authenticated
// provider ingest routes plus /healthz.
const InboundAddr = ":8082"

// MXMode is the user-facing direct-SMTP (MX) switch.
type MXMode string

const (
	// MXOff disables MX: no ingress endpoints, no edge.
	MXOff MXMode = "false"
	// MXLocal ("true") enables MX and embeds the edge in this container as a
	// separate-uid child process. It is the default-on form of the switch.
	MXLocal MXMode = "true"
	// MXRemote enables MX for an edge that runs as a separate container/host and
	// authenticates with operator-supplied MX_EDGE_KEYS.
	MXRemote MXMode = "remote"
)

// parseMXMode maps the MX_ENABLE value to a mode. It accepts false/off for off,
// true/local/on for the embedded edge, and remote for a separate edge; it
// rejects anything unrecognised so a typo does not silently disable MX.
func parseMXMode(raw string) (MXMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "false", "off", "0", "no":
		return MXOff, nil
	case "true", "local", "on", "1", "yes":
		return MXLocal, nil
	case "remote":
		return MXRemote, nil
	default:
		return MXOff, fmt.Errorf("MX_ENABLE must be false, true or remote, got %q", raw)
	}
}

type Config struct {
	ListenAddr string
	BaseURL    string
	// baseHost is the canonical host (with explicit port) parsed from BaseURL.
	// It is the only host the HTTPS redirect may target; request Host headers
	// are attacker-controlled and never used for redirects.
	baseHost          string
	DataDir           string
	Mode              string
	AllowRegistration bool
	TrustProxyHeaders bool
	TrustedProxies    []netip.Prefix
	// ForceHTTPS redirects plaintext requests to https and reports https in
	// discovery documents. It is for deployments that terminate TLS at a
	// reverse proxy and never want the app to answer over cleartext.
	ForceHTTPS       bool
	AppEncryptionKey string
	// AdminEmail/AdminPassword are the configured system-administrator
	// credentials. When present they are authoritative: on every start the
	// stored system administrator is reconciled to them, rotating its login
	// (and revoking its sessions) if either changed. When both are absent the
	// stored system administrator is left untouched, so a deployment can omit
	// them once provisioned. Supplying only one is a startup error. Never log
	// the password.
	AdminEmail    string
	AdminPassword string
	// AdminAccountName is the display name for the system administrator's own
	// account. Defaults to "MailMoose". It is only used when the system
	// administrator is first created.
	AdminAccountName       string
	MaxMessageBytes        int64
	DefaultQuotaBytes      int64
	SessionTTL             time.Duration
	RelayRequireBearer     bool
	LoginLimitPerMinute    int
	SendLimitPerMinute     int
	RegisterLimitPerMinute int
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
	WebhookRetryWindow  time.Duration
	// MXMode selects the optional direct-SMTP (MX) deployment: off, true (edge
	// embedded in this container as a separate-uid child) or remote (edge runs
	// as a separate container/host and shares MX_EDGE_KEYS). It is the single
	// user-facing MX switch; MXReceiveEnabled/MXEmbedded are derived.
	MXMode MXMode
	// MXReceiveEnabled turns on the direct-SMTP ingress endpoints on the inbound
	// listener. Derived: true for embedded and remote.
	MXReceiveEnabled bool
	// MXEdgeKeys maps an operator edge key ID to its HMAC secret. MX requests
	// are authenticated by key ID, not by a self-reported edge name. Overlapping
	// keys are accepted so a credential can be rotated without downtime. In
	// embedded mode the key may be generated; remote mode requires it.
	MXEdgeKeys map[string]string
	// MXSignatureSkew bounds how old a signed MX request may be.
	MXSignatureSkew time.Duration
	// MXReceiptRetention is how long a durable MX delivery receipt is kept. It
	// must cover the supported sender retry window and expected outage recovery.
	MXReceiptRetention time.Duration
	// MXEmbedded is true only in embedded mode: cmd/server spawns the edge as a
	// child under MXUID/MXGID and drops privileges. Remote mode leaves it false.
	MXEmbedded bool
	// MXUID/MXGID are the uid/gid the embedded edge process runs as, separate
	// from the app's runtime user, so the edge cannot read /data or the app's
	// encryption key.
	MXUID int
	MXGID int
	// InboundTLSCertFile/InboundTLSKeyFile optionally serve the inbound listener
	// over TLS, so a remote MX edge can reach it over verified TLS.
	InboundTLSCertFile string
	InboundTLSKeyFile  string
}

func Load() (Config, error) {
	mxMode, err := parseMXMode(env("MX_ENABLE", "false"))
	if err != nil {
		return Config{}, err
	}
	mxEdgeKeys, err := parseEdgeKeys(env("MX_EDGE_KEYS", ""))
	if err != nil {
		return Config{}, err
	}
	admin, err := loadAdmin()
	if err != nil {
		return Config{}, err
	}
	baseURL := strings.TrimRight(env("BASE_URL", "http://localhost:8081"), "/")
	baseHost, err := parseBaseHost(baseURL)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:             env("LISTEN_ADDR", ":8081"),
		BaseURL:                baseURL,
		baseHost:               baseHost,
		DataDir:                env("DATA_DIR", "/data"),
		Mode:                   strings.ToLower(env("MODE", "selfhosted")),
		AllowRegistration:      envBool("ALLOW_REGISTRATION", false),
		TrustProxyHeaders:      envBool("TRUST_PROXY_HEADERS", false),
		ForceHTTPS:             envBool("FORCE_HTTPS", false),
		AppEncryptionKey:       strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY")),
		AdminEmail:             admin.email,
		AdminPassword:          admin.password,
		AdminAccountName:       admin.accountName,
		MaxMessageBytes:        envInt64("MAX_MESSAGE_BYTES", 30<<20),
		DefaultQuotaBytes:      envInt64("DEFAULT_STORAGE_QUOTA_BYTES", 100<<20),
		SessionTTL:             time.Duration(envInt("SESSION_TTL_HOURS", 24*14)) * time.Hour,
		RelayRequireBearer:     envBool("RELAY_REQUIRE_CALLER_AUTH", false),
		LoginLimitPerMinute:    envInt("LOGIN_LIMIT_PER_MINUTE", 10),
		SendLimitPerMinute:     envInt("SEND_LIMIT_PER_MINUTE", 60),
		RegisterLimitPerMinute: envInt("REGISTER_LIMIT_PER_MINUTE", 5),
		AllowPrivateOutbound:   envBool("ALLOW_PRIVATE_OUTBOUND", false),
		InboundConcurrency:     envInt("INBOUND_CONCURRENCY", 32),
		MaxMultipartParts:      envInt("MAX_MULTIPART_PARTS", 64),
		MaxMIMEDepth:           envInt("MAX_MIME_DEPTH", 8),
		MaxMIMEParts:           envInt("MAX_MIME_PARTS", 256),
		BodyReadTimeout:        time.Duration(envInt("BODY_READ_TIMEOUT_SECONDS", 30)) * time.Second,
		ApprovalExpiryHours:    envInt("APPROVAL_EXPIRY_HOURS", 48),
		WebhookRetryWindow:     time.Duration(envInt("WEBHOOK_RETRY_WINDOW_DAYS", 7)) * 24 * time.Hour,
		MXMode:                 mxMode,
		MXReceiveEnabled:       mxMode != MXOff,
		MXEmbedded:             mxMode == MXLocal,
		MXEdgeKeys:             mxEdgeKeys,
		MXSignatureSkew:        time.Duration(envInt("MX_SIGNATURE_SKEW_SECONDS", 600)) * time.Second,
		MXReceiptRetention:     time.Duration(envInt("MX_RECEIPT_RETENTION_HOURS", 7*24)) * time.Hour,
		MXUID:                  envInt("MX_UID", 65533),
		MXGID:                  envInt("MX_GID", 65533),
		InboundTLSCertFile:     strings.TrimSpace(os.Getenv("INBOUND_TLS_CERT_FILE")),
		InboundTLSKeyFile:      strings.TrimSpace(os.Getenv("INBOUND_TLS_KEY_FILE")),
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
	// embedded mode auto-generates an edge credential; remote mode shares an
	// operator secret and must be given one.
	if cfg.MXMode == MXRemote && len(cfg.MXEdgeKeys) == 0 {
		return Config{}, fmt.Errorf("MX_ENABLE=remote requires at least one MX_EDGE_KEYS entry")
	}
	if cfg.MXEmbedded && (cfg.MXUID <= 0 || cfg.MXGID <= 0) {
		return Config{}, fmt.Errorf("MX_UID and MX_GID must be positive non-zero integers")
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
	if cfg.ForceHTTPS && !strings.HasPrefix(strings.ToLower(cfg.BaseURL), "https://") {
		return Config{}, fmt.Errorf("FORCE_HTTPS=true requires BASE_URL=https://... so the redirect target is canonical")
	}
	proxies, err := parseTrustedProxies(env("TRUSTED_PROXIES", ""))
	if err != nil {
		return Config{}, err
	}
	// A trust set that matches every caller (legacy trust-all, or a /0 range)
	// makes the peer check meaningless and lets any client choose its own
	// rate-limit identity and scheme via forwarded headers. It is always a
	// misconfiguration, so refuse it at startup rather than run insecurely.
	if cfg.TrustProxyHeaders {
		return Config{}, fmt.Errorf("TRUST_PROXY_HEADERS=true trusts proxy headers from every caller; set TRUSTED_PROXIES to the proxy ranges instead")
	}
	for _, p := range proxies {
		if p.Bits() == 0 {
			return Config{}, fmt.Errorf("TRUSTED_PROXIES entry %q trusts every caller; list the proxy ranges instead", p.String())
		}
		// A prefix this short is not a proxy address in any real topology and
		// almost certainly covers ordinary clients too. Refusing at load is the
		// only point where a deliberately-wide covering set can be caught, since
		// startup cannot know which addresses will actually connect.
		if p.Bits() < minTrustedProxyBits {
			return Config{}, fmt.Errorf("TRUSTED_PROXIES entry %q is wider than /%d and would trust ordinary clients; list the specific proxy addresses", p.String(), minTrustedProxyBits)
		}
	}
	cfg.TrustedProxies = proxies
	return cfg, nil
}

// parseEdgeKeys parses MX_EDGE_KEYS, a comma-separated list of
// "key_id:secret" pairs. Both halves are required; a malformed entry is
// ignored so one bad pair cannot silently disable the rest. Every
// operator-supplied secret must meet the mxwire entropy bar (32 bytes / 256
// bits); a weak secret is a startup error, never silently accepted, because it
// authenticates the edge and the core trusts edge auth evidence on a valid
// signature. The secret is never logged or included in the error.
func parseEdgeKeys(raw string) (map[string]string, error) {
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
		if err := mxwire.CheckEdgeSecret(secret); err != nil {
			return nil, fmt.Errorf("MX_EDGE_KEYS entry %q too weak: need 32 bytes of entropy (generate: openssl rand -hex 32)", id)
		}
		out[id] = secret
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// adminConfig carries the configured system-administrator credentials.
type adminConfig struct {
	email       string
	password    string
	accountName string
}

// loadAdmin resolves ADMIN_EMAIL / ADMIN_PASSWORD (either may come from a
// *_FILE variant) and validates them. Supplying only one half is a startup
// error: a half-configured administrator must never start and later be
// mistaken for "no credentials", which would silently keep an old login.
// Supplying neither is valid: the stored system administrator (if any) is left
// untouched. The password is never included in an error.
func loadAdmin() (adminConfig, error) {
	email, emailSet, err := envSecret("ADMIN_EMAIL")
	if err != nil {
		return adminConfig{}, err
	}
	password, passwordSet, err := envSecret("ADMIN_PASSWORD")
	if err != nil {
		return adminConfig{}, err
	}
	if emailSet != passwordSet {
		return adminConfig{}, fmt.Errorf("ADMIN_EMAIL and ADMIN_PASSWORD must be supplied together")
	}
	if emailSet {
		addr, perr := mail.ParseAddress(email)
		if perr != nil || addr.Address != email {
			return adminConfig{}, fmt.Errorf("ADMIN_EMAIL must be a plain email address")
		}
		if err := auth.ValidatePassword(password); err != nil {
			return adminConfig{}, fmt.Errorf("ADMIN_PASSWORD %w", err)
		}
	}
	name := strings.TrimSpace(env("ADMIN_ACCOUNT_NAME", "MailMoose"))
	if name == "" {
		name = "MailMoose"
	}
	return adminConfig{email: email, password: password, accountName: name}, nil
}

// envSecret resolves a secret from either NAME or NAME_FILE. The two forms are
// mutually exclusive: setting both is a startup error so the effective value
// is never ambiguous. A file is read as text with surrounding whitespace
// trimmed (so a trailing newline is not part of the secret).
func envSecret(name string) (string, bool, error) {
	direct := strings.TrimSpace(os.Getenv(name))
	path := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if direct != "" && path != "" {
		return "", false, fmt.Errorf("%s and %s_FILE must not both be set", name, name)
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", false, fmt.Errorf("cannot read %s_FILE: %w", name, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", false, fmt.Errorf("%s_FILE is empty", name)
		}
		return v, true, nil
	}
	if direct != "" {
		return direct, true, nil
	}
	return "", false, nil
}

// parseBaseHost extracts the canonical redirect host from BASE_URL, rejecting
// values that cannot serve as one (missing scheme/host, userinfo, path, query
// or fragment). Request Host headers are attacker-controlled, so the HTTPS
// redirect targets only this host.
func parseBaseHost(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("BASE_URL must be an absolute http(s) URL with a host, got %q", baseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("BASE_URL must use http or https, got %q", baseURL)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("BASE_URL must be a bare origin (scheme://host[:port]), got %q", baseURL)
	}
	return u.Host, nil
}

// BaseHost returns the canonical host (with explicit port) parsed from
// BASE_URL. It is the only host the HTTPS redirect may target.
func (c Config) BaseHost() string {
	if c.baseHost != "" {
		return c.baseHost
	}
	// Struct-literal configs in tests bypass Load: derive best-effort so the
	// redirect still has a canonical host instead of trusting r.Host.
	if h, err := parseBaseHost(c.BaseURL); err == nil {
		return h
	}
	return ""
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

// minTrustedProxyBits is the narrowest prefix length accepted for a
// TRUSTED_PROXIES entry. Anything wider is refused at load (see Load): a
// /1 or similar is not a proxy address in any real topology, and trusting it
// would let ordinary clients set their own rate-limit identity and scheme.
const minTrustedProxyBits = 8

// IsTrustedProxy reports whether the given remote address (host:port) is a
// configured trusted proxy. When TRUSTED_PROXIES is empty it falls back to the
// TRUST_PROXY_HEADERS bool; config.Load refuses TRUST_PROXY_HEADERS=true, so
// that path is reachable only from an in-process config that sets the field
// directly.
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
			// A peer inside the trust set is the normal case: the request
			// arrived through a configured proxy. Load refuses entries wider
			// than minTrustedProxyBits, so a match is not by itself evidence of
			// an over-covering set, and logging it on the per-request path only
			// reported that the proxy is working.
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
