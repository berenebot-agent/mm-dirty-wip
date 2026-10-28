package mxdial

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// Tusk MX endpoint discovery.
//
// The build embeds one canonical manifest and also fetches it live from the
// repository at setup-save time. A live fetch lets the service publish
// additional receivers for new setups immediately, without a core release;
// existing setups are unaffected because every setup snapshots the receivers it
// enrolled. The fetch is best-effort: a cached last-known-good copy, then the
// embedded copy, keep setup working through any network or hosting failure.
//
// The manifest decoder deliberately ignores unknown fields (unlike the mx-v1
// wire contract), so the hosted document can grow fields without breaking older
// cores; known fields are still validated strictly.

// AntlerManifestURL is the live manifest fetched when a setup is saved. The
// embedded copy in endpoints.json is the fallback and the copy of record for
// this repository.
const AntlerManifestURL = "https://raw.githubusercontent.com/dellarb/mailmoose/main/internal/transport/mxdial/endpoints.json"

const (
	antlerManifestTTL       = 10 * time.Minute
	antlerFetchTimeout      = 5 * time.Second
	antlerRetryAfterFailure = time.Minute
	antlerManifestMaxBytes  = 1 << 20
	antlerMaxReceivers      = 8
)

//go:embed endpoints.json
var embeddedAntlerManifest []byte

// antler-endpoints.json is the pre-rename filename. Older cores still fetch it
// from this repository, so it is kept and used only if the copy of record fails
// to decode.
//
//go:embed antler-endpoints.json
var embeddedLegacyAntlerManifest []byte

// AntlerReceiver is one Tusk MX receiver: the HTTPS session origin the core
// dials and the SMTP hostname (with MX preference) a receiving domain publishes.
type AntlerReceiver struct {
	ID           string `json:"id"`
	SessionURL   string `json:"session_url"`
	SMTPHostname string `json:"smtp_hostname"`
	MXPriority   int    `json:"mx_priority"`
}

// AntlerSource names where a resolved receiver set came from, so setup can say
// whether it used the live manifest or the copy compiled into the binary.
type AntlerSource string

const (
	// AntlerSourceGitHub is a manifest fetched from the live URL this call.
	AntlerSourceGitHub AntlerSource = "github"
	// AntlerSourceCache is a last-known-good copy from an earlier fetch.
	AntlerSourceCache AntlerSource = "cache"
	// AntlerSourceEmbedded is the copy compiled into this build, used when the
	// fetch failed and no cached copy exists.
	AntlerSourceEmbedded AntlerSource = "embedded"
)

// AntlerEndpointSet is a resolved receiver set plus where it came from.
// FetchedAt is when the set was fetched from the manifest: the current time for
// a live fetch, the original fetch time for a cached set, and the zero time for
// the embedded copy. Err is the fetch failure that forced the embedded copy and
// is nil for every other source.
type AntlerEndpointSet struct {
	Receivers []AntlerReceiver
	Source    AntlerSource
	FetchedAt time.Time
	Err       error
}

type antlerManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Receivers     []AntlerReceiver `json:"receivers"`
}

func init() {
	if _, err := ParseAntlerManifest(embeddedAntlerManifest); err != nil {
		panic("mxdial: embedded Tusk manifest is invalid: " + err.Error())
	}
}

// ParseAntlerManifest decodes and validates a manifest. Unknown fields are
// ignored so the hosted document can add fields without breaking older cores.
func ParseAntlerManifest(data []byte) ([]AntlerReceiver, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var m antlerManifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("mxdial: invalid Tusk manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("mxdial: invalid Tusk manifest: trailing data")
	}
	if m.SchemaVersion != 1 {
		return nil, fmt.Errorf("mxdial: unsupported Tusk manifest schema %d", m.SchemaVersion)
	}
	if err := ValidateAntlerReceivers(m.Receivers); err != nil {
		return nil, err
	}
	return m.Receivers, nil
}

// ValidateAntlerReceivers enforces the endpoint contract: one to eight
// receivers with unique ids, canonical HTTPS session origins and DNS hostnames
// with a bounded priority. The hosted service is predefined, so its origins
// are additionally held to a public-only policy regardless of the operator's
// outbound opt-out: an IP-literal origin must be public-routable and obvious
// private names are refused. The manifest itself is the trust anchor (fetched
// over TLS from the project repository); see D082.
func ValidateAntlerReceivers(receivers []AntlerReceiver) error {
	if len(receivers) == 0 || len(receivers) > antlerMaxReceivers {
		return fmt.Errorf("mxdial: Tusk manifest must list one to %d receivers", antlerMaxReceivers)
	}
	seen := map[string]bool{}
	for _, r := range receivers {
		if !validAntlerID(r.ID) {
			return fmt.Errorf("mxdial: invalid Tusk receiver id")
		}
		if seen[r.ID] {
			return fmt.Errorf("mxdial: duplicate Tusk receiver id")
		}
		seen[r.ID] = true
		canonical, err := mxwire.ReceiverURL(r.SessionURL)
		if err != nil || canonical != r.SessionURL {
			return fmt.Errorf("mxdial: Tusk receiver %q session url must be a canonical HTTPS origin", r.ID)
		}
		if err := validateAntlerOrigin(canonical); err != nil {
			return fmt.Errorf("mxdial: Tusk receiver %q: %w", r.ID, err)
		}
		host, err := mxwire.CanonicalDomain(r.SMTPHostname)
		if err != nil || host != r.SMTPHostname {
			return fmt.Errorf("mxdial: Tusk receiver %q smtp hostname is invalid", r.ID)
		}
		if r.MXPriority < 0 || r.MXPriority > 65535 {
			return fmt.Errorf("mxdial: Tusk receiver %q mx priority is out of range", r.ID)
		}
	}
	return nil
}

// validateAntlerOrigin applies the always-on public-only policy to a manifest
// session origin. Hostnames are resolved at dial time by the shared dialer, so
// this is a static guard; an IP literal must be public-routable and the
// obvious local-only names are refused outright.
func validateAntlerOrigin(canonical string) error {
	u, err := url.Parse(canonical)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid session url")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if !netutil.PublicIP(ip) {
			return fmt.Errorf("session url host is not public-routable")
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || !strings.Contains(host, ".") {
		return fmt.Errorf("session url host must be a public DNS name")
	}
	return nil
}

func validAntlerID(id string) bool {
	if len(id) == 0 || len(id) > 32 {
		return false
	}
	for _, ch := range []byte(id) {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

// EmbeddedAntlerReceivers returns the manifest compiled into this build. The
// legacy antler-endpoints.json copy is the fallback when the copy of record does
// not decode.
func EmbeddedAntlerReceivers() []AntlerReceiver {
	if receivers, err := ParseAntlerManifest(embeddedAntlerManifest); err == nil {
		return receivers
	}
	receivers, _ := ParseAntlerManifest(embeddedLegacyAntlerManifest)
	return receivers
}

// LegacyEmbeddedManifest returns the raw antler-endpoints.json copy compiled
// into this build, so tests can hold it to the same receiver set as the copy of
// record that older cores fetch from that path.
func LegacyEmbeddedManifest() []byte {
	return embeddedLegacyAntlerManifest
}

// AntlerResolver resolves the live manifest with a bounded cache, falling back
// to the last known good copy and then the embedded manifest.
type AntlerResolver struct {
	URL    string
	Client *http.Client
	TTL    time.Duration
	// Now overrides the clock in tests. Nil means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	cached    []AntlerReceiver
	fetchedAt time.Time
	failedAt  time.Time
}

// DefaultAntlerResolver returns the resolver used in production: the repository
// manifest over the shared guarded outbound client.
func DefaultAntlerResolver() *AntlerResolver {
	return &AntlerResolver{URL: AntlerManifestURL, Client: netutil.HTTPClient(), TTL: antlerManifestTTL}
}

func (r *AntlerResolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *AntlerResolver) ttl() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return antlerManifestTTL
}

// Receivers returns the current receiver set and its source. A fresh cache is
// returned directly; a stale cache is returned while a recent fetch failure is
// still in its retry cooldown; otherwise the manifest is fetched and validated.
// When the fetch fails and there is no cache, the embedded manifest is used so
// setup never depends on the network, with the fetch error carried alongside it
// so the caller can report the fallback.
func (r *AntlerResolver) Receivers(ctx context.Context) (AntlerEndpointSet, error) {
	now := r.now()
	r.mu.Lock()
	if len(r.cached) > 0 {
		if now.Sub(r.fetchedAt) < r.ttl() {
			out := r.cachedSet(AntlerSourceCache)
			r.mu.Unlock()
			return out, nil
		}
		if !r.failedAt.IsZero() && now.Sub(r.failedAt) < antlerRetryAfterFailure {
			out := r.cachedSet(AntlerSourceCache)
			r.mu.Unlock()
			return out, nil
		}
	}
	r.mu.Unlock()

	fetched, err := r.fetch(ctx)
	if err != nil {
		r.mu.Lock()
		if len(r.cached) > 0 {
			r.failedAt = now
			out := r.cachedSet(AntlerSourceCache)
			r.mu.Unlock()
			return out, nil
		}
		r.mu.Unlock()
		embedded := EmbeddedAntlerReceivers()
		if len(embedded) == 0 {
			return AntlerEndpointSet{}, fmt.Errorf("mxdial: no Tusk endpoints available: %w", err)
		}
		return AntlerEndpointSet{Receivers: embedded, Source: AntlerSourceEmbedded, Err: err}, nil
	}
	r.mu.Lock()
	r.cached = cloneAntlerReceivers(fetched)
	r.fetchedAt = now
	r.failedAt = time.Time{}
	r.mu.Unlock()
	return AntlerEndpointSet{Receivers: fetched, Source: AntlerSourceGitHub, FetchedAt: now}, nil
}

// cachedSet returns the cached receivers with the time they were fetched, so
// callers can report when the live manifest was last read. The caller holds mu.
func (r *AntlerResolver) cachedSet(source AntlerSource) AntlerEndpointSet {
	return AntlerEndpointSet{Receivers: cloneAntlerReceivers(r.cached), Source: source, FetchedAt: r.fetchedAt}
}

func (r *AntlerResolver) fetch(ctx context.Context) ([]AntlerReceiver, error) {
	url := r.URL
	if strings.TrimSpace(url) == "" {
		url = AntlerManifestURL
	}
	client := r.Client
	if client == nil {
		client = netutil.HTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, antlerFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mxdial: Tusk manifest fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, antlerManifestMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > antlerManifestMaxBytes {
		return nil, fmt.Errorf("mxdial: Tusk manifest too large")
	}
	return ParseAntlerManifest(body)
}

func cloneAntlerReceivers(in []AntlerReceiver) []AntlerReceiver {
	if in == nil {
		return nil
	}
	out := make([]AntlerReceiver, len(in))
	copy(out, in)
	return out
}
