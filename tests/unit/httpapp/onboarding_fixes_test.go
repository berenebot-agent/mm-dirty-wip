package httpapp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// forceHTTPSHandler builds a handler whose config forces HTTPS, for testing the
// redirect and the advertised scheme without a TLS socket.
func forceHTTPSHandler(t *testing.T) http.Handler {
	t.Helper()
	return forceHTTPSHandlerWith(t, nil)
}

// forceHTTPSHandlerWith is forceHTTPSHandler with a hook to adjust the config,
// so tests can declare a trusted proxy.
func forceHTTPSHandlerWith(t *testing.T, adjust func(*config.Config)) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "https://mail.example.test", Mode: "selfhosted", ForceHTTPS: true, AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	if adjust != nil {
		adjust(&cfg)
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	return httpapp.New(svc, nil).Handler()
}

func adminToken(t *testing.T, svc *app.Service, accountID string) string {
	t.Helper()
	_, token, err := svc.Store.CreateAPIKey(context.Background(), accountID, "test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestMessagesWaitDoesNotReplayBacklog is the regression for the long-poll
// returning the oldest message instantly: with no cursor it must block for a
// new message, not hand back history.
func TestMessagesWaitDoesNotReplayBacklog(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	if _, _, err := svc.IngestInbound(context.Background(), "mailgun", signedMGRequest(t, testMailgunKey, "wait-seed", box.Address, inboundRawMessage())); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/wait?inbox="+box.ID+"&timeout=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("wait = %d body=%s, want 204 (must not replay backlog)", rr.Code, rr.Body.String())
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("wait returned in %s; it did not block", time.Since(start))
	}
}

// TestWaitRejectsMalformedCursor asserts a non-evt_ cursor is a 400 rather than
// silently treated as the start of history.
func TestWaitRejectsMalformedCursor(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	for _, path := range []string{
		"/v1/messages/wait?after=bogus&timeout=1",
		"/v1/events/wait?after=bogus&timeout=1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d body=%s, want 400", path, rr.Code, rr.Body.String())
		}
	}
}

func TestV1LimitsEndpoint(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	req := httptest.NewRequest(http.MethodGet, "/v1/limits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/limits = %d body=%s", rr.Code, rr.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["page_size_default"]; !ok {
		t.Fatalf("limits missing page_size_default: %#v", doc)
	}
}

// TestSendRequiresSubject asserts an empty subject is a descriptive 400 at send
// time rather than a later provider failure.
func TestSendRequiresSubject(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	body := `{"inbox_id":"` + box.ID + `","to":["friend@example.net"],"text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-subject send = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "subject is required") {
		t.Fatalf("error body does not explain the subject requirement: %s", rr.Body.String())
	}
}

func TestHealthAliasAndDocsRedirect(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	for _, path := range []string{"/health", "/healthz", "/v1/health"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/agent" {
		t.Fatalf("GET /docs = %d Location=%q, want 302 /agent", rr.Code, rr.Header().Get("Location"))
	}
}

// TestExamplesDefaultToRequestOrigin asserts a downloaded client points at the
// instance it came from instead of localhost.
func TestExamplesDefaultToRequestOrigin(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	get := func(path, host string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}
	if body := get("/examples/bash", "mail.example.org"); !strings.Contains(body, `DEFAULT_BASE_URL="http://mail.example.org"`) {
		t.Fatalf("bash client did not adopt request origin")
	}
	if body := get("/examples/python", "mail.example.org"); !strings.Contains(body, `DEFAULT_BASE_URL = "http://mail.example.org"`) {
		t.Fatalf("python client did not adopt request origin")
	}
	if body := get("/examples/bash", "mail.example.org:8081"); !strings.Contains(body, `DEFAULT_BASE_URL="http://mail.example.org:8081"`) {
		t.Fatalf("bash client did not preserve a valid port")
	}
	if body := get("/examples/bash", "mail.example.org;$(touch${IFS}/tmp/mammothmail-shell-injection)"); !strings.Contains(body, `DEFAULT_BASE_URL="http://localhost:8081"`) {
		t.Fatalf("bash client interpolated an unsafe Host header")
	}
}

func TestForceHTTPSRedirectsAndAdvertisesHTTPS(t *testing.T) {
	t.Parallel()
	h := forceHTTPSHandler(t)

	// The redirect target is the canonical BASE_URL host
	// (https://mail.example.test), never the request Host: r.Host is
	// attacker-controlled, so echoing it would turn the 308 into a phishing
	// redirect or poison caches.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
	req.Host = "mail.example.org"
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("plaintext /v1/bootstrap = %d, want 308", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "https://mail.example.test/v1/bootstrap" {
		t.Fatalf("redirect Location = %q", loc)
	}

	// A forged Host header must not change the redirect target.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
	req.Host = "evil.example"
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("plaintext /v1/bootstrap = %d, want 308", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "https://mail.example.test/v1/bootstrap" {
		t.Fatalf("forged-Host redirect Location = %q", loc)
	}

	// An untrusted X-Forwarded-Proto must not bypass the redirect.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
	req.Host = "mail.example.org"
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("untrusted X-Forwarded-Proto bypassed redirect: %d", rr.Code)
	}

	for _, path := range []string{"/health", "/v1/health"} {
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s must not redirect, got %d", path, rr.Code)
		}
	}

	rr = httptest.NewRecorder()
	// Fetch over https (TLS set) so the forced redirect does not apply; the
	// advertised server must still be https.
	req = httptest.NewRequest(http.MethodGet, "https://mail.example.org/openapi.json", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d", rr.Code)
	}
	var doc struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://mail.example.org" {
		t.Fatalf("servers = %#v, want https", doc.Servers)
	}
}

// TestForceHTTPSRelayPaths pins the one scheme-redirect exception: a WebSocket
// upgrade through a trusted proxy may pass so a relay behind a TLS front that
// omits X-Forwarded-Proto on upgrade requests is not 308'd (a WebSocket client
// cannot follow that redirect). Everything else on the relay paths keeps the
// redirect, in particular POST /relay/enroll, which returns a fresh relay
// secret and must not be minted over cleartext (D141).
func TestForceHTTPSRelayPaths(t *testing.T) {
	t.Parallel()
	const proxyAddr = "10.1.1.18:53124"
	h := forceHTTPSHandlerWith(t, func(cfg *config.Config) {
		cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.1.1.18/32")}
	})
	upgrade := func(req *http.Request) *http.Request {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		return req
	}
	tests := []struct {
		name   string
		method string
		path   string
		remote string
		proto  string
		ws     bool
		want   int
	}{
		// The enrollment POST returns a new relay secret, so it is never exempt:
		// a caller reaching a mistakenly published port over cleartext, or a
		// trusted proxy that reports no scheme, both get the canonical 308.
		{name: "enroll direct cleartext", method: http.MethodPost, path: "/relay/enroll", remote: "203.0.113.9:4000", want: http.StatusPermanentRedirect},
		{name: "enroll trusted proxy without proto", method: http.MethodPost, path: "/relay/enroll", remote: proxyAddr, want: http.StatusPermanentRedirect},
		{name: "enroll trusted proxy over https", method: http.MethodPost, path: "/relay/enroll", remote: proxyAddr, proto: "https", want: http.StatusForbidden},
		{name: "enroll trusted proxy with upgrade headers", method: http.MethodPost, path: "/relay/enroll", remote: proxyAddr, ws: true, want: http.StatusPermanentRedirect},

		// A direct cleartext upgrade is still redirected, so an exposed port
		// cannot be used to open a plaintext relay connection.
		{name: "upgrade direct cleartext", method: http.MethodGet, path: "/relay", remote: "203.0.113.9:4000", ws: true, want: http.StatusPermanentRedirect},
		// The UAT case: trusted Traefik omits X-Forwarded-Proto on the upgrade.
		// Reaching the handler (401, no bearer token) proves it was not
		// redirected.
		{name: "upgrade through trusted proxy without proto", method: http.MethodGet, path: "/relay", remote: proxyAddr, ws: true, want: http.StatusUnauthorized},
		{name: "upgrade through trusted proxy over https", method: http.MethodGet, path: "/relay", remote: proxyAddr, proto: "https", ws: true, want: http.StatusUnauthorized},
		// Only an actual upgrade is exempt; an ordinary GET on /relay redirects.
		{name: "plain GET through trusted proxy", method: http.MethodGet, path: "/relay", remote: proxyAddr, want: http.StatusPermanentRedirect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.Reader
			if tt.method == http.MethodPost {
				body = strings.NewReader(`{"enrollmentToken":"nope","gatewayId":"test"}`)
			}
			req := httptest.NewRequest(tt.method, tt.path, body)
			req.RemoteAddr = tt.remote
			if tt.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			if tt.ws {
				upgrade(req)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Fatalf("%s %s = %d body=%q, want %d", tt.method, tt.path, rr.Code, rr.Body.String(), tt.want)
			}
			if tt.want == http.StatusPermanentRedirect {
				if loc := rr.Header().Get("Location"); loc != "https://mail.example.test"+tt.path {
					t.Fatalf("%s %s redirect Location = %q", tt.method, tt.path, loc)
				}
			}
		})
	}
}
