package receiver_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// fakeAddr is a fixed net.Addr used to present a non-loopback peer to the
// admission gate while the test socket really binds loopback.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

type spoofConn struct {
	net.Conn
	remote net.Addr
}

func (c *spoofConn) RemoteAddr() net.Addr { return c.remote }

// spoofListener rewrites every accepted connection's RemoteAddr so a test can
// exercise the trusted-proxy gate without a second host.
type spoofListener struct {
	net.Listener
	remote string
}

func (l *spoofListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &spoofConn{Conn: c, remote: fakeAddr(l.remote)}, nil
}

// newCleartextH2Server starts the receiver's session handler over cleartext
// HTTP/2 (h2c, prior knowledge) with the given peer address, which a
// TLS-terminating proxy fronts.
func newCleartextH2Server(t *testing.T, r *receiver.Receiver, peer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP2(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = &spoofListener{Listener: ln, remote: peer}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// h2cClient returns an HTTP client that speaks cleartext HTTP/2 (prior
// knowledge) to the server, matching the receiver's own h2c listener.
func h2cClient() *http.Client {
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetHTTP2(true)
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: tr}
}

// cleartextSession opens a cleartext session and returns the HTTP status. A 200
// means the receiver wrote Ready and admitted the session; a 426 means the
// admission gate refused it before any session state existed.
func cleartextSession(t *testing.T, base string) int {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+mxwire.SessionPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := h2cClient().Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "test"})
	if err := mxwire.WriteFrame(pw, hello); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-done:
		if resp == nil {
			t.Fatal("no response")
		}
		defer resp.Body.Close()
		return resp.StatusCode
	case <-time.After(5 * time.Second):
		t.Fatal("cleartext handshake timeout")
		return 0
	}
}

// TestSharedModeRejectsCleartextWithoutTrustedProxy proves the default: with an
// empty allowlist a cleartext shared-mode session from a routable peer is
// refused with 426.
func TestSharedModeRejectsCleartextWithoutTrustedProxy(t *testing.T) {
	r := newReceiver(t, receiver.Config{Mode: "shared"})
	srv := newCleartextH2Server(t, r, "203.0.113.9:5555")
	if code := cleartextSession(t, srv.URL); code != http.StatusUpgradeRequired {
		t.Fatalf("expected 426 for untrusted cleartext, got %d", code)
	}
}

// TestSharedModeAdmitsCleartextFromTrustedProxy proves the allowlist path: a
// peer inside the allowlist is admitted and the session reaches Ready.
func TestSharedModeAdmitsCleartextFromTrustedProxy(t *testing.T) {
	r := newReceiver(t, receiver.Config{Mode: "shared", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}})
	srv := newCleartextH2Server(t, r, "203.0.113.9:5555")
	if code := cleartextSession(t, srv.URL); code != http.StatusOK {
		t.Fatalf("expected 200 for trusted cleartext, got %d", code)
	}
}

// TestSharedModeCleartextLoopback proves the loopback exemption: the included
// receiver dials 127.0.0.1 cleartext without being in the allowlist. It also
// checks that an h2c GET reaches the health endpoint.
func TestSharedModeCleartextLoopback(t *testing.T) {
	r := newReceiver(t, receiver.Config{Mode: "shared"})
	srv := newCleartextH2Server(t, r, "127.0.0.1:5555")
	if code := cleartextSession(t, srv.URL); code != http.StatusOK {
		t.Fatalf("expected loopback cleartext admission, got %d", code)
	}
	resp, err := h2cClient().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
}
