package mxdial_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/mxwire"
	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// TestSessionRefusalIsReportedRejectedNotReconnecting proves a receiver that
// answers the session POST with a refusal is reported as a settled rejection.
// An older receiver build answers 404 for the session path this core posts, and
// the row must not sit on "connecting" or read as a retry: the receiver has
// answered, and nothing about the failure is still in progress.
func TestSessionRefusalIsReportedRejectedNotReconnecting(t *testing.T) {
	t.Parallel()
	receiverURL, tlsCfg := startRefusingReceiver(t, http.StatusNotFound)
	m, _ := startReceiverManager(t, receiverURL, tlsCfg)

	waitState(t, m, "example.test", mxdial.StatusRejected, 5*time.Second)
	if rows := m.Status("example.test"); rows[0].Reason != mxdial.ReasonRejected {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, mxdial.ReasonRejected)
	}
	// The refusal is settled, so it survives retries: neither the in-progress
	// "connecting" nor the retrying "disconnected" may replace it.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, row := range m.Status("example.test") {
			if row.State != mxdial.StatusRejected {
				t.Fatalf("state = %q after a settled refusal, want %q", row.State, mxdial.StatusRejected)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTransientReceiverErrorKeepsRetrying proves a receiver's own 5xx stays a
// retrying session rather than becoming a settled refusal: the receiver is up
// and is expected to recover, so the row must not claim it refused this core.
func TestTransientReceiverErrorKeepsRetrying(t *testing.T) {
	t.Parallel()
	receiverURL, tlsCfg := startRefusingReceiver(t, http.StatusServiceUnavailable)
	m, _ := startReceiverManager(t, receiverURL, tlsCfg)

	waitState(t, m, "example.test", mxdial.StatusDisconnected, 5*time.Second)
	rows := m.Status("example.test")
	if rows[0].Reason != mxdial.ReasonReceiverError {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, mxdial.ReasonReceiverError)
	}
}

// TestAnsweredStatusIsCarriedOnTheRow proves the HTTP status the receiver URL
// answered with is carried on the status row, so a panel can name an endpoint's
// own 4xx/5xx instead of a bare "unreachable". Whatever answered at that URL —
// the receiver, or a proxy in front of it — owns that status, and the core
// cannot tell the two apart, so the code is carried and nothing is attributed.
func TestAnsweredStatusIsCarriedOnTheRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		code       int
		wantState  string
		wantReason string
	}{
		{"server error", http.StatusBadGateway, mxdial.StatusDisconnected, mxdial.ReasonReceiverError},
		{"refusal", http.StatusMethodNotAllowed, mxdial.StatusRejected, mxdial.ReasonRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			receiverURL, tlsCfg := startRefusingReceiver(t, tc.code)
			m, _ := startReceiverManager(t, receiverURL, tlsCfg)

			waitState(t, m, "example.test", tc.wantState, 5*time.Second)
			rows := m.Status("example.test")
			if rows[0].HTTPCode != tc.code {
				t.Fatalf("http code = %d, want %d", rows[0].HTTPCode, tc.code)
			}
			if rows[0].Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", rows[0].Reason, tc.wantReason)
			}
		})
	}
}

// TestModeMismatchIsReportedRejected proves a receiver that answers the other
// session mode is a settled, incompatible pairing rather than a connection still
// being authorized.
func TestModeMismatchIsReportedRejected(t *testing.T) {
	t.Parallel()
	rc := newFakeReceiver(t)
	rc.setReadyMode("single")
	b := &scriptedBackend{}
	b.setDomains(mxdial.Domain{Name: "example.test", KeyID: rc.keyID, PrivateKey: rc.priv, ReceiverURLs: []string{rc.url()}})
	m, _ := managerFor(t, b, rc, t.TempDir())

	waitState(t, m, "example.test", mxdial.StatusRejected, 5*time.Second)
	if rows := m.Status("example.test"); rows[0].Reason != mxdial.ReasonModeMismatch {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, mxdial.ReasonModeMismatch)
	}
}

// TestBadReadyVersionIsReportedProtocolMismatch proves a receiver that completes
// the transport handshake but answers a Ready frame in another protocol version
// is reported as a protocol mismatch, not as an unreachable receiver.
func TestBadReadyVersionIsReportedProtocolMismatch(t *testing.T) {
	t.Parallel()
	receiverURL, tlsCfg := startBadReadyReceiver(t)
	m, _ := startReceiverManager(t, receiverURL, tlsCfg)

	waitState(t, m, "example.test", mxdial.StatusRejected, 5*time.Second)
	if rows := m.Status("example.test"); rows[0].Reason != mxdial.ReasonProtocolMismatch {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, mxdial.ReasonProtocolMismatch)
	}
}

// startRefusingReceiver serves the session path with one HTTP status and no
// frames, standing in for a receiver that answers but will not take the session.
// It reads the Hello first, like a real receiver, so the refusal answers a
// complete attempt rather than resetting the stream mid-write.
func startRefusingReceiver(t *testing.T, code int) (string, *tls.Config) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = mxwire.ReadFrame(r.Body)
		w.WriteHeader(code)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, trustServer(t, srv)
}

// startBadReadyReceiver completes the handshake with a Ready frame from another
// protocol version: a receiver built against a different core API.
func startBadReadyReceiver(t *testing.T) (string, *tls.Config) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := mxwire.ReadFrame(r.Body); err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		f, err := mxwire.JSONFrame(mxwire.FrameReady, 0, 0, mxwire.Ready{
			Mode: "shared", Version: "mx-v0", ReceiverID: "receiver-old", ConnectionID: "conn-old", MaxMessageBytes: 1 << 20,
		})
		if err != nil {
			return
		}
		if err := mxwire.WriteFrame(w, f); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, trustServer(t, srv)
}

// trustServer returns a TLS config that trusts a test server's self-signed
// certificate, the same way managerFor wires the fake receiver.
func trustServer(t *testing.T, srv *httptest.Server) *tls.Config {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool, ServerName: parsed.Hostname()}
}

// startReceiverManager runs a shared-mode manager against one receiver URL for
// one domain, so a session-level failure is observable on that domain's row.
func startReceiverManager(t *testing.T, receiverURL string, tlsCfg *tls.Config) (*mxdial.Manager, *scriptedBackend) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := &scriptedBackend{}
	b.setDomains(mxdial.Domain{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{receiverURL}})
	m := mxdial.New(b, mxdial.Config{
		DataDir:                  t.TempDir(),
		TLSConfig:                tlsCfg,
		ReconcileInterval:        20 * time.Millisecond,
		AllowPrivateDestinations: true,
	})
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
	return m, b
}
