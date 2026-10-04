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

// TestAccountSettingsRequireAdmin verifies that account name and account time
// zone are editable only by an account Admin. A non-admin operator's POSTs must
// not change the stored values, and must not render the admin-only controls.
func TestAccountSettingsRequireAdmin(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()

	// An operator: Owner of one mailbox, not an account Admin.
	cookie, csrf := uiSession(t, svc, root.ID)
	token := createInvite(t, h, cookie, csrf, "/ui/account/operators/invites", "/account", url.Values{"email": {"op@example.com"}, "inboxes": {mailerBox.ID}})
	op := acceptInvite(t, h, token, "correct horse battery staple")
	opUser, err := svc.Store.GetUserByEmail(ctx, "op@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if opUser.IsAdmin {
		t.Fatal("operator unexpectedly an account Admin")
	}

	before, err := svc.Store.GetAccount(ctx, root.AccountID)
	if err != nil {
		t.Fatal(err)
	}

	// The operator's /account page must not render account-admin controls.
	rr := uiGet(t, h, op, "/account")
	if rr.Code != http.StatusOK {
		t.Fatalf("operator /account = %d", rr.Code)
	}
	for _, banned := range []string{"Change account name", "Account time zone", "Account administration", "Mailbox operators"} {
		if strings.Contains(rr.Body.String(), banned) {
			t.Fatalf("operator /account must not contain admin control %q", banned)
		}
	}
	// The operator keeps their personal controls.
	if !strings.Contains(rr.Body.String(), "Your time zone") {
		t.Fatal("operator /account missing personal settings")
	}

	opCookie, opCSRF := uiSession(t, svc, opUser.ID)
	rename := url.Values{"_csrf": {opCSRF}, "name": {"Hacked"}}
	if rr := domainPost(t, h, opCookie, "/ui/account/account", rename); rr.Code != http.StatusSeeOther {
		t.Fatalf("operator rename status = %d", rr.Code)
	}
	if after, _ := svc.Store.GetAccount(ctx, root.AccountID); after.Name != before.Name {
		t.Fatalf("operator changed account name: %q -> %q", before.Name, after.Name)
	}
	tz := url.Values{"_csrf": {opCSRF}, "timezone": {"Asia/Tokyo"}}
	if rr := domainPost(t, h, opCookie, "/ui/account/timezone", tz); rr.Code != http.StatusSeeOther {
		t.Fatalf("operator tz status = %d", rr.Code)
	}
	if got, _ := svc.Store.GetAccountTimezone(ctx, model.Principal{AccountID: root.AccountID, Admin: true}); got == "Asia/Tokyo" {
		t.Fatal("operator changed account time zone")
	}

	// The account Admin succeeds.
	if rr := domainPost(t, h, cookie, "/ui/account/account", url.Values{"_csrf": {csrf}, "name": {"Renamed Co"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("admin rename status = %d", rr.Code)
	}
	if after, _ := svc.Store.GetAccount(ctx, root.AccountID); after.Name != "Renamed Co" {
		t.Fatalf("admin rename not applied: %q", after.Name)
	}
}

// TestInboxTrashRetentionUISetsOverride covers the inbox edit Quota tab: the
// retention override posts through /ui/inboxes/{id}/edit, persists, renders its
// value back into the settings button data attribute, renders the dialog
// control, and clears when the override checkbox is absent.
func TestInboxTrashRetentionUISetsOverride(t *testing.T) {
	svc, h, root, mailerBox := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, root.ID)

	d, err := svc.Store.GetDomain(ctx, root.AccountID, mailerBox.DomainID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := svc.Store.CreateInbox(ctx, root.AccountID, d.ID, "team", "Team")
	if err != nil {
		t.Fatal(err)
	}

	// The dialog control is present on the dashboard.
	if rr := uiGet(t, h, cookie, "/"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "inbox-trash-retention-override") {
		t.Fatalf("dashboard missing override control: %d", rr.Code)
	}

	// Set an override of 21 days.
	form := url.Values{
		"_csrf":                    {csrf},
		"display":                  {"Team"},
		"trash_retention_override": {"1"},
		"trash_retention_days":     {"21"},
	}
	if rr := domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("set override status = %d body=%s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(ctx, root.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrashRetentionDays == nil || *got.TrashRetentionDays != 21 {
		t.Fatalf("override = %v, want 21", got.TrashRetentionDays)
	}

	// The dashboard settings button carries the value for the dialog.
	rr := uiGet(t, h, cookie, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `data-trash-retention="21"`) {
		t.Fatalf("dashboard missing rendered override")
	}

	// Without the override checkbox the value clears back to inherit.
	form = url.Values{"_csrf": {csrf}, "display": {"Team"}}
	if rr := domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("clear override status = %d", rr.Code)
	}
	got, _ = svc.Store.GetInboxInternal(ctx, root.AccountID, box.ID)
	if got.TrashRetentionDays != nil {
		t.Fatalf("override after clear = %v, want nil", *got.TrashRetentionDays)
	}

	// A negative day count is rejected without setting the override.
	form = url.Values{"_csrf": {csrf}, "display": {"Team"}, "trash_retention_override": {"1"}, "trash_retention_days": {"-3"}}
	if rr := domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit", form); rr.Code != http.StatusBadRequest {
		t.Fatalf("negative override status = %d", rr.Code)
	}
}

// TestAPIInboxTrashRetentionOverride covers the trash_retention_days field on
// the inbox PATCH route: an integer sets the override, null clears it to
// inherit, and an absent field leaves it unchanged.
func TestAPIInboxTrashRetentionOverride(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Set an override; it is reflected in the response.
	if rr := do(`{"trash_retention_days":45}`); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"trash_retention_days":45`) {
		t.Fatalf("set override %d %s", rr.Code, rr.Body.String())
	}
	got, _ := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.TrashRetentionDays == nil || *got.TrashRetentionDays != 45 {
		t.Fatalf("override = %v, want 45", got.TrashRetentionDays)
	}

	// An unrelated field leaves the override unchanged.
	if rr := do(`{"display_name":"Renamed"}`); rr.Code != 200 {
		t.Fatalf("unrelated patch %d %s", rr.Code, rr.Body.String())
	}
	if got, _ = svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID); got.TrashRetentionDays == nil || *got.TrashRetentionDays != 45 {
		t.Fatalf("override changed by unrelated patch: %v", got.TrashRetentionDays)
	}

	// null clears it to inherit.
	if rr := do(`{"trash_retention_days":null}`); rr.Code != 200 {
		t.Fatalf("clear override %d %s", rr.Code, rr.Body.String())
	}
	if got, _ = svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID); got.TrashRetentionDays != nil {
		t.Fatalf("override after null = %v, want nil", *got.TrashRetentionDays)
	}

	// A negative value is a 400.
	if rr := do(`{"trash_retention_days":-1}`); rr.Code != 400 {
		t.Fatalf("negative override %d %s", rr.Code, rr.Body.String())
	}
}
