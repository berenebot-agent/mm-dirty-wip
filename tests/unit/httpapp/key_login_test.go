package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// keyLogin signs in through the HTML login form with an API key and returns the
// resulting session cookie, or nil when the login is refused.
func keyLogin(t *testing.T, h http.Handler, key string) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("login GET = %d", rr.Code)
	}
	var csrf *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			csrf = c
		}
	}
	if csrf == nil {
		t.Fatal("no pre-auth CSRF cookie on /login")
	}
	form := url.Values{"_csrf": {csrf.Value}, "api_key": {key}}
	req := httptest.NewRequest("POST", "/login/key", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("key login POST = %d body=%s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
			return c
		}
	}
	return nil
}

// TestKeyLoginShowsOperatorScope verifies a mailbox-scoped API key can sign in
// and lands on the operator view, scoped to its mailboxes, with no admin
// controls and no Admin plane.
func TestKeyLoginShowsOperatorScope(t *testing.T) {
	svc, h, u, d, box := httpFixture(t)
	ctx := context.Background()
	other, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "sales", "Sales")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "agent", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	session := keyLogin(t, h, key)
	if session == nil {
		t.Fatal("mailbox-scoped key did not set a session cookie")
	}

	rr := uiGet(t, h, session, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("key session / = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, box.Address) {
		t.Fatalf("key session dashboard missing assigned mailbox %q", box.Address)
	}
	if strings.Contains(body, other.Address) {
		t.Fatalf("key session leaked unassigned mailbox %q", other.Address)
	}
	for _, banned := range []string{`id="add-inbox"`, "/admin", "Add Client", "Add Domain"} {
		if strings.Contains(body, banned) {
			t.Fatalf("key session rendered admin control %q", banned)
		}
	}
	if rr := uiGet(t, h, session, "/admin"); rr.Code != http.StatusForbidden {
		t.Fatalf("key session /admin = %d, want 403", rr.Code)
	}
	// The Account page is the slim key-session variant, not the human one.
	rr = uiGet(t, h, session, "/account")
	if rr.Code != http.StatusOK {
		t.Fatalf("key session /account = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Signed in with an API key") {
		t.Fatal("key session /account missing key-session view")
	}
	for _, banned := range []string{"Change password", "Change email address", "Passkeys", "Account administration", "Mailbox operators"} {
		if strings.Contains(rr.Body.String(), banned) {
			t.Fatalf("key session /account rendered human control %q", banned)
		}
	}
}

// TestKeyLoginRejectsAdminKey verifies an admin key cannot sign in to the HTML
// UI: this path maps mailbox access, not admin mode.
func TestKeyLoginRejectsAdminKey(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "full", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session := keyLogin(t, h, key); session != nil {
		t.Fatal("admin key obtained a session cookie")
	}
}

// TestKeyLoginRejectsUnknownKey verifies an unknown key is refused with a
// redirect back to the login form (no session).
func TestKeyLoginRejectsUnknownKey(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	if session := keyLogin(t, h, "mmm_not-a-real-key"); session != nil {
		t.Fatal("unknown key obtained a session cookie")
	}
}

// TestKeySessionDiesOnRevoke verifies revoking a key invalidates a browser
// session already minted from it.
func TestKeySessionDiesOnRevoke(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	k, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "agent", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	session := keyLogin(t, h, key)
	if session == nil {
		t.Fatal("key login failed")
	}
	// /account is session-guarded: it works before revoke and redirects to
	// /login after, which is the definitive authentication signal (the root
	// path serves discovery to non-HTML clients even without a session).
	if rr := uiGet(t, h, session, "/account"); rr.Code != http.StatusOK {
		t.Fatalf("session should work before revoke: %d", rr.Code)
	}
	if err := svc.Store.RevokeAPIKey(ctx, u.AccountID, k.ID); err != nil {
		t.Fatal(err)
	}
	rr := uiGet(t, h, session, "/account")
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "/login") {
		t.Fatalf("revoked key session still authenticated: %d %q", rr.Code, rr.Header().Get("Location"))
	}
}

// TestKeySessionReadOnlyStaysReadOnly verifies a Read-scoped key session cannot
// send from its own mailbox, even though it can open the mailbox.
func TestKeySessionReadOnlyStaysReadOnly(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	session := keyLogin(t, h, key)
	if session == nil {
		t.Fatal("read key login failed")
	}
	seedInbound(t, svc, box, "key-ro-1", "<key-ro@test>", "Hello", "body")
	// A read key may open the mailbox.
	if rr := uiGet(t, h, session, "/ui/inboxes/"+box.ID); rr.Code != http.StatusOK {
		t.Fatalf("read key mailbox view = %d", rr.Code)
	}
	// But its send POST is refused: a Read role cannot send. The refusal and a
	// success both redirect (303), so assert no message was queued.
	domainPost(t, h, session, "/ui/inboxes/"+box.ID+"/send", url.Values{
		"_csrf": {csrfFromInbox(t, h, session, box.ID)},
		"to":    {"friend@example.net"}, "subject": {"Hi"}, "text": {"body"},
	})
	owner := model.Principal{AccountID: u.AccountID, Admin: true}
	queued, err := svc.Store.ListOutbox(ctx, owner, box.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("read-only key session queued %d send(s)", len(queued))
	}
}

// csrfFromInbox reads the CSRF token off an inbox view (any authenticated page
// render carries it in a form).
func csrfFromInbox(t *testing.T, h http.Handler, session *http.Cookie, inboxID string) string {
	t.Helper()
	rr := uiGet(t, h, session, "/ui/inboxes/"+inboxID)
	if rr.Code != http.StatusOK {
		t.Fatalf("csrf source inbox = %d", rr.Code)
	}
	body := rr.Body.String()
	const marker = `name="_csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no csrf token on inbox view")
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	return rest[:j]
}
