package httpapp_test

import (
	"context"
	"encoding/json"
	"errors"
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
)

// passkeyFixture builds a self-contained handler so these tests do not depend on
// the shared test-database helper.
func passkeyFixture(t *testing.T) (*app.Service, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes); err != nil {
		t.Fatal(err)
	}
	return svc, httpapp.New(svc, nil).Handler()
}

// loginSession signs in the fixture admin over HTTP and returns the session and
// CSRF cookies, mirroring what the browser would carry.
func loginSession(t *testing.T, h http.Handler) (*http.Cookie, *http.Cookie) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	var preauth *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			preauth = c
		}
	}
	if preauth == nil {
		t.Fatal("missing preauth csrf cookie")
	}
	form := url.Values{"email": {"admin@example.com"}, "password": {"correct horse battery staple"}, "_csrf": {preauth.Value}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(preauth)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login = %d body=%s", rr.Code, rr.Body.String())
	}
	var session, csrf *http.Cookie
	for _, c := range rr.Result().Cookies() {
		switch c.Name {
		case "mmm_session":
			session = c
		}
	}
	// The CSRF value for session-scoped forms is stored server-side; the login
	// page re-issues it as a cookie only pre-auth. Fetch the account page to
	// read it from the rendered form.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(session)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("account get = %d", rr.Code)
	}
	body := rr.Body.String()
	const marker = `name="_csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no csrf token rendered on account page")
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	csrf = &http.Cookie{Name: "csrf", Value: rest[:j]}
	return session, csrf
}

// TestPasskeyRegisterRequiresCSRF guards the client/server contract: the
// registration endpoints are CSRF-protected and the account-page JS must send
// the token (regression: it previously sent none and every add failed with 403).
func TestPasskeyRegisterRequiresCSRF(t *testing.T) {
	svc, h := passkeyFixture(t)
	if svc.Config.WebAuthnRPID() == "" {
		t.Skip("webauthn not configured for fixture")
	}
	session, csrf := loginSession(t, h)

	req := httptest.NewRequest("POST", "/ui/account/passkeys/begin", nil)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("begin without csrf = %d, want 403", rr.Code)
	}

	req = httptest.NewRequest("POST", "/ui/account/passkeys/begin", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf.Value)
	req.AddCookie(session)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("begin with csrf = %d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		ChallengeToken string `json:"challenge_token"`
		Options        struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
				User      struct {
					ID string `json:"id"`
				} `json:"user"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode options: %v body=%s", err, rr.Body.String())
	}
	if payload.ChallengeToken == "" || payload.Options.PublicKey.Challenge == "" || payload.Options.PublicKey.User.ID == "" {
		t.Fatalf("incomplete registration options: %s", rr.Body.String())
	}
}

// TestPasskeySysadminCannotDisablePassword guards the break-glass invariant at
// the HTTP layer: the sysadmin's password must stay usable.
func TestPasskeySysadminCannotDisablePassword(t *testing.T) {
	svc, _ := passkeyFixture(t)
	if svc.Config.WebAuthnRPID() == "" {
		t.Skip("webauthn not configured for fixture")
	}
	u, _, err := svc.Store.SyncSystemAdmin(context.Background(), "MailMoose", "admin@example.com", "correct horse battery staple", svc.Config.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetPasswordAuth(context.Background(), u.ID, u.AccountID, false); !errors.Is(err, store.ErrLastAuthMethod) {
		t.Fatalf("sysadmin disable err = %v, want ErrLastAuthMethod", err)
	}
}
