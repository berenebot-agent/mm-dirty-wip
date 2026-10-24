package config

import (
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/dnsfallback"
)

// InboundAddr is the default address of the dedicated inbound webhook listener.
const InboundAddr = ":8082"

type Config struct {
	ListenAddr              string
	BaseURL                 string
	DedicatedReceiverEnable bool
	DedicatedReceiverPort   int
	DedicatedReceiverURL    string
	MicrosoftClientID       string
	MicrosoftTenantID       string
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
	ForceHTTPS bool
	// NoIndex asks search and AI crawlers to stay out of the human UI. When
	// set, HTML pages carry X-Robots-Tag and a robots meta element, and
	// /robots.txt disallows everything. It is off by default: the hosted
	// deployment wants its signup pages discoverable, and a self-hoster opts
	// in. It never covers the REST API or the discovery document, which agents
	// are meant to find.
	NoIndex          bool
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
	AdminAccountName  string
	MaxMessageBytes   int64
	DefaultQuotaBytes int64
	SessionTTL        time.Duration
	// RelayRequireBearer (RELAY_REQUIRE_CALLER_AUTH) requires a relay enrolment
	// request to present an Authorization header with a "Bearer " prefix. It
	// checks only that such a header is present; the credential's value is not
	// validated, so this is a minimal deterrent, not authentication. The
	// single-use enrolment token remains the authority for enrolment.
	RelayRequireBearer     bool
	LoginLimitPerMinute    int
	SendLimitPerMinute     int
	RegisterLimitPerMinute int
	// AllowPrivateOutbound disables the public-routable destination check for
	// outbound transports. It defaults to true because self-hosting is the
	// primary model, where sending through a private gateway, local relay, or
	// LAN MX receiver is normal. A hosted operator that must confine outbound
	// traffic to the public internet sets ALLOW_PRIVATE_OUTBOUND=false; that
	// policy then also governs the per-account Remote MX receiver, so a tenant
	// cannot re-enable private destinations on their own.
	AllowPrivateOutbound bool
	InboundConcurrency   int
	MaxMultipartParts    int
	MaxMIMEDepth         int
	MaxMIMEParts         int
	BodyReadTimeout      time.Duration
	// OutboundHTTPTimeout bounds one outbound provider HTTP request in full:
	// connect, upload of the (possibly attachment-heavy) body, and the wait for
	// response headers. A short bound aborts large attachment sends that a slow
	// uplink cannot complete, and a timeout while awaiting headers can follow a
	// send the provider already accepted.
	OutboundHTTPTimeout time.Duration
	// OutboundConcurrency is the number of outbox deliveries sent at once. Each
	// in-flight send holds its attachments in memory, so this bounds peak memory
	// as well as parallelism. Values below 1 behave as 1.
	OutboundConcurrency   int
	RemoteSyncConcurrency int
	// ApprovalExpiryHours bounds how long an external email approval request
	// stays valid. Zero disables expiry (the token lives until decided or
	// cancelled).
	ApprovalExpiryHours int
	WebhookRetryWindow  time.Duration
	// MXReceiptRetention is how long a durable MX delivery receipt is kept. It
	// must cover the supported sender retry window and expected outage recovery.
	MXReceiptRetention time.Duration
	// MXUID/MXGID are the uid/gid the included edge child runs as, separate
	// from the app's runtime user, so the edge cannot read /data or the app's
	// encryption key. The receiver mode itself comes from the persisted
	// mx_settings row, not the environment.
	MXUID int
	MXGID int
	// InboundTLSCertFile/InboundTLSKeyFile optionally serve the inbound listener
	// over TLS, so a remote MX edge can reach it over verified TLS.
	InboundTLSCertFile string
	InboundTLSKeyFile  string
	// DialMXCAFile optionally adds private receiver CAs to the system trust roots.
	DialMXCAFile string
	// DNSFallbackServers are the ordered public resolvers tried when the host's
	// configured DNS servers fail an exchange. A nil slice means failover is
	// disabled and the native resolver is left untouched. Populated from
	// MAILMOOSE_DNS_FALLBACK_SERVERS; empty selects the Cloudflare-then-Google
	// defaults and "off" disables failover.
	DNSFallbackServers []string
	// Build identifies the running binary: the commit stamped at build time, or
	// the build timestamp when only that is known. It is reported by /healthz
	// and the discovery document so an operator can tell "not fixed" from "not
	// deployed".
	Build string
}

func Load() (Config, error) {
	receiverEnabled, err := strconv.ParseBool(env("DEDICATED_RECEIVER_ENABLE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("DEDICATED_RECEIVER_ENABLE must be a boolean")
	}
	receiverPort, err := strconv.Atoi(env("DEDICATED_RECEIVER_PORT", "8082"))
	if err != nil || receiverPort < 1 || receiverPort > 65535 {
		return Config{}, fmt.Errorf("DEDICATED_RECEIVER_PORT must be an integer from 1 to 65535")
	}
	receiverURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DEDICATED_RECEIVER_URL")), "/")
	if receiverURL != "" {
		if _, err := parseBaseHost(receiverURL); err != nil {
			return Config{}, fmt.Errorf("DEDICATED_RECEIVER_URL: %w", err)
		}
	}
	admin, err := loadAdmin()
	if err != nil {
		return Config{}, err
	}
	dnsFallbackServers, err := dnsfallback.ParseServers(env("MAILMOOSE_DNS_FALLBACK_SERVERS", ""))
	if err != nil {
		return Config{}, fmt.Errorf("MAILMOOSE_DNS_FALLBACK_SERVERS: %w", err)
	}
	baseURL := strings.TrimRight(env("BASE_URL", "http://localhost:8081"), "/")
	baseHost, err := parseBaseHost(baseURL)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:              env("LISTEN_ADDR", ":8081"),
		BaseURL:                 baseURL,
		DedicatedReceiverEnable: receiverEnabled,
		DedicatedReceiverPort:   receiverPort,
		DedicatedReceiverURL:    receiverURL,
		MicrosoftClientID:       strings.TrimSpace(env("MICROSOFT_CLIENT_ID", "1b20bf22-8552-44a8-b4ff-1fabddafab02")),
		MicrosoftTenantID:       env("MICROSOFT_TENANT_ID", "common"),
		baseHost:                baseHost,
		DataDir:                 env("DATA_DIR", "/data"),
		Mode:                    strings.ToLower(env("MODE", "selfhosted")),
		AllowRegistration:       envBool("ALLOW_REGISTRATION", false),
		TrustProxyHeaders:       envBool("TRUST_PROXY_HEADERS", false),
		ForceHTTPS:              envBool("FORCE_HTTPS", false),
		NoIndex:                 envBool("NOINDEX", false),
		AppEncryptionKey:        strings.TrimSpace(os.Getenv("APP_ENCRYPTION_KEY")),
		AdminEmail:              admin.email,
		AdminPassword:           admin.password,
		AdminAccountName:        admin.accountName,
		MaxMessageBytes:         envInt64("MAX_MESSAGE_BYTES", 30<<20),
		DefaultQuotaBytes:       envInt64("DEFAULT_STORAGE_QUOTA_BYTES", 100<<20),
		SessionTTL:              time.Duration(envInt("SESSION_TTL_HOURS", 24*14)) * time.Hour,
		RelayRequireBearer:      envBool("RELAY_REQUIRE_CALLER_AUTH", false),
		LoginLimitPerMinute:     envInt("LOGIN_LIMIT_PER_MINUTE", 10),
		SendLimitPerMinute:      envInt("SEND_LIMIT_PER_MINUTE", 60),
		RegisterLimitPerMinute:  envInt("REGISTER_LIMIT_PER_MINUTE", 5),
		AllowPrivateOutbound:    envBool("ALLOW_PRIVATE_OUTBOUND", true),
		InboundConcurrency:      envInt("INBOUND_CONCURRENCY", 32),
		MaxMultipartParts:       envInt("MAX_MULTIPART_PARTS", 64),
		MaxMIMEDepth:            envInt("MAX_MIME_DEPTH", 8),
		MaxMIMEParts:            envInt("MAX_MIME_PARTS", 256),
		BodyReadTimeout:         time.Duration(envInt("BODY_READ_TIMEOUT_SECONDS", 30)) * time.Second,
		OutboundHTTPTimeout:     time.Duration(envInt("OUTBOUND_HTTP_TIMEOUT_SECONDS", 300)) * time.Second,
		OutboundConcurrency:     envInt("OUTBOUND_CONCURRENCY", 5),
		RemoteSyncConcurrency:   envInt("REMOTE_SYNC_CONCURRENCY", 2),
		ApprovalExpiryHours:     envInt("APPROVAL_EXPIRY_HOURS", 48),
		WebhookRetryWindow:      time.Duration(envInt("WEBHOOK_RETRY_WINDOW_DAYS", 7)) * 24 * time.Hour,
		MXReceiptRetention:      time.Duration(envInt("MX_RECEIPT_RETENTION_HOURS", 7*24)) * time.Hour,
		MXUID:                   envInt("MX_UID", 65533),
		MXGID:                   envInt("MX_GID", 65533),
		InboundTLSCertFile:      strings.TrimSpace(os.Getenv("INBOUND_TLS_CERT_FILE")),
		InboundTLSKeyFile:       strings.TrimSpace(os.Getenv("INBOUND_TLS_KEY_FILE")),
		DialMXCAFile:            strings.TrimSpace(os.Getenv("DIALMX_CA_FILE")),
		DNSFallbackServers:      dnsFallbackServers,
		Build:                   strings.TrimSpace(os.Getenv("MAILMOOSE_BUILD")),
	}
	if cfg.Mode != "selfhosted" {
		return Config{}, fmt.Errorf("MODE must be selfhosted")
	}
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err == nil && cfg.DedicatedReceiverEnable {
		if n, err := strconv.Atoi(port); err == nil && n == cfg.DedicatedReceiverPort {
			return Config{}, fmt.Errorf("LISTEN_ADDR must use a different port from DEDICATED_RECEIVER_PORT")
		}
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
	if cfg.OutboundHTTPTimeout <= 0 {
		return Config{}, fmt.Errorf("OUTBOUND_HTTP_TIMEOUT_SECONDS must be positive")
	}
	if cfg.OutboundConcurrency < 1 || cfg.OutboundConcurrency > 32 {
		return Config{}, fmt.Errorf("OUTBOUND_CONCURRENCY must be between 1 and 32")
	}
	if cfg.RemoteSyncConcurrency < 1 || cfg.RemoteSyncConcurrency > 32 {
		return Config{}, fmt.Errorf("REMOTE_SYNC_CONCURRENCY must be between 1 and 32")
	}
	if cfg.ApprovalExpiryHours < 0 {
		return Config{}, fmt.Errorf("APPROVAL_EXPIRY_HOURS must be zero or greater")
	}
	// The legacy MX environment variables (MX_ENABLE, MX_RECEIVER_URL,
	// DIALMX_CORE_KEY and the MX_* SMTP tunables) are no longer read or
	// validated here: the persisted mx_settings row is the authoritative
	// receiver configuration, so a stale or partial legacy value must not fail
	// startup.
	if cfg.MXUID <= 0 || cfg.MXGID <= 0 || cfg.MXUID > math.MaxInt32 || cfg.MXGID > math.MaxInt32 {
		return Config{}, fmt.Errorf("MX_UID and MX_GID must be positive integers within the 32-bit uid range")
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

// SecureContext reports whether the canonical origin is a browser secure
// context, which WebAuthn (passkeys) requires. A context is secure when the
// scheme is https, or the host is a loopback address: browsers treat
// http://localhost (and 127.0.0.1 / ::1) as secure for local development. A
// plain-HTTP deployment reached by IP or hostname is not, and the browser
// refuses to run a WebAuthn ceremony there.
func (c Config) SecureContext() bool {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback()
	}
	return false
}

// WebAuthnRPID returns the relying-party id for passkeys: the canonical host
// with any port removed. WebAuthn scopes a credential to a registrable domain,
// so the port must not be included; a bare "localhost" stays "localhost" for
// local development.
func (c Config) WebAuthnRPID() string {
	host := c.BaseHost()
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// ReceiverURL is the public origin used for inbound provider setup. It is
// independent of whether the dedicated listener is enabled.
func (c Config) ReceiverURL() string {
	if c.DedicatedReceiverURL != "" {
		return c.DedicatedReceiverURL
	}
	return c.BaseURL
}

// WebAuthnOrigins returns the canonical UI origin permitted for passkeys.
func (c Config) WebAuthnOrigins() []string {
	if c.BaseURL == "" {
		return nil
	}
	return []string{c.BaseURL}
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
// public-routable destinations. The check is off by default: self-hosting is the
// primary model, so private gateways, local relays and LAN receivers work
// without configuration. An operator who must confine outbound traffic to the
// public internet opts in by setting ALLOW_PRIVATE_OUTBOUND=false.
func (c Config) RequirePublicOutbound() bool {
	return !c.AllowPrivateOutbound
}

// outboundDeliveryMargin is added to the provider HTTP timeout to size the
// per-message delivery context, so the client's own timeout fires before the
// delivery context's deadline and a timeout is reported as an ambiguous send
// rather than a context cancellation.
const outboundDeliveryMargin = 30 * time.Second

// OutboundDeliveryAttemptTimeout is the per-attempt budget the outbox worker
// gives one outbound delivery. It is derived from the provider HTTP timeout plus
// a fixed margin so recording the outcome and the read/write work fit inside it.
func (c Config) OutboundDeliveryAttemptTimeout() time.Duration {
	t := c.OutboundHTTPTimeout
	if t <= 0 {
		t = 300 * time.Second
	}
	return t + outboundDeliveryMargin
}

// OutboundConcurrency is the number of outbox deliveries sent at once. Each
// in-flight send holds its attachments in memory, so this bounds peak memory as
// well as parallelism.
func (c Config) OutboundConcurrencyCount() int {
	if c.OutboundConcurrency < 1 {
		return 1
	}
	return c.OutboundConcurrency
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
