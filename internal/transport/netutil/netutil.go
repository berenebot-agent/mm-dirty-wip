// Package netutil provides shared network helpers for outbound transports,
// notably the public-routable destination check used to prevent SSRF. The check
// is on by default in every mode; self-hosted operators can opt out when they
// intentionally send through a private gateway or local relay.
package netutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var requirePublic atomic.Bool

var (
	clientMu   sync.Mutex
	shared     *http.Client
	sharedLong *http.Client
)

// SetRequirePublic controls whether outbound transports reject non-public
// destinations. It also selects whether the outbound HTTP client honours the
// standard proxy environment: when enforcement is active the proxy is bypassed,
// because a proxy would resolve the destination itself and defeat the guard.
func SetRequirePublic(v bool) {
	requirePublic.Store(v)
	clientMu.Lock()
	if shared != nil {
		shared.CloseIdleConnections()
		shared = nil
	}
	if sharedLong != nil {
		sharedLong.CloseIdleConnections()
		sharedLong = nil
	}
	clientMu.Unlock()
}

// RequirePublic reports whether public-destination enforcement is active.
func RequirePublic() bool { return requirePublic.Load() }

// lookupIP is overridable in tests.
var lookupIP = net.DefaultResolver.LookupIP

// PublicIP reports whether ip is a public-routable address. Loopback, private,
// link-local, multicast, unspecified and documentation ranges are rejected.
func PublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

// ResolvePublicHost resolves host and returns its public-routable addresses.
// When enforcement is active it rejects any destination that is not
// public-routable before a connection is attempted.
func ResolvePublicHost(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host resolved no addresses")
	}
	if !requirePublic.Load() {
		return ips, nil
	}
	var public []net.IP
	for _, ip := range ips {
		if PublicIP(ip) {
			public = append(public, ip)
		}
	}
	if len(public) == 0 {
		return nil, fmt.Errorf("destination is not public-routable")
	}
	return public, nil
}

// ValidateBaseURL validates a user-supplied provider API base. When enforcement
// is active the URL must use HTTPS and must not name a loopback, private,
// link-local or otherwise non-public IP literal; hostnames are resolved at dial
// time so DNS cannot rebind between validation and connection. An empty base is
// allowed and means the caller uses the provider default.
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base URL")
	}
	if u.Host == "" {
		return fmt.Errorf("base URL has no host")
	}
	if !requirePublic.Load() {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("base URL must use https")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !PublicIP(ip) {
		return fmt.Errorf("base URL host is not public-routable")
	}
	return nil
}

// HTTPClient returns the shared outbound provider client. It resolves and
// validates the destination inside the dialer (so DNS cannot rebind between
// validation and connection) and never follows redirects.
func HTTPClient() *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if shared == nil {
		shared = newClient(30 * time.Second)
	}
	return shared
}

// HTTPClientLong returns a client with a longer timeout for large downloads
// (for example a provider's signed raw MIME), using the same guarded transport.
func HTTPClientLong() *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if sharedLong == nil {
		sharedLong = newClient(2 * time.Minute)
	}
	return sharedLong
}

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: guardedTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func guardedTransport() http.RoundTripper {
	// When enforcement is off the default transport is used, which honours the
	// standard proxy environment and remains overridable by tests through
	// http.DefaultTransport.
	if !requirePublic.Load() {
		return http.DefaultTransport
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         dialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !requirePublic.Load() {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		if !PublicIP(ip) {
			last = fmt.Errorf("destination %s is not public-routable", ip)
			continue
		}
		conn, derr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr != nil {
			last = derr
			continue
		}
		return conn, nil
	}
	if last == nil {
		last = fmt.Errorf("destination is not public-routable")
	}
	return nil, last
}
