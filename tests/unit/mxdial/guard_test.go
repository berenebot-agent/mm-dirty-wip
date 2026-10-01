package mxdial_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// startSingleReceiver starts a real single-mode receiver on a loopback port and
// returns its URL. In single mode the receiver requires a matching bearer but
// permits cleartext HTTP/2, so the only thing that can vary between tests is
// the destination policy or the bearer.
func startSingleReceiver(t *testing.T, coreKey string) *httptest.Server {
	t.Helper()
	r := receiver.New(receiver.Config{Mode: "single", CoreKey: coreKey}, nil)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// startManager runs a manager with the given configuration and stops it when
// the test ends.
func startManager(t *testing.T, cfg mxdial.Config) *mxdial.Manager {
	t.Helper()
	m := mxdial.New(&scriptedBackend{}, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})
	return m
}

// waitConnectionState waits for the single-mode connection status to reach want.
func waitConnectionState(t *testing.T, m *mxdial.Manager, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last mxdial.Status
	for time.Now().Before(deadline) {
		last = m.ConnectionStatus()
		if last.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connection state = %q (reason %q), want %q", last.State, last.Reason, want)
}

// assertNeverReady fails if the manager reports ready within the window, which
// is how a blocked or rejected handshake must present.
func assertNeverReady(t *testing.T, m *mxdial.Manager, d time.Duration) mxdial.Status {
	t.Helper()
	deadline := time.Now().Add(d)
	var last mxdial.Status
	for time.Now().Before(deadline) {
		last = m.ConnectionStatus()
		if last.State == "ready" {
			t.Fatalf("connection reported ready despite the guard: %#v", last)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// TestPrivateReceiverRejectedByDefault proves a loopback private receiver is
// refused when AllowPrivateDestinations is not set, so the SSRF guard is on by
// default for the dialer.
func TestPrivateReceiverRejectedByDefault(t *testing.T) {
	srv := startSingleReceiver(t, "private-key")
	m := startManager(t, mxdial.Config{
		DataDir:           t.TempDir(),
		ReceiverURL:       srv.URL,
		CoreKey:           "private-key",
		ReconcileInterval: 20 * time.Millisecond,
	})
	last := assertNeverReady(t, m, 700*time.Millisecond)
	if last.State != "disconnected" {
		t.Fatalf("guarded connection state = %q, want disconnected", last.State)
	}
}

// TestPrivateReceiverOptInBecomesReady proves the explicit opt-out allows a
// loopback receiver and that readiness is reported only after a real single
// handshake, not merely because a manager exists.
func TestPrivateReceiverOptInBecomesReady(t *testing.T) {
	srv := startSingleReceiver(t, "private-key")
	m := startManager(t, mxdial.Config{
		DataDir:                  t.TempDir(),
		ReceiverURL:              srv.URL,
		CoreKey:                  "private-key",
		ReconcileInterval:        20 * time.Millisecond,
		AllowPrivateDestinations: true,
	})
	if got := m.ConnectionStatus(); got.State != "connecting" {
		t.Fatalf("initial connection state = %q, want connecting", got.State)
	}
	waitConnectionState(t, m, "ready", 5*time.Second)
}

// TestPrivateReceiverWrongBearerNeverReady proves a rejected bearer yields a
// disconnected status rather than ready, distinguishing a real handshake from a
// mere dial.
func TestPrivateReceiverWrongBearerNeverReady(t *testing.T) {
	srv := startSingleReceiver(t, "private-key")
	m := startManager(t, mxdial.Config{
		DataDir:                  t.TempDir(),
		ReceiverURL:              srv.URL,
		CoreKey:                  "wrong-key",
		ReconcileInterval:        20 * time.Millisecond,
		AllowPrivateDestinations: true,
	})
	last := assertNeverReady(t, m, 700*time.Millisecond)
	if last.State != "disconnected" {
		t.Fatalf("wrong-bearer connection state = %q, want disconnected", last.State)
	}
}

// TestPrivateReceiverRejectsPrivateLiterals proves the guard is not bypassed by
// a numeric IPv4, numeric IPv6 or hostname localhost receiver URL. A receiver is
// bound on the exact address each spelling resolves to, so a missing guard would
// otherwise connect successfully: the rejection is attributable to the policy.
func TestPrivateReceiverRejectsPrivateLiterals(t *testing.T) {
	for _, tc := range []struct {
		name string
		// bind is the listener address; urlHost is the spelling used in the
		// receiver URL. They agree in address but differ in representation.
		bind    string
		urlHost string
	}{
		{"ipv4", "127.0.0.1:0", "127.0.0.1"},
		{"ipv6", "[::1]:0", "::1"},
		{"localhost", "127.0.0.1:0", "localhost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", tc.bind)
			if err != nil {
				t.Skipf("cannot bind %s: %v", tc.bind, err)
			}
			r := receiver.New(receiver.Config{Mode: "single", CoreKey: "private-key"}, nil)
			proto := new(http.Protocols)
			proto.SetUnencryptedHTTP2(true)
			httpSrv := &http.Server{Handler: r.Handler(), Protocols: proto}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = httpSrv.Serve(ln)
			}()
			t.Cleanup(func() {
				_ = httpSrv.Close()
				<-done
			})
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			host := tc.urlHost
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			m := startManager(t, mxdial.Config{
				DataDir:           t.TempDir(),
				ReceiverURL:       "http://" + host + ":" + port,
				CoreKey:           "private-key",
				ReconcileInterval: 20 * time.Millisecond,
				// Intentionally no AllowPrivateDestinations: the guard must
				// reject every private spelling.
			})
			last := assertNeverReady(t, m, 500*time.Millisecond)
			if last.State != "disconnected" {
				t.Fatalf("%s connection state = %q, want disconnected", tc.name, last.State)
			}
		})
	}
}
