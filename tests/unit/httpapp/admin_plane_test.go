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
// system administrator (as well as account Admin of its own account), with a
// domain and a mailbox of their own.
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

// createInvite posts an invite form and returns the one-time setup token from
// the flashed link re-read from flashPage.
func createInvite(t *testing.T, h http.Handler, cookie *http.Cookie, csrf, postPath, flashPage string, form url.Values) string {
	t.Helper()
	form.Set("_csrf", csrf)
	rr := domainPost(t, h, cookie, postPath, form)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create invite %s status = %d body=%s", postPath, rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	flash := ""
	if i := strings.Index(loc, "_flash="); i >= 0 {
		flash = loc[i+len("_flash="):]
	}
	rr = uiGet(t, h, cookie, flashPage+"?_flash="+flash)
	if rr.Code != http.StatusOK {
		t.Fatalf("flash page %s status = %d", flashPage, rr.Code)
	}
	return inviteToken(t, rr.Body.String())
}

// acceptInvite walks the public setup flow and returns the resulting session.
func acceptInvite(t *testing.T, h http.Handler, token, password string) *http.Cookie {
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
			return c
		}
	}
	t.Fatal("invite POST did not set a session cookie")
	return nil
}

func TestAdminPlaneRequiresSystemAdmin(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusForbidden {
		t.Fatalf("account admin /admin = %d, want 403", rr.Code)
	}
	rr := uiGet(t, h, cookie, "/account")
	if rr.Code != http.StatusOK {
		t.Fatalf("account admin /account = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Mailbox users") {
		t.Fatal("account page must show the mailbox users section for account Admins")
	}
}

func TestOperatorsHiddenFromNonAdmin(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, root.ID)
	token := createInvite(t, h, cookie, csrf, "/ui/account/operators/invites", "/account", url.Values{"email": {"op@example.com"}, "inboxes": {mailerBox.ID}})
	op := acceptInvite(t, h, token, "correct horse battery staple")
	rr := uiGet(t, h, op, "/account")
	if rr.Code != http.StatusOK {
		t.Fatalf("operator /account = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Mailbox users") {
		t.Fatal("operators must not see the mailbox users section")
	}
}

func TestSystemAdminInvitesNewAccount(t *testing.T) {
	svc, h, root, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, root.ID)
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusOK {
		t.Fatalf("system admin /admin = %d", rr.Code)
	}
	token := createInvite(t, h, cookie, csrf, "/ui/admin/invites", "/admin", url.Values{"email": {"new@example.com"}, "account_name": {"New Co"}})
	session := acceptInvite(t, h, token, "correct horse battery staple")
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

func TestOperatorInviteGrantsSelectedMailboxOnly(t *testing.T) {
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
	if err := svc.Store.SetInboxAllowedSenders(ctx, root.AccountID, mailerBox.ID, []string{"friend@outside.test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxSenderRestricted(ctx, root.AccountID, mailerBox.ID, true); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, root.ID)
	token := createInvite(t, h, cookie, csrf, "/ui/account/operators/invites", "/account", url.Values{"email": {"op@example.com"}, "inboxes": {mailerBox.ID}})
	// The system admin plane lists accounts only; an operator invite must not
	// leak into it.
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "op@example.com") {
		t.Fatalf("operator invite leaked onto /admin: status=%d", rr.Code)
	}
	session := acceptInvite(t, h, token, "correct horse battery staple")
	_ = seedInbound(t, svc, mailerBox, "op-dash-1", "<op-dash@test>", "Hello operator", "body")
	rr := uiGet(t, h, session, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("operator dashboard = %d", rr.Code)
	}
	body := rr.Body.String()
	// The operator's landing page uses the same inbox table (and counts/status
	// flags) as the account Admin's dashboard.
	for _, want := range []string{">Unread</th>", ">Pending send</th>", ">Size</th>", `class="pill unread-pill">1<`, "issue-dot", mailerBox.Address, "Sender allow list:", "friend@outside.test"} {
		if !strings.Contains(body, want) {
			t.Fatalf("operator dashboard missing %q", want)
		}
	}
	if strings.Contains(body, other.Address) {
		t.Fatalf("operator dashboard leaked unassigned mailbox %q", other.Address)
	}
	// Admin-only controls (Add Inbox, settings gear, delete) must not render.
	for _, banned := range []string{`id="add-inbox"`, "edit-inbox", "/delete"} {
		if strings.Contains(body, banned) {
			t.Fatalf("operator dashboard must not contain admin control %q", banned)
		}
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

func TestAccountMailerSendsOperatorInvite(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	if err := svc.Store.SetAccountMailerInbox(ctx, root.AccountID, mailerBox.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, root.ID)
	createInvite(t, h, cookie, csrf, "/ui/account/operators/invites", "/account", url.Values{"email": {"queued@example.com"}, "inboxes": {mailerBox.ID}})
	invites, err := svc.Store.ListInvites(ctx, root.AccountID)
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %d, %v", len(invites), err)
	}
	rr := domainPost(t, h, cookie, "/ui/account/operators/invites/"+invites[0].ID+"/send", url.Values{"_csrf": {csrf}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("send operator invite = %d body=%s", rr.Code, rr.Body.String())
	}
	admin := model.Principal{AccountID: root.AccountID, Admin: true}
	outbox, err := svc.Store.ListOutbox(ctx, admin, mailerBox.ID, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox = %d, %v; want 1 queued invitation", len(outbox), err)
	}
	if outbox[0].To[0] != "queued@example.com" {
		t.Fatalf("queued invitation recipient = %v", outbox[0].To)
	}
}

func TestReissueRotatesOperatorLink(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, root.ID)
	oldToken := createInvite(t, h, cookie, csrf, "/ui/account/operators/invites", "/account", url.Values{"email": {"op@example.com"}, "inboxes": {mailerBox.ID}})
	invites, err := svc.Store.ListInvites(ctx, root.AccountID)
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %d, %v", len(invites), err)
	}
	rr := domainPost(t, h, cookie, "/ui/account/operators/invites/"+invites[0].ID+"/reissue", url.Values{"_csrf": {csrf}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("reissue = %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	flash := loc[strings.Index(loc, "_flash=")+len("_flash="):]
	page := uiGet(t, h, cookie, "/account?_flash="+flash)
	newToken := inviteToken(t, page.Body.String())
	if newToken == oldToken {
		t.Fatal("reissue must rotate the setup token")
	}
	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, httptest.NewRequest("GET", "/invite/"+oldToken, nil))
	if strings.Contains(stale.Body.String(), "Set your password") {
		t.Fatal("old invite link still redeemable after reissue")
	}
}

func TestSystemAdminSendsNewAccountInviteFromOwnMailer(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	if err := svc.Store.SetAccountMailerInbox(ctx, root.AccountID, mailerBox.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, root.ID)
	_ = createInvite(t, h, cookie, csrf, "/ui/admin/invites", "/admin", url.Values{"email": {"fresh@example.com"}})
	invites, err := svc.Store.ListInvites(ctx, "")
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %d, %v", len(invites), err)
	}
	if rr := domainPost(t, h, cookie, "/ui/admin/invites/"+invites[0].ID+"/send", url.Values{"_csrf": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("send new-account invite = %d body=%s", rr.Code, rr.Body.String())
	}
	admin := model.Principal{AccountID: root.AccountID, Admin: true}
	outbox, err := svc.Store.ListOutbox(ctx, admin, mailerBox.ID, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox = %d, %v; want 1 queued invitation", len(outbox), err)
	}
}

func TestSystemAdminEditsAccountStorageQuota(t *testing.T) {
	svc, h, root, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, root.ID)

	// The accounts table shows the quota and an edit control.
	rr := uiGet(t, h, cookie, "/admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{">Quota</th>", "edit-quota", `data-id="` + root.AccountID + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/admin missing %q", want)
		}
	}

	// A value with a unit is converted to bytes.
	rr = domainPost(t, h, cookie, "/ui/admin/accounts/"+root.AccountID+"/quota", url.Values{"_csrf": {csrf}, "quota_value": {"2"}, "quota_unit": {"gb"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("set quota = %d body=%s", rr.Code, rr.Body.String())
	}
	acc, err := svc.Store.GetAccount(ctx, root.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if acc.StorageQuotaBytes != 2<<30 {
		t.Fatalf("quota = %d, want %d", acc.StorageQuotaBytes, int64(2)<<30)
	}

	// Zero removes the limit.
	if rr = domainPost(t, h, cookie, "/ui/admin/accounts/"+root.AccountID+"/quota", url.Values{"_csrf": {csrf}, "quota_value": {"0"}, "quota_unit": {"mb"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("clear quota = %d body=%s", rr.Code, rr.Body.String())
	}
	if acc, _ = svc.Store.GetAccount(ctx, root.AccountID); acc.StorageQuotaBytes != 0 {
		t.Fatalf("quota after clear = %d, want 0", acc.StorageQuotaBytes)
	}

	// Invalid value, unknown unit and unknown account are rejected.
	if rr = domainPost(t, h, cookie, "/ui/admin/accounts/"+root.AccountID+"/quota", url.Values{"_csrf": {csrf}, "quota_value": {"-5"}, "quota_unit": {"mb"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("negative quota status = %d, want 400", rr.Code)
	}
	if rr = domainPost(t, h, cookie, "/ui/admin/accounts/"+root.AccountID+"/quota", url.Values{"_csrf": {csrf}, "quota_value": {"10"}, "quota_unit": {"parsecs"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown unit status = %d, want 400", rr.Code)
	}
	if rr = domainPost(t, h, cookie, "/ui/admin/accounts/acct_missing/quota", url.Values{"_csrf": {csrf}, "quota_value": {"10"}, "quota_unit": {"mb"}}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", rr.Code)
	}

	// An account Admin who is not a system administrator is refused.
	plain, err := svc.Store.CreateAccountAndAdmin(ctx, "Plain", "plain@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	plainCookie, plainCSRF := uiSession(t, svc, plain.ID)
	if rr = domainPost(t, h, plainCookie, "/ui/admin/accounts/"+plain.AccountID+"/quota", url.Values{"_csrf": {plainCSRF}, "quota_value": {"1"}, "quota_unit": {"gb"}}); rr.Code != http.StatusForbidden {
		t.Fatalf("non-system-admin set quota = %d, want 403", rr.Code)
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
