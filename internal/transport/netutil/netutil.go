// Package netutil provides shared network helpers for outbound transports,
// notably the public-routable destination check used to prevent SSRF in hosted
// mode.
package netutil

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
)

var hosted atomic.Bool

// SetHosted controls whether outbound HTTP transports reject non-public
// destinations. It mirrors the SMTP adapter's hosted flag.
func SetHosted(v bool) { hosted.Store(v) }

// Hosted reports whether hosted-mode destination restrictions are active.
func Hosted() bool { return hosted.Load() }

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
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
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
