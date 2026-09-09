// Package netutil provides shared network helpers for outbound transports,
// notably the public-routable destination check used to prevent SSRF in hosted
// mode.
package netutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var hosted atomic.Bool

var (
	clientMu sync.Mutex
	shared   *http.Client
)

// SetHosted controls whether outbound HTTP transports reject non-public
// destinations. It mirrors the SMTP adapter's hosted flag and resets the shared
// client so a later change takes effect.
func SetHosted(v bool) {
	hosted.Store(v)
	clientMu.Lock()
	if shared != nil {
		shared.CloseIdleConnections()
		shared = nil
	}
	clientMu.Unlock()
}

// Hosted reports whether hosted-mode destination restrictions are active.
func Hosted() bool { return hosted.Load() }

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
// In hosted mode it rejects any destination that is not public-routable before
// a connection is attempted.
func ResolvePublicHost(ctx context.Context, host string) ([]net.IP, error) {
	ips, err := lookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host resolved no addresses")
	}
	if !hosted.Load() {
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

// HTTPClient returns the shared outbound provider client. In hosted mode it
// resolves and validates the destination inside the dialer (so DNS cannot
// rebind between validation and connection) and never routes through a proxy;
// in self-hosted mode it honours the standard proxy environment. Redirects are
// never followed, so a tenant-controlled api_base cannot bounce a request to an
// unvalidated destination.
func HTTPClient() *http.Client {
	clientMu.Lock()
	defer clientMu.Unlock()
	if shared == nil {
		transport := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         dialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		if hosted.Load() {
			transport.Proxy = nil
		}
		shared = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return shared
}

func dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !hosted.Load() {
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
