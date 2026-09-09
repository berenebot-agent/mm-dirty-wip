package netutil

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDialContextRejectsPrivateHosted(t *testing.T) {
	orig := lookupIP
	lookupIP = func(ctx context.Context, network, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	defer func() { lookupIP = orig }()
	SetHosted(true)
	defer SetHosted(false)
	if _, err := dialContext(context.Background(), "tcp", "internal.test:80"); err == nil {
		t.Fatal("expected a private destination to be rejected in hosted mode")
	}
}

func TestHTTPClientDoesNotFollowRedirect(t *testing.T) {
	SetHosted(false)
	defer SetHosted(false)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	resp, err := HTTPClient().Get(ts.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
}
