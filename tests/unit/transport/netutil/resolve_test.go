package netutil_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// withLoopbackResolver installs a loopback DNS resolver on net.DefaultResolver
// for the duration of a test. netutil consults net.DefaultResolver at call
// time, so this makes resolution deterministic without an exported test hook.
func withLoopbackResolver(t *testing.T, dns *loopbackDNS) {
	t.Helper()
	prev := net.DefaultResolver
	net.DefaultResolver = dns.resolver()
	t.Cleanup(func() { net.DefaultResolver = prev })
}

func TestResolvePublicHostRejectsPrivateAnswers(t *testing.T) {
	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)
	dns := newLoopbackDNS(t)
	withLoopbackResolver(t, dns)
	dns.setA("private.test", net.ParseIP("10.0.0.5"))
	dns.setA("loopback.test", net.ParseIP("127.0.0.1"))
	dns.setAAAA("v6private.test", net.ParseIP("fc00::1"))

	for _, host := range []string{"private.test", "loopback.test", "v6private.test"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := netutil.ResolvePublicHost(ctx, host)
		cancel()
		if err == nil {
			t.Fatalf("ResolvePublicHost(%s) accepted a non-public answer", host)
		}
	}
}

func TestResolvePublicHostAcceptsPublicAnswer(t *testing.T) {
	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)
	dns := newLoopbackDNS(t)
	withLoopbackResolver(t, dns)
	dns.setA("public.test", net.ParseIP("93.184.216.34"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := netutil.ResolvePublicHost(ctx, "public.test")
	if err != nil {
		t.Fatalf("ResolvePublicHost(public.test): %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("93.184.216.34")) {
		t.Fatalf("ResolvePublicHost(public.test) = %v", ips)
	}
}

// TestDialContextRebindingRejectsAtDialTime proves the dial guard re-resolves
// the host at connection time and rejects a name that has been rebound from a
// public address to a private one after an earlier successful resolution.
func TestDialContextRebindingRejectsAtDialTime(t *testing.T) {
	dns := newLoopbackDNS(t)
	withLoopbackResolver(t, dns)

	// First resolution is public and passes.
	dns.setA("rebind.test", net.ParseIP("93.184.216.34"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := netutil.ResolvePublicHost(ctx, "rebind.test"); err != nil {
		t.Fatalf("initial public resolution failed: %v", err)
	}

	// The name now rebinds to a private address. A dial must re-resolve and
	// reject rather than trusting the earlier public answer.
	dns.setA("rebind.test", net.ParseIP("10.1.2.3"))
	conn, err := netutil.DialContext(ctx, "tcp", "rebind.test:25", true)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not public-routable") {
		t.Fatalf("rebound private destination was not rejected: %v", err)
	}
	if dns.queryCount() < 2 {
		t.Fatalf("dial reused a stale resolution; queries = %d, want >= 2", dns.queryCount())
	}
}

// TestDialContextNumericPrivateRejected proves a numeric private literal is
// refused without relying on DNS.
func TestDialContextNumericPrivateRejected(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:25",
		"10.0.0.1:25",
		"169.254.169.254:80",
		"[::1]:25",
		"[fc00::1]:25",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := netutil.DialContext(ctx, "tcp", addr, true)
		cancel()
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatalf("DialContext(%s) allowed a non-public destination", addr)
		}
	}
}

// TestDialContextNumericPublicAttemptsDial proves a public numeric literal is
// not rejected by the guard: it proceeds to a real (and here failing)
// connection attempt rather than an address-policy error.
func TestDialContextNumericPublicAttemptsDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := netutil.DialContext(ctx, "tcp", "93.184.216.34:9", true)
	if conn != nil {
		conn.Close()
	}
	if err != nil && strings.Contains(err.Error(), "not public-routable") {
		t.Fatalf("public numeric literal was rejected by policy: %v", err)
	}
}

// TestDialContextOptOutConnectsLoopback proves publicOnly=false still dials a
// private loopback destination, so the guard is opt-out rather than absolute.
func TestDialContextOptOutConnectsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := netutil.DialContext(ctx, "tcp", ln.Addr().String(), false)
	if err != nil {
		t.Fatalf("opt-out dial to loopback failed: %v", err)
	}
	conn.Close()
}
