package httpapp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
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
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// csrfFromLogin performs GET /login and returns the pre-auth CSRF cookie.
func csrfFromLogin(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			return c
		}
	}
	t.Fatal("missing pre-auth CSRF cookie")
	return nil
}

// TestSafeNextPathRejectsControlCharacters covers the finding-3 open redirect:
// an embedded tab or newline survives into the Location header, and a browser
// strips it, turning "/<TAB>/evil.com" into the off-site "//evil.com".
func TestSafeNextPathRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	_, h, u, _, _ := httpFixture(t)
	csrf := csrfFromLogin(t, h)

	cases := []string{
		"/\t/evil.com",
		"/\n/evil.com",
		"/\r/evil.com",
		"/\x7f/evil.com",
		"//evil.com",
		"/\\evil.com",
		"https://evil.com",
		"/ok/path",
		"/ui/inboxes/abc/compose?_flash=xyz",
	}
	for _, next := range cases {
		form := url.Values{"email": {u.Email}, "password": {"wrong-password"}, "_csrf": {csrf.Value}, "next": {next}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		// A failed login re-renders via flashAuth; the embedded next is echoed
		// in the login redirect. A successful login would emit the Location
		// directly. Either way, no control character and no off-site host may
		// reach a Location header.
		loc := rr.Header().Get("Location")
		for _, r := range loc {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("next=%q produced a control character in Location %q", next, loc)
			}
		}
		if strings.Contains(loc, "evil.com") {
			t.Fatalf("next=%q produced an off-site Location %q", next, loc)
		}
	}
}

// TestMultipartCSRFViaHiddenField covers finding 5: the CSRF token for a
// multipart compose/reply form must be read from the hidden form field, not
// from a query string. actionWithCSRF must not append it to the URL.
func TestMultipartCSRFViaHiddenField(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	// The compose form must not carry a CSRF token in the action URL.
	getRR := httptest.NewRecorder()
	getReq := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/compose", nil)
	getReq.AddCookie(cookie)
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != 200 {
		t.Fatalf("compose GET %d", getRR.Code)
	}
	if strings.Contains(getRR.Body.String(), "/send?_csrf=") {
		t.Fatal("compose form action still carries ?_csrf=")
	}

	// A multipart POST with the token only in the hidden field must be accepted
	// (not 403). A send with no configured outbound provider still validates
	// CSRF first; a 403 here would mean the multipart field was not read.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("_csrf", csrf)
	_ = mw.WriteField("to", "friend@example.net")
	_ = mw.WriteField("subject", "hi")
	_ = mw.WriteField("text", "body")
	_ = mw.WriteField("action", "draft")
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("multipart CSRF hidden field was not honoured: %d %s", rr.Code, rr.Body.String())
	}

	// A multipart POST with a wrong token must still be refused.
	var bad bytes.Buffer
	bw := multipart.NewWriter(&bad)
	_ = bw.WriteField("_csrf", "wrong-token")
	_ = bw.WriteField("to", "friend@example.net")
	_ = bw.WriteField("text", "body")
	_ = bw.Close()
	badReq := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", &bad)
	badReq.Header.Set("Content-Type", bw.FormDataContentType())
	badReq.AddCookie(cookie)
	badRR := httptest.NewRecorder()
	h.ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusForbidden {
		t.Fatalf("wrong multipart CSRF token accepted: %d", badRR.Code)
	}
}

// TestAnonymousComposeNotPreserved covers finding 6: an unauthenticated compose
// POST (no session cookie at all) must not parse its body or create a flash.
func TestAnonymousComposeNotPreserved(t *testing.T) {
	t.Parallel()
	_, h, _, _, box := httpFixture(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("to", "friend@example.net")
	_ = mw.WriteField("subject", "anonymous")
	_ = mw.WriteField("text", "must not be stored")
	att, _ := mw.CreateFormFile("attachments", "big.bin")
	_, _ = att.Write(bytes.Repeat([]byte("A"), 1<<20))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// No mmm_session cookie at all.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("anonymous compose POST status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	// The redirect carries only the plain expiry notice. What must NOT appear is
	// a preserved compose: no next= back to the compose form and no draft flash.
	if strings.Contains(loc, "next=") {
		t.Fatalf("anonymous compose POST preserved a next path: %q", loc)
	}
	if strings.Contains(loc, "compose") {
		t.Fatalf("anonymous compose POST redirected back to compose: %q", loc)
	}
}

// TestRateLimitKeyNormalizesIPv6 covers finding 7: an IPv6 client's whole /64
// collapses to one limiter key, so it cannot rotate addresses within its prefix
// to burn distinct entries.
func TestRateLimitKeyNormalizesIPv6(t *testing.T) {
	t.Parallel()
	// rateLimitKey is unexported; assert the behaviour through the limiter path
	// by exercising the login throttle from two addresses in the same /64.
	_, h, u, _, _ := httpFixture(t)
	csrf := csrfFromLogin(t, h)

	post := func(addr string) int {
		v := url.Values{"email": {u.Email}, "password": {"wrong-password"}, "_csrf": {csrf.Value}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr
		req.AddCookie(csrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	// The login limiter is configured with a small per-minute cap. Fire enough
	// failed attempts from the first address to exhaust the /64 bucket, then a
	// login from a second address in the same /64 must be refused too.
	var last int
	for i := 0; i < 30; i++ {
		last = post("[2001:db8::1]:1234")
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("first /64 address was never throttled (last=%d)", last)
	}
	if got := post("[2001:db8::2]:1234"); got != http.StatusTooManyRequests {
		t.Fatalf("second /64 address not throttled with the first: %d", got)
	}
	// A genuinely different /64 is a different IP bucket. Use a different
	// account too, so the per-account limiter (correctly) does not fire: this
	// asserts the IP keying specifically.
	otherEmail := "other-" + u.Email
	postOther := func(addr string) int {
		v := url.Values{"email": {otherEmail}, "password": {"wrong-password"}, "_csrf": {csrf.Value}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr
		req.AddCookie(csrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := postOther("[2001:db8:1::2]:1234"); got == http.StatusTooManyRequests {
		t.Fatalf("distinct /64 was throttled by the first: %d", got)
	}
}

// TestPerAccountLoginThrottle covers finding 7's second half: repeated attempts
// at one account are throttled even from a fresh source address.
func TestPerAccountLoginThrottle(t *testing.T) {
	t.Parallel()
	_, h, u, _, _ := httpFixture(t)
	csrf := csrfFromLogin(t, h)
	throttled := false
	for i := 0; i < 40; i++ {
		v := url.Values{"email": {u.Email}, "password": {"wrong-password"}, "_csrf": {csrf.Value}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// A distinct /64 each time, so the per-IP limiter cannot be the cause:
		// only the per-account limiter can produce a 429 here.
		req.RemoteAddr = fmt.Sprintf("[2001:db8:%x::1]:1234", i+1)
		req.AddCookie(csrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("no per-account throttle: 40 attempts from distinct addresses were all admitted")
	}
}

// TestInboundPerIPCap covers finding 9: a single source cannot hold every shared
// inbound slot. The handler takes the per-IP slot before the shared semaphore
// and before the provider reads the body, so once one address holds its cap a
// further concurrent request is refused with 429.
func TestInboundPerIPCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	defer st.Close()
	// A small inbound pool makes the per-IP cap small (4/4 clamped up to 2).
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60, InboundConcurrency: 4}
	svc, _ := app.New(cfg, st, events.NewHub())
	h := httpapp.New(svc, nil).Handler()

	// The Resend transport reads the whole body before doing anything else, so a
	// pipe whose writer stays open parks each handler after it has taken its
	// per-IP slot. Fire more requests than the cap and confirm at least one is
	// refused before any body is delivered.
	const n = 5
	type result struct {
		code int
	}
	results := make(chan result, n)
	writers := make([]*io.PipeWriter, 0, n)
	started := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		go func() {
			req := httptest.NewRequest("POST", "/internal/ingest/resend", pr)
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = "[2001:db8::9]:1234"
			rr := httptest.NewRecorder()
			started <- struct{}{}
			h.ServeHTTP(rr, req)
			results <- result{code: rr.Code}
		}()
	}
	for i := 0; i < n; i++ {
		<-started
	}
	// Let the handlers reach the per-IP acquire; the ones over the cap return
	// immediately with 429 while the rest stay parked on the body read.
	time.Sleep(200 * time.Millisecond)
	for _, w := range writers {
		_ = w.Close()
	}
	refused := 0
	for i := 0; i < n; i++ {
		if (<-results).code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("per-IP inbound cap did not refuse any of %d concurrent requests from one address", n)
	}
}

// passkeyOriginFixture builds a handler for a given BASE_URL with one admin
// account, so the passkey secure-context gate can be exercised per origin.
func passkeyOriginFixture(t *testing.T, baseURL string) (*app.Service, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: baseURL, Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes); err != nil {
		t.Fatal(err)
	}
	return svc, httpapp.New(svc, nil).Handler()
}

// signIn signs the fixture admin in and returns the session cookie.
func signIn(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	csrf := csrfFromLogin(t, h)
	v := url.Values{"email": {"admin@example.com"}, "password": {"correct horse battery staple"}, "_csrf": {csrf.Value}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login = %d body=%s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// TestPasskeysRequireSecureContext covers the passkey gating: a plain-HTTP
// deployment reached by IP or hostname must not offer passkeys (the browser
// refuses WebAuthn outside a secure context), and its account page must explain
// why instead of rendering controls. An https origin, and http on localhost,
// keep passkeys enabled.
func TestPasskeysRequireSecureContext(t *testing.T) {
	t.Parallel()
	t.Run("http by hostname is disabled", func(t *testing.T) {
		t.Parallel()
		_, h := passkeyOriginFixture(t, "http://mail.example.test")
		// The login page must not offer the passkey button.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
		if strings.Contains(rr.Body.String(), "passkey-signin") {
			t.Fatal("login page offers passkey sign-in over plain HTTP")
		}
		// The account page must explain the requirement, not show the controls.
		session := signIn(t, h)
		rr = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/account", nil)
		req.AddCookie(session)
		h.ServeHTTP(rr, req)
		body := rr.Body.String()
		if strings.Contains(body, `id="passkey-add"`) {
			t.Fatal("account page shows passkey-add over plain HTTP")
		}
		if !strings.Contains(body, "Passkeys need HTTPS") {
			t.Fatal("account page does not explain the HTTPS requirement")
		}
	})
	t.Run("https is enabled", func(t *testing.T) {
		t.Parallel()
		_, h := passkeyOriginFixture(t, "https://mail.example.test")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
		if !strings.Contains(rr.Body.String(), "passkey-signin") {
			t.Fatal("login page omits passkey sign-in over HTTPS")
		}
	})
	t.Run("http on localhost is enabled", func(t *testing.T) {
		t.Parallel()
		_, h := passkeyOriginFixture(t, "http://localhost:8081")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
		if !strings.Contains(rr.Body.String(), "passkey-signin") {
			t.Fatal("login page omits passkey sign-in on localhost")
		}
	})
}
