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
	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)
	if _, err := netutil.HTTPClient().Get(ts.URL); err == nil {
		t.Fatal("expected a private destination to be rejected in hosted mode")
	}
}

func TestValidateBaseURL(t *testing.T) {
	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)
	for _, test := range []struct {
		name    string
		base    string
		wantErr bool
	}{
		{"empty", "", false},
		{"https public host", "https://api.example.com", false},
		{"https private literal", "https://127.0.0.1:8080", true},
		{"https metadata literal", "https://169.254.169.254", true},
		{"http public host", "http://api.example.com", true},
		{"malformed", "://", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := netutil.ValidateBaseURL(test.base)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateBaseURL(%q) err=%v wantErr=%v", test.base, err, test.wantErr)
			}
		})
	}
	netutil.SetRequirePublic(false)
	if err := netutil.ValidateBaseURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("opt-out should accept a private http base: %v", err)
	}
}

func TestHTTPClientDoesNotFollowRedirect(t *testing.T) {
	netutil.SetRequirePublic(false)
	defer netutil.SetRequirePublic(false)
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
