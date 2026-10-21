package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
)

// TestInstallMXUIRendersReceiverChoice pins that the Admin page offers the two
// ways to add the installation receiver (the bundled one and one the operator
// runs), and that the per-domain Direct MX panel stays Included-only.
func TestInstallMXUIRendersReceiverChoice(t *testing.T) {
	t.Parallel()
	svc, h, u, box := systemAdminFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	body := uiGet(t, h, cookie, "/admin").Body.String()
	for _, want := range []string{"MX receiver", `name="mode" value="included"`, `name="mode" value="remote"`, `name="url"`, `name="bearer_key"`, "/ui/admin/mx"} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin page missing %q:\n%s", want, body)
		}
	}
	// The per-domain editor keeps its Included-only mode.
	dialog := dialogHTML(t, uiGet(t, h, cookie, "/?domain="+box.DomainID+"&kind=receiving&provider=mx").Body.String(), "domain-receiving-dialog-"+box.DomainID)
	if strings.Contains(dialog, `name="mode" value="remote"`) || strings.Contains(dialog, `name="bearer_key"`) {
		t.Fatal("the per-domain Direct MX panel must stay Included-only")
	}
}

// TestInstallMXUISavesRemoteReceiver exercises the installation receiver form
// in Remote mode: the write needs a CSRF token, the receiver is stored, and
// the bearer key is never echoed back to the page.
func TestInstallMXUISavesRemoteReceiver(t *testing.T) {
	t.Parallel()
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"mode": {"remote"}, "url": {"https://r.example"}, "bearer_key": {"remote-secret"}}

	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusForbidden {
		t.Fatalf("save without CSRF = %d", rr.Code)
	}
	form.Set("_csrf", csrf)
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("remote save = %d %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != app.MXModeRemote || settings.URL != "https://r.example" || !settings.KeyConfigured {
		t.Fatalf("saved settings = %+v %v", settings, err)
	}
	if strings.Contains(uiGet(t, h, cookie, "/admin").Body.String(), "remote-secret") {
		t.Fatal("the admin page echoed the bearer key")
	}
}

// TestInstallMXUISavesIncludedReceiver saves the bundled receiver from the
// Admin page and clears it again, so the installation can be taken back to an
// unconfigured state.
func TestInstallMXUISavesIncludedReceiver(t *testing.T) {
	t.Parallel()
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"_csrf": {csrf}, "mode": {"included"}, "hostname": {"mx.example"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("included save = %d %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != app.MXModeIncluded || settings.Hostname != "mx.example" || !settings.KeyConfigured {
		t.Fatalf("saved settings = %+v %v", settings, err)
	}
	clear := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx/clear", clear.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("clear = %d %s", rr.Code, rr.Body.String())
	}
	cleared, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || cleared.Mode != "" {
		t.Fatalf("cleared settings = %+v %v", cleared, err)
	}
}

// TestInstallMXUIReportsValidationFailure pins that a refused save changes
// nothing, and that the Admin page shows the reason with the operator's
// chosen mode kept.
func TestInstallMXUIReportsValidationFailure(t *testing.T) {
	t.Parallel()
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"_csrf": {csrf}, "mode": {"remote"}, "bearer_key": {"k"}}
	rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode())
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid save = %d %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != "" {
		t.Fatalf("a refused save must not configure a receiver: %+v %v", settings, err)
	}
	body := uiGet(t, h, cookie, rr.Header().Get("Location")).Body.String()
	if !strings.Contains(body, "requires a URL") {
		t.Fatalf("the admin page must show the validation reason:\n%s", body)
	}
	if !strings.Contains(body, `name="mode" value="remote" checked`) {
		t.Fatalf("the chosen mode must survive the failed save:\n%s", body)
	}
}

// TestInstallMXUIRejectsAccountAdmin pins that an account Admin cannot reach
// the installation receiver: one receiver serves every account, so only a
// system administrator may change it.
func TestInstallMXUIRejectsAccountAdmin(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"_csrf": {csrf}, "mode": {"remote"}, "url": {"https://r.example"}, "bearer_key": {"k"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusForbidden {
		t.Fatalf("account admin save = %d", rr.Code)
	}
	if rr := uiGet(t, h, cookie, "/admin"); rr.Code != http.StatusForbidden {
		t.Fatalf("account admin admin page = %d", rr.Code)
	}
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != "" {
		t.Fatalf("an account admin must not configure the receiver: %+v %v", settings, err)
	}
}
