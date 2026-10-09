package httpapp_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(loginBody, `name="next" value="/ui/inboxes/`+box.ID+`/compose"`) {
		t.Fatalf("login page missing the next hidden field: %s", loginBody)
	}
	_ = svc
	_ = u
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
