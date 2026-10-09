package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestFeedbackButtonOnSignedInShell asserts the global header exposes the
// feedback entry point and its mailto target once a user is signed in.
func TestFeedbackButtonOnSignedInShell(t *testing.T) {
	_, h, u, _, _ := httpFixture(t)

	csrf := loginCSRFCookie(t, h)
	form := url.Values{"email": {u.Email}, "password": {"correct horse battery staple"}, "_csrf": {csrf.Value}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("login = %d body=%s", rr.Code, rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie")
	}

	req = httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(session)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("GET /account = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `id="feedback-open"`) {
		t.Fatalf("signed-in shell has no feedback button")
	}
	if !strings.Contains(body, `mailto:mailmoose@hgolabs.com`) {
		t.Fatalf("signed-in shell has no feedback mailto link")
	}
}

// TestFeedbackAbsentWhenSignedOut asserts the feedback button is not shown on
// the public login page, since the shell only renders it for a session.
func TestFeedbackAbsentWhenSignedOut(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /login = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), `id="feedback-open"`) {
		t.Fatal("feedback button should not render when signed out")
	}
}

// loginCSRFCookie fetches the login page and returns its pre-auth CSRF cookie.
func loginCSRFCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /login = %d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			return c
		}
	}
	t.Fatal("login page did not set a pre-auth CSRF cookie")
	return nil
}
