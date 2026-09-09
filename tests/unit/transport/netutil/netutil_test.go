package netutil_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gatehouse-mail/internal/transport/netutil"
)

func TestHTTPClientRejectsPrivateHosted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	netutil.SetHosted(true)
	defer netutil.SetHosted(false)
	if _, err := netutil.HTTPClient().Get(ts.URL); err == nil {
		t.Fatal("expected a private destination to be rejected in hosted mode")
	}
}

func TestHTTPClientDoesNotFollowRedirect(t *testing.T) {
	netutil.SetHosted(false)
	defer netutil.SetHosted(false)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	resp, err := netutil.HTTPClient().Get(ts.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
}
