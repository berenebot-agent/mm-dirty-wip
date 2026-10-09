package httpapp_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestExpiredSessionComposePreserved proves a compose POST with no session is
// redirected to login carrying both a post-login `next` back to the form and a
// flash that preserves the typed message, and that the login page carries the
// notice and the next path forward.
func TestExpiredSessionComposePreserved(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"to":      "friend@example.net",
		"subject": "hi",
		"text":    "the body that must survive",
		"action":  "send",
	} {
		_ = mw.WriteField(k, v)
	}
	attachment, _ := mw.CreateFormFile("attachments", "kept.txt")
	attachment.Write([]byte("attachment bytes"))
	mw.Close()

	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// No session cookie: the session has expired.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("expired compose POST status = %d, want 303", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?") || !strings.Contains(loc, "next=") || !strings.Contains(loc, "_flash=") {
		t.Fatalf("expired compose redirect = %q, want /login?next=...&_flash=...", loc)
	}

	// Following the login redirect renders the notice and carries next forward.
	loginRR := httptest.NewRecorder()
	loginReq := httptest.NewRequest("GET", loc, nil)
	h.ServeHTTP(loginRR, loginReq)
	if loginRR.Code != 200 {
		t.Fatalf("login page %d", loginRR.Code)
	}
	loginBody := loginRR.Body.String()
	if !strings.Contains(loginBody, "session expired") {
		t.Fatalf("login page missing the expiry notice")
	}
	if !strings.Contains(loginBody, `name="next" value="/ui/inboxes/`+box.ID+`/compose?_flash=`) {
		t.Fatalf("login page missing the next hidden field: %s", loginBody)
	}
	// Sign in through the real handler and verify restoration, not merely the
	// intermediate redirect. A failed attempt must keep the resume destination.
	start := strings.Index(loginBody, `name="next" value="`) + len(`name="next" value="`)
	end := strings.Index(loginBody[start:], `"`)
	next := loginBody[start : start+end]
	csrfCookie := loginRR.Result().Cookies()[0]
	post := func(password string) *httptest.ResponseRecorder {
		form := url.Values{"email": {u.Email}, "password": {password}, "_csrf": {csrfCookie.Value}, "next": {next}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrfCookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	failed := post("wrong password")
	if !strings.Contains(failed.Header().Get("Location"), "next=") {
		t.Fatal("failed login lost resume destination")
	}
	// Fixture password is shared by the HTTP tests.
	_ = svc
	signed := post("correct horse battery staple")
	if signed.Header().Get("Location") != next {
		t.Fatalf("login destination: %q", signed.Header().Get("Location"))
	}
	resume := httptest.NewRequest("GET", next, nil)
	for _, c := range signed.Result().Cookies() {
		resume.AddCookie(c)
	}
	restored := httptest.NewRecorder()
	h.ServeHTTP(restored, resume)
	if !strings.Contains(restored.Body.String(), "the body that must survive") {
		t.Fatal("compose body was lost after sign-in")
	}
	if !strings.Contains(restored.Body.String(), "kept.txt") {
		t.Fatal("uploaded attachment lost during sign-in")
	}
}

// TestExpiredSessionNonComposeGetsNotice proves a non-compose POST with no
// session gets the plain expiry notice (no content to preserve).
func TestExpiredSessionNonComposeGetsNotice(t *testing.T) {
	_, h, _, _, box := httpFixture(t)
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/edit", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if strings.Contains(loc, "next=") {
		t.Fatalf("non-compose POST should not carry next: %q", loc)
	}
	if !strings.Contains(loc, "login") || !strings.Contains(loc, "_flash=") {
		t.Fatalf("non-compose POST should redirect to login with a notice: %q", loc)
	}
}
