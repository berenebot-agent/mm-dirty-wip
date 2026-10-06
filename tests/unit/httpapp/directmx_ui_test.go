package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// configureMXReceiver seeds the installation MX receiver as a system
// administrator would, so the per-domain Direct MX panel can report a
// configured receiver. The fixture user is an account Admin, so a synthetic
// system-admin principal is used for the installation-scoped write.
func configureMXReceiver(t *testing.T, svc *app.Service, mode string) {
	t.Helper()
	p := model.Principal{SystemAdmin: true}
	if _, err := svc.SaveMXReceiverSettings(context.Background(), p, app.MXReceiverInput{Mode: mode, Hostname: "mx.test"}); err != nil {
		t.Fatalf("seed MX receiver: %v", err)
	}
}

// TestDirectMXUIVisibleToAdminAndShowsReceiverStatus pins the per-domain Direct
// MX UX: an account admin sees the Direct MX option and, once the installation
// receiver is configured, a status panel naming the mode, state and MX target.
func TestDirectMXUIVisibleToAdminAndShowsReceiverStatus(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	configureMXReceiver(t, svc, app.MXModeIncluded)

	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=mx")
	body := dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID)

	if !strings.Contains(body, "Direct MX") {
		t.Fatalf("admin receiving dialog missing Direct MX option:\n%s", body)
	}
	// The status panel is present and reflects the configured receiver. Without a
	// live runtime the state is not "active", so the honest warning must show.
	if !strings.Contains(body, "Direct MX receiver") {
		t.Fatalf("admin receiving dialog missing Direct MX status panel:\n%s", body)
	}
	if strings.Contains(body, "Edit receiver settings") || strings.Contains(body, "Save receiver settings") {
		t.Fatal("account admin must see status only, not receiver controls")
	}
	if strings.Contains(body, "State:") || strings.Contains(body, "not connected") || strings.Contains(body, "not active yet") {
		t.Fatalf("built-in receiver must present configuration rather than connection status:\n%s", body)
	}
	if strings.Contains(body, "not configured") {
		t.Fatalf("configured receiver must not show the not-configured state:\n%s", body)
	}
	// The dead-end instruction to configure the receiver elsewhere is gone now
	// that the receiver exists.
	if strings.Contains(body, "Admin → MX receiver") {
		t.Fatalf("configured receiver must not tell the admin to configure it elsewhere:\n%s", body)
	}
}

func TestDirectMXEditorPermissionsAndDomainReturn(t *testing.T) {
	svc, h, u, box := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/domains/" + box.DomainID + "/mx"
	view := "/?domain=" + box.DomainID + "&kind=receiving&provider=mx"
	if body := uiGet(t, h, cookie, "/admin").Body.String(); strings.Contains(body, "Edit receiver settings") {
		t.Fatal("receiver editor still appears on Admin")
	}
	body := dialogHTML(t, uiGet(t, h, cookie, view).Body.String(), "domain-receiving-dialog-"+box.DomainID)
	if !strings.Contains(body, `class="mx-editor"`) || strings.Contains(body, "Save receiver settings") || strings.Contains(body, "not configured") {
		t.Fatal("unconfigured receiver must offer inline setup")
	}
	form := url.Values{"_csrf": {csrf}, "hostname": {"mx.example"}, "revision": {"0"}}
	rr := uiPost(t, h, cookie, path, form.Encode())
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "domain="+box.DomainID) {
		t.Fatalf("save must return to originating domain: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	body = dialogHTML(t, uiGet(t, h, cookie, rr.Header().Get("Location")).Body.String(), "domain-receiving-dialog-"+box.DomainID)
	if strings.Contains(body, "Save receiver settings") || strings.Contains(body, "private-test-secret") {
		t.Fatal("configured editor must collapse and never echo secrets")
	}
	// A stale revision must return a useful conflict to this same dialog.
	rr = uiPost(t, h, cookie, path, form.Encode())
	if rr.Code != http.StatusSeeOther || !strings.Contains(uiGet(t, h, cookie, rr.Header().Get("Location")).Body.String(), "configuration changed") {
		t.Fatal("stale save did not surface its conflict")
	}
	if rr = uiPost(t, h, cookie, "/ui/domains/missing/mx", form.Encode()); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign/missing domain = %d", rr.Code)
	}
	second, err := svc.Store.CreateDomain(context.Background(), u.AccountID, "second.example")
	if err != nil {
		t.Fatal(err)
	}
	form.Set("revision", "1")
	form.Set("hostname", "")
	form.Set("require_tls", "true")
	rr = uiPost(t, h, cookie, path, form.Encode())
	loc := rr.Header().Get("Location")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid remote save = %d", rr.Code)
	}
	wrongDomain := strings.Replace(loc, "domain="+box.DomainID, "domain="+second.ID, 1)
	if strings.Contains(uiGet(t, h, cookie, wrongDomain).Body.String(), "requires a STARTTLS") {
		t.Fatal("validation flash leaked to another domain")
	}
	body = dialogHTML(t, uiGet(t, h, cookie, loc).Body.String(), "domain-receiving-dialog-"+box.DomainID)
	if !strings.Contains(body, `class="mx-editor"`) || strings.Contains(body, `value="mx.example"`) || strings.Contains(body, "private-test-secret") {
		t.Fatal("validation must reopen editor, retain blank URL, and hide secret")
	}
	// Every domain's forms are siblings of the receiving form, never nested.
	depth := 0
	for _, fragment := range strings.Split(body, "<") {
		if strings.HasPrefix(fragment, "form ") {
			depth++
			if depth > 1 {
				t.Fatal("nested forms in Direct MX dialog")
			}
		} else if strings.HasPrefix(fragment, "/form>") {
			depth--
		}
	}
	if depth != 0 {
		t.Fatal("unbalanced Direct MX forms")
	}
	otherSvc, otherH, otherUser, domain, _ := httpFixture(t)
	otherCookie, otherCSRF := uiSession(t, otherSvc, otherUser.ID)
	for _, suffix := range []string{"", "/clear"} {
		form.Set("_csrf", otherCSRF)
		if rr = uiPost(t, otherH, otherCookie, "/ui/domains/"+domain.ID+"/mx"+suffix, form.Encode()); rr.Code != http.StatusForbidden {
			t.Fatalf("account admin write %s = %d", suffix, rr.Code)
		}
	}
}

func TestDirectMXIncludedDefaultsAndEditableGreeting(t *testing.T) {
	svc, h, u, box := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	view := "/?domain=" + box.DomainID + "&kind=receiving&provider=mx"
	path := "/ui/domains/" + box.DomainID + "/receiving"
	svc.Config.BaseURL = "https://public.example:8443/app"
	for _, tc := range []struct{ external, want string }{
		{"", "public.example"},
		{"https://webhooks.example:9443/inbound", "webhooks.example"},
	} {
		svc.Config.DedicatedReceiverURL = tc.external
		body := dialogHTML(t, uiGet(t, h, cookie, view).Body.String(), "domain-receiving-dialog-"+box.DomainID)
		if !strings.Contains(body, `name="mode" value="included"`) || !strings.Contains(body, `name="hostname" value="`+tc.want+`"`) {
			t.Fatalf("missing Included/greeting default %q", tc.want)
		}
		if strings.Contains(body, `name="mode" value="remote"`) || strings.Contains(body, `name="bearer_key"`) || strings.Contains(body, `name="url"`) {
			t.Fatal("Direct MX must not offer Remote configuration")
		}
	}
	form := url.Values{"_csrf": {csrf}, "provider": {"mx"}, "hostname": {"custom.example"}, "revision": {"0"}}
	if rr := uiPost(t, h, cookie, path, form.Encode()); rr.Code != http.StatusSeeOther || strings.Contains(rr.Header().Get("Location"), "domain=") {
		t.Fatalf("successful Included save must close settings: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != app.MXModeIncluded || settings.Hostname != "custom.example" {
		t.Fatalf("editable greeting not saved: %+v %v", settings, err)
	}
	body := dialogHTML(t, uiGet(t, h, cookie, view).Body.String(), "domain-receiving-dialog-"+box.DomainID)
	if !strings.Contains(body, `name="hostname" value="custom.example"`) {
		t.Fatal("stored greeting overridden by URL default")
	}
	if domain, err := svc.Store.GetDomain(context.Background(), u.AccountID, box.DomainID); err != nil || domain.ReceivingProvider != "mx" {
		t.Fatalf("single Save did not select domain MX: %+v %v", domain, err)
	}
	form.Set("revision", "1")
	if rr := uiPost(t, h, cookie, path, form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("second Save = %d", rr.Code)
	}
}

// TestDirectMXUIUnconfiguredReceiverShowsGuidance pins that an admin choosing
// Direct MX before the receiver exists is told, in the dialog, that a system
// administrator must enable it, rather than silently accepting mail nowhere.
func TestDirectMXUIUnconfiguredReceiverShowsGuidance(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)

	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=mx")
	body := dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID)

	if strings.Contains(body, "not configured") {
		t.Fatalf("setup must not show an unconfigured flag:\n%s", body)
	}
	if !strings.Contains(body, "system administrator") {
		t.Fatalf("unconfigured receiver must point at the system administrator:\n%s", body)
	}
}

// TestDirectMXLabel proves the transport advertises the human label used by the
// provider dropdown rather than the internal transport string.
func TestDirectMXLabel(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving").Body.String(), "domain-receiving-dialog-"+domain.ID)
	if !strings.Contains(body, "Direct MX (SMTP to the MailMoose receiver)") {
		t.Fatalf("provider option missing the Direct MX label:\n%s", body)
	}
}
