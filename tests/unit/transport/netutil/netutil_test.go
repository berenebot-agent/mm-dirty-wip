package netutil_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

func TestHTTPClientRejectsPrivateWhenPublicRequired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)
	if _, err := netutil.HTTPClient().Get(ts.URL); err == nil {
		t.Fatal("expected a private destination to be rejected when public is required")
	}
}

func TestPublicIPRejectsSpecialUseRanges(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1",
		"0.0.0.0", "100.64.0.1", "192.0.0.1", "192.0.2.1",
		"198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"::1", "fc00::1", "fe80::1", "2001:db8::1",
	} {
		ip := net.ParseIP(s)
		if netutil.PublicIP(ip) {
			t.Fatalf("PublicIP(%s) = true, want false", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !netutil.PublicIP(net.ParseIP(s)) {
			t.Fatalf("PublicIP(%s) = false, want true", s)
		}
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

// A slow response must be aborted once the configured outbound timeout elapses,
// and the error must be recognisable as a client timeout (the signal the
// adapters use to mark a send ambiguous).
func TestOutboundHTTPTimeoutAbortsSlowResponse(t *testing.T) {
	netutil.SetRequirePublic(false)
	defer netutil.SetRequirePublic(false)
	netutil.SetOutboundHTTPTimeout(100 * time.Millisecond)
	defer netutil.SetOutboundHTTPTimeout(0)

	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); ts.Close() }()

	start := time.Now()
	_, err := netutil.HTTPClient().Get(ts.URL)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !netutil.IsClientTimeout(err) {
		t.Fatalf("IsClientTimeout(%v) = false, want true", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want it bounded near the configured 100ms", elapsed)
	}
}

// A restored (zero) override must not retain the shortened timeout.
func TestSetOutboundHTTPTimeoutRestoresDefault(t *testing.T) {
	netutil.SetOutboundHTTPTimeout(50 * time.Millisecond)
	netutil.SetOutboundHTTPTimeout(0)
	defer netutil.SetRequirePublic(false)
	netutil.SetRequirePublic(false)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	resp, err := netutil.HTTPClient().Get(ts.URL)
	if err != nil {
		t.Fatalf("default timeout should tolerate a 200ms response: %v", err)
	}
	resp.Body.Close()
}
