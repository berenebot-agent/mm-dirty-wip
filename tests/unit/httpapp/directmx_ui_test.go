package httpapp_test

import (
	"context"
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
	if !strings.Contains(body, "Included (receiver runs in this deployment)") {
		t.Fatalf("status panel missing the receiver mode:\n%s", body)
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

// TestDirectMXUIUnconfiguredReceiverShowsGuidance pins that an admin choosing
// Direct MX before the receiver exists is told, in the dialog, that a system
// administrator must enable it, rather than silently accepting mail nowhere.
func TestDirectMXUIUnconfiguredReceiverShowsGuidance(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)

	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=mx")
	body := dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID)

	if !strings.Contains(body, "not configured") {
		t.Fatalf("unconfigured receiver must show the not-configured state:\n%s", body)
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
