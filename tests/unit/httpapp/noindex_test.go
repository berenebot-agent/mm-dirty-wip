package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// noIndexHandler builds a minimal server with the given NOINDEX setting, so the
// middleware and the /robots.txt route can be exercised without a full fixture.
func noIndexHandler(t *testing.T, noIndex bool) http.Handler {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{
		DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted",
		AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901",
		MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour,
		LoginLimitPerMinute: 20, SendLimitPerMinute: 60, NoIndex: noIndex,
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes); err != nil {
		t.Fatal(err)
	}
	return httpapp.New(svc, nil).Handler()
}

// TestNoIndexSkipsAPIAndDiscovery is the core contract: the human UI is marked,
// but the REST API and the discovery document are not, so a noindex deployment
// can still be bootstrapped by an agent.
func TestNoIndexSkipsAPIAndDiscovery(t *testing.T) {
	t.Parallel()
	h := noIndexHandler(t, true)
	for _, tc := range []struct {
		path      string
		wantIndex string
	}{
		{"/login", "noindex, nofollow"},
		{"/setup", "noindex, nofollow"},
		{"/v1/health", ""},
		{"/.well-known/mailmoose", ""},
		{"/openapi.json", ""},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Accept", "text/html")
		h.ServeHTTP(rr, req)
		if got := rr.Header().Get("X-Robots-Tag"); got != tc.wantIndex {
			t.Errorf("GET %s X-Robots-Tag = %q, want %q", tc.path, got, tc.wantIndex)
		}
	}
}

// TestNoIndexOffByDefault proves the default keeps the UI indexable, so the
// hosted deployment's signup pages stay discoverable unless NOINDEX is set.
func TestNoIndexOffByDefault(t *testing.T) {
	t.Parallel()
	h := noIndexHandler(t, false)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rr, req)
	if got := rr.Header().Get("X-Robots-Tag"); got != "" {
		t.Errorf("X-Robots-Tag = %q, want empty when NOINDEX is off", got)
	}
	if body := rr.Body.String(); strings.Contains(body, "noindex") {
		t.Error("login page carries a robots meta element while NOINDEX is off")
	}
}

// TestNoIndexMetaMatchesHeader keeps the HTML meta element and the HTTP header
// in agreement, since some crawlers read only one of the two.
func TestNoIndexMetaMatchesHeader(t *testing.T) {
	t.Parallel()
	h := noIndexHandler(t, true)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept", "text/html")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `<meta name="robots" content="noindex,nofollow">`) {
		t.Errorf("login page is missing the robots meta element: %s", rr.Body.String())
	}
}

// TestRobotsTxt proves the blanket disallow is served only under NOINDEX, and
// that the plain "no such file" response is returned otherwise.
func TestRobotsTxt(t *testing.T) {
	t.Parallel()
	// Serial-ish note: the handler is cheap and stateless, so this runs in
	// parallel with the others.

	h := noIndexHandler(t, true)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/robots.txt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /robots.txt = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if body := rr.Body.String(); body != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt body = %q", body)
	}

	off := noIndexHandler(t, false)
	rr = httptest.NewRecorder()
	off.ServeHTTP(rr, httptest.NewRequest("GET", "/robots.txt", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /robots.txt with NOINDEX off = %d, want 404", rr.Code)
	}
}
