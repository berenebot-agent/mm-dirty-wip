package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// systemAdminFixture builds a service whose bootstrap user is the installation
// system administrator (rather than an ordinary account Admin), with a domain
// and a mailbox of their own.
func systemAdminFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Inbox) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 50, SendLimitPerMinute: 60, AdminAccountName: "MailMoose"}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := st.SyncSystemAdmin(context.Background(), "MailMoose", "root@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "mailer", "Mailer")
	if err != nil {
		t.Fatal(err)
	}
	return svc, httpapp.New(svc, nil).Handler(), u, box
}

func inviteToken(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`/invite/([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no invite link found in body:\n%s", body)
	}
	return m[1]
}

// createInvite posts an invite form as the system administrator and returns the
// one-time setup token from the flashed link.
func systemAdminInvite(t *testing.T, h http.Handler, cookie *http.Cookie, csrf, path string, form url.Values) string {
	t.Helper()
	form.Set("_csrf", csrf)
	rr := domainPost(t, h, cookie, path, form)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create invite status = %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	flash := ""
	if i := strings.Index(loc, "_flash="); i >= 0 {
		flash = loc[i+len("_flash="):]
	}
	page := "/admin"
	if strings.HasPrefix(path, "/ui/members") {
		page = "/members"
	}
	rr = uiGet(t, h, cookie, page+"?_flash="+flash)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin page status = %d", rr.Code)
	}
	return inviteToken(t, rr.Body.String())
}

// acceptInvite walks the public setup flow and returns the resulting session.
func acceptInvite(t *testing.T, h http.Handler, token, password string) (*http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/invite/"+token, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("invite GET = %d", rr.Code)
	}
	var csrf *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			csrf = c
		}
	}
	if csrf == nil {
		t.Fatal("missing pre-auth csrf cookie")
	}
	form := url.Values{"_csrf": {csrf.Value}, "password": {password}, "confirm_password": {password}}
	req := httptest.NewRequest("POST", "/invite/"+token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrf)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invite POST = %d body=%s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
			return c, rr
		}
	}
	t.Fatal("invite POST did not set a session cookie")
	return nil, nil
}

func TestAdminPlaneRequiresSystemAdmin(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusForbidden {
		t.Fatalf("account admin /admin = %d, want 403", rr.Code)
	}
	if rr := uiGet(t, h, cookie, "/members"); rr.Code != http.StatusOK {
		t.Fatalf("account admin /members = %d, want 200", rr.Code)
	}
}

func TestSystemAdminInvitesNewAccount(t *testing.T) {
	svc, h, root, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, root.ID)
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusOK {
		t.Fatalf("system admin /admin = %d", rr.Code)
	}
	token := systemAdminInvite(t, h, cookie, csrf, "/ui/admin/invites", url.Values{
		"kind": {"account_admin"}, "email": {"new@example.com"}, "account_name": {"New Co"},
	})
	session, _ := acceptInvite(t, h, token, "correct horse battery staple")
	if rr := uiGet(t, h, session, "/"); rr.Code != http.StatusOK {
		t.Fatalf("new admin dashboard = %d", rr.Code)
	}
	created, err := svc.Store.GetUserByEmail(context.Background(), "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !created.IsAdmin || created.SystemAdmin {
		t.Fatalf("invited user roles = %#v", created)
	}
	if created.AccountID == root.AccountID {
		t.Fatal("invited admin must get a separate account")
	}
}

func TestMembersInviteOperatorGrantsSelectedMailboxOnly(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	d, err := svc.Store.GetDomain(ctx, root.AccountID, mailerBox.DomainID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Store.CreateInbox(ctx, root.AccountID, d.ID, "sales", "Sales")
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, root.ID)
	token := systemAdminInvite(t, h, cookie, csrf, "/ui/members/invites", url.Values{
		"kind": {"operator"}, "email": {"op@example.com"}, "inboxes": {mailerBox.ID},
	})
	session, _ := acceptInvite(t, h, token, "correct horse battery staple")
	if rr := uiGet(t, h, session, "/"); rr.Code != http.StatusOK {
		t.Fatalf("operator dashboard = %d", rr.Code)
	}
	if rr := uiGet(t, h, session, "/ui/inboxes/"+mailerBox.ID); rr.Code != http.StatusOK {
		t.Fatalf("operator assigned inbox = %d, want 200", rr.Code)
	}
	if rr := uiGet(t, h, session, "/ui/inboxes/"+other.ID); rr.Code != http.StatusNotFound {
		t.Fatalf("operator unassigned inbox = %d, want 404", rr.Code)
	}
	if rr := uiGet(t, h, session, "/admin"); rr.Code != http.StatusForbidden {
		t.Fatalf("operator /admin = %d, want 403", rr.Code)
	}
}

func TestSendInviteQueuesFromSystemMailer(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	if err := svc.Store.SetSystemMailerInbox(ctx, root.AccountID, mailerBox.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, root.ID)
	// Create the invite, then send it. The setup link is rotated by the send.
	token := systemAdminInvite(t, h, cookie, csrf, "/ui/admin/invites", url.Values{
		"kind": {"account_admin"}, "email": {"queued@example.com"},
	})
	invites, err := svc.Store.ListInvites(ctx, "")
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %d, %v", len(invites), err)
	}
	rr := domainPost(t, h, cookie, "/ui/admin/invites/"+invites[0].ID+"/send", url.Values{"_csrf": {csrf}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("send invite = %d body=%s", rr.Code, rr.Body.String())
	}
	admin := model.Principal{AccountID: root.AccountID, Admin: true}
	outbox, err := svc.Store.ListOutbox(ctx, admin, mailerBox.ID, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox = %d, %v; want 1 queued invitation", len(outbox), err)
	}
	if outbox[0].To[0] != "queued@example.com" {
		t.Fatalf("queued invitation recipient = %v", outbox[0].To)
	}
	// The previously copied link is invalidated by the send rotation.
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequest("GET", "/invite/"+token, nil))
	if strings.Contains(stale.Body.String(), "Set your password") {
		t.Fatalf("stale link still redeemable: status=%d", stale.Code)
	}
}

func TestSystemAdminCannotChangeLoginInUI(t *testing.T) {
	svc, h, root, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, root.ID)
	rr := domainPost(t, h, cookie, "/ui/account/password", url.Values{"_csrf": {csrf}, "current_password": {"correct horse battery staple"}, "new_password": {"another correct horse battery staple"}, "confirm_password": {"another correct horse battery staple"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("system admin password change = %d", rr.Code)
	}
	if _, err := svc.Store.AuthenticateUser(context.Background(), "root@example.com", "another correct horse battery staple"); err == nil {
		t.Fatal("system admin password must not change from the UI")
	}
}
