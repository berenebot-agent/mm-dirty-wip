package httpapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// TestUIExternalAliasLifecycle exercises the external-alias UI: creation
// redirects to the dashboard with the alias's connector popup, the dialogs
// render, the connector saves with secret retention, the activity page shows
// the alias, the name edits, and delete removes it.
func TestUIExternalAliasLifecycle(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	base := "/ui/inboxes/" + box.ID + "/external-aliases"

	rr := domainPost(t, h, cookie, base, url.Values{
		"_csrf": {csrf}, "external_alias": {"Agent@Gmail.com"}, "external_alias_name": {"Agent"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.HasPrefix(loc, "/?") || !strings.Contains(loc, "alias=") {
		t.Fatalf("create redirect %q", loc)
	}
	groups, err := svc.Store.ListExternalAliases(ctx, u.AccountID)
	if err != nil || len(groups[box.ID]) != 1 {
		t.Fatalf("alias not created: %v %#v", err, groups)
	}
	alias := groups[box.ID][0]
	if alias.Address != "agent@gmail.com" || alias.DisplayName != "Agent" {
		t.Fatalf("created alias %+v", alias)
	}
	aliasBase := base + "/" + alias.ID
	dlgID := "external-alias-sending-dialog-" + alias.ID

	// The dashboard renders the connector popup for the alias with the provider
	// picker; the alias activity page shows identity and activity.
	rr = domainGet(t, h, cookie, "/?alias="+alias.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`id="` + dlgID + `"`, `name="provider"`, aliasBase + `/sending`, "Sending · agent@gmail.com"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	rr = domainGet(t, h, cookie, aliasBase)
	if rr.Code != http.StatusOK {
		t.Fatalf("activity page %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("activity page cache-control = %q", got)
	}
	for _, want := range []string{"agent@gmail.com", "sending only", "Activity", "Delete"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("activity page missing %q", want)
		}
	}

	// Save a connector; the secret is not echoed back.
	rr = domainPost(t, h, cookie, aliasBase+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"resend"}, "cfg_resend_api_key": {"re_secret"}, "cfg_resend_api_base": {"https://api.resend.com"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save connector %d %s", rr.Code, rr.Body.String())
	}
	// A successful save returns to the inbox edit dialog, not the open popup.
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "inbox="+box.ID) {
		t.Fatalf("save connector redirect %q, want inbox=%s", loc, box.ID)
	}
	saved, err := svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, alias.ID)
	if err != nil || !saved.Configured || saved.Provider != "resend" {
		t.Fatalf("saved alias %+v err=%v", saved, err)
	}
	if dec, derr := svc.DecryptExternalAliasSendingConfig(saved); derr != nil || dec["api_key"] != "re_secret" {
		t.Fatalf("stored secret %+v err=%v", dec, derr)
	}
	rr = domainGet(t, h, cookie, "/?alias="+alias.ID)
	if strings.Contains(rr.Body.String(), "re_secret") || strings.Contains(rr.Body.String(), `name="cfg_resend_api_key" value=`) {
		t.Fatalf("secret leaked in dialog: %s", rr.Body.String())
	}

	// Same-provider save with a blank secret retains it.
	rr = domainPost(t, h, cookie, aliasBase+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"resend"}, "cfg_resend_api_base": {"https://api2.resend.com"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("resave connector %d %s", rr.Code, rr.Body.String())
	}
	saved, _ = svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, alias.ID)
	if dec, _ := svc.DecryptExternalAliasSendingConfig(saved); dec["api_key"] != "re_secret" || dec["api_base"] != "https://api2.resend.com" {
		t.Fatalf("secret retention failed: %+v", dec)
	}

	// Clearing the connector removes it.
	rr = domainPost(t, h, cookie, aliasBase+"/sending/clear", url.Values{"_csrf": {csrf}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("clear connector %d %s", rr.Code, rr.Body.String())
	}
	cleared, _ := svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, alias.ID)
	if cleared.Configured {
		t.Fatalf("connector not cleared: %+v", cleared)
	}

	// Rename the alias.
	rr = domainPost(t, h, cookie, aliasBase+"/edit", url.Values{"_csrf": {csrf}, "external_alias_name": {"Agent Two"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("rename %d %s", rr.Code, rr.Body.String())
	}
	renamed, _ := svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, alias.ID)
	if renamed.DisplayName != "Agent Two" {
		t.Fatalf("rename failed: %+v", renamed)
	}

	// Delete removes it.
	rr = domainPost(t, h, cookie, aliasBase+"/delete", url.Values{"_csrf": {csrf}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alias survived delete: %v", err)
	}
}

// TestUIExternalAliasCSRF proves the state-changing routes reject a request
// without the session's CSRF token.
func TestUIExternalAliasCSRF(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	base := "/ui/inboxes/" + box.ID + "/external-aliases"

	rr := domainPost(t, h, cookie, base, url.Values{"external_alias": {"agent@gmail.com"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing csrf = %d", rr.Code)
	}
}

// TestUIExternalAliasHostedForbidden proves external aliases are self-hosted
// only: even an Admin is refused in hosted mode.
func TestUIExternalAliasHostedForbidden(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	base := "/ui/inboxes/" + box.ID + "/external-aliases"

	svc.Config.Mode = "hosted"
	rr := domainGet(t, h, cookie, base+"/ea_missing")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("hosted GET = %d", rr.Code)
	}
	rr = domainPost(t, h, cookie, base, url.Values{"_csrf": {csrf}, "external_alias": {"agent@gmail.com"}})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("hosted POST = %d", rr.Code)
	}
}

// TestUIExternalAliasDefaultSenderPreserved proves an inbox save through the
// edit form does not drop an external default_sender, and that the compose From
// select offers the external alias.
func TestUIExternalAliasDefaultSenderPreserved(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	a, err := svc.CreateExternalAlias(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, "agent@gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxDefaultSender(ctx, u.AccountID, box.ID, a.Address); err != nil {
		t.Fatal(err)
	}

	// Re-save the inbox with the external address still selected as default.
	rr := domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/edit", url.Values{
		"_csrf": {csrf}, "display": {"Hermes"}, "default_sender": {a.Address},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("inbox edit %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil || got.DefaultSender != a.Address {
		t.Fatalf("external default dropped: %q err=%v", got.DefaultSender, err)
	}

	// Compose offers the external alias in the From select.
	rr = domainGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/compose")
	if !strings.Contains(rr.Body.String(), a.Address) {
		t.Fatalf("compose missing external sender: %s", rr.Body.String())
	}
}

// TestUIExternalAliasConnectorValidationRetainsInput proves a connector
// validation error reopens the alias page with the error and the submitted
// non-secret values, while never echoing a secret.
func TestUIExternalAliasConnectorValidationRetainsInput(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	a, err := svc.CreateExternalAlias(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, "agent@gmail.com", "")
	if err != nil {
		t.Fatal(err)
	}
	aliasBase := "/ui/inboxes/" + box.ID + "/external-aliases/" + a.ID

	// A non-numeric SMTP port is a user-safe validation error.
	rr := domainPost(t, h, cookie, aliasBase+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"smtp"}, "cfg_smtp_host": {"smtp.example.com"}, "cfg_smtp_port": {"nope"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("validation redirect %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "provider=smtp") || !strings.Contains(loc, "_flash=") {
		t.Fatalf("validation redirect lacks provider/flash: %q", loc)
	}
	rr = domainGet(t, h, cookie, loc)
	body := rr.Body.String()
	if !strings.Contains(body, "whole number") {
		t.Fatalf("validation error not shown: %s", body)
	}
	if !strings.Contains(body, `name="cfg_smtp_host"`) || !strings.Contains(body, `value="smtp.example.com"`) {
		t.Fatalf("non-secret input not retained: %s", body)
	}
	// The failed save must not have stored a config.
	got, err := svc.Store.GetExternalAlias(ctx, u.AccountID, box.ID, a.ID)
	if err != nil || got.Configured {
		t.Fatalf("failed validation stored a connector: %+v err=%v", got, err)
	}
}

// TestUIExternalAliasReadinessDrivesBanner proves an unconfigured external
// default shows the "Sending paused" banner pointing at the alias page, and a
// configured one clears it.
func TestUIExternalAliasReadinessDrivesBanner(t *testing.T) {
	svc, h, u, d, box := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	// The inbox domain has a working sender, to prove the external alias does
	// not inherit it.
	setDomainBrevo(t, svc, u.AccountID, d.ID)
	a, err := svc.CreateExternalAlias(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, "agent@gmail.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxDefaultSender(ctx, u.AccountID, box.ID, a.Address); err != nil {
		t.Fatal(err)
	}

	rr := domainGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	body := rr.Body.String()
	if !strings.Contains(body, "Sending paused") || !strings.Contains(body, "/?alias="+a.ID) {
		t.Fatalf("paused banner missing or not linked: %s", body)
	}

	// Configure the alias connector; the banner clears.
	rr = domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/external-aliases/"+a.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"brevo"}, "cfg_brevo_api_key": {"k"}, "cfg_brevo_api_base": {"https://api.brevo.com"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save connector %d %s", rr.Code, rr.Body.String())
	}
	rr = domainGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	if strings.Contains(rr.Body.String(), "Sending paused") {
		t.Fatalf("banner not cleared after connector save: %s", rr.Body.String())
	}
}
