package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// unconfiguredHandler returns a handler over a fresh database with no users, so
// the first-run setup route is live.
func unconfiguredHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	return st, httpapp.New(svc, nil).Handler()
}

func preAuthCSRFCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/setup", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /setup = %d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			return c
		}
	}
	t.Fatal("GET /setup did not set a pre-auth CSRF cookie")
	return nil
}

func TestSetupPageRendersForm(t *testing.T) {
	_, h := unconfiguredHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/setup", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /setup = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `action="/setup"`) {
		t.Fatalf("setup page has no form: %s", body)
	}
	if !strings.Contains(body, `name="_csrf"`) {
		t.Fatalf("setup form missing CSRF field")
	}
}

func TestSetupCreatesAdminAndSignsIn(t *testing.T) {
	st, h := unconfiguredHandler(t)
	csrf := preAuthCSRFCookie(t, h)

	form := url.Values{
		"account":  {"Acme"},
		"email":    {"founder@example.com"},
		"password": {"correct horse battery staple"},
		"confirm":  {"correct horse battery staple"},
		"_csrf":    {csrf.Value},
	}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 || rr.Header().Get("Location") != "/" {
		t.Fatalf("POST /setup = %d location=%q body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("POST /setup did not set a session cookie")
	}

	// The new account's user must be the installation system administrator.
	has, err := st.HasSystemAdmin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("setup did not create a system administrator")
	}
}

func TestSetupSecondClaimRefused(t *testing.T) {
	st, h := unconfiguredHandler(t)
	csrf := preAuthCSRFCookie(t, h)

	claim := func() *httptest.ResponseRecorder {
		form := url.Values{
			"account":  {"Acme"},
			"email":    {"founder@example.com"},
			"password": {"correct horse battery staple"},
			"confirm":  {"correct horse battery staple"},
			"_csrf":    {csrf.Value},
		}
		req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := claim(); rr.Code != 303 {
		t.Fatalf("first claim = %d body=%s", rr.Code, rr.Body.String())
	}
	// A second claim must not run: the route self-disables once a user exists.
	rr := claim()
	if rr.Code != 303 || rr.Header().Get("Location") != "/login" {
		t.Fatalf("second claim = %d location=%q, want 303 -> /login", rr.Code, rr.Header().Get("Location"))
	}
	if has, err := st.HasUsers(context.Background()); err != nil || !has {
		t.Fatalf("users after setup: has=%v err=%v", has, err)
	}
}

func TestSetupGetRedirectsWhenConfigured(t *testing.T) {
	st, h := unconfiguredHandler(t)
	if _, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/setup", nil))
	if rr.Code != 303 || rr.Header().Get("Location") != "/login" {
		t.Fatalf("GET /setup when configured = %d location=%q, want 303 -> /login", rr.Code, rr.Header().Get("Location"))
	}
}

func TestSetupPostRejectsCrossOrigin(t *testing.T) {
	_, h := unconfiguredHandler(t)
	csrf := preAuthCSRFCookie(t, h)

	form := url.Values{
		"account":  {"Acme"},
		"email":    {"founder@example.com"},
		"password": {"correct horse battery staple"},
		"confirm":  {"correct horse battery staple"},
		"_csrf":    {csrf.Value},
	}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site setup = %d, want 403", rr.Code)
	}
}

func TestSetupPostRequiresCSRF(t *testing.T) {
	_, h := unconfiguredHandler(t)
	csrf := preAuthCSRFCookie(t, h)

	form := url.Values{
		"account":  {"Acme"},
		"email":    {"founder@example.com"},
		"password": {"correct horse battery staple"},
		"confirm":  {"correct horse battery staple"},
	}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("setup without csrf = %d, want 403", rr.Code)
	}
}
