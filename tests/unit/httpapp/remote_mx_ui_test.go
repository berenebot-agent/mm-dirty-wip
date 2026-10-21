package httpapp_test

import (
	"strings"
	"testing"
)

func TestRemoteMXUIExplainsOutboundConnectionAndShowsReceiverSettings(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=remotemx").Body.String(), "domain-receiving-dialog-"+domain.ID)
	for _, want := range []string{
		"Set up a receiver you run separately.",
		"MailMoose connects outbound to it over HTTP/2.",
		"Remote SMTP receiving guide",
		"Point the MX records for <code>example.com</code> at that hostname",
		"configured on the receiver itself, not here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Remote MX setup instructions missing %q:\n%s", want, body)
		}
	}
	for _, want := range []string{
		`class="remote-mx-editor"`,
		`name="rx_url"`,
		`name="rx_bearer_key"`,
		"DIALMX_CORE_KEY",
		"Private CA certificate bundle (PEM, optional)",
		// The only receiving setting that applies per domain to a remote
		// receiver is the core-side spam policy.
		"Receiver authentication enforcement",
		// Saving from this panel returns to the dialog that opened it.
		`name="rx_domain"`,
		// The receiver owns its SMTP-facing configuration, and the copy must say
		// so rather than duplicating it here.
		"configured on the receiver itself, not here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Remote MX dialog missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"This account's own receiver at",
		"internal receiver",
		"No webhook, DNS proof",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("Remote MX dialog contains unrelated copy %q", unwanted)
		}
	}
	for _, unwanted := range []string{
		"This account's own receiver at",
		"Register this webhook URL with",
		"Register the webhook URL above",
		"Remote receiver status: disabled",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("Remote MX dialog unexpectedly contains %q", unwanted)
		}
	}
}

// TestRemoteMXViewHidesDirectMXReceiverSettings pins the separation between the
// two receivers. The Direct MX tuning (greeting hostname, limits, verification,
// timeouts, STARTTLS) configures the bundled receiver every Direct MX domain
// shares, so it must be gated on the Direct MX provider choice rather than
// offered as if it configured the account's remote receiver.
func TestRemoteMXViewHidesDirectMXReceiverSettings(t *testing.T) {
	t.Parallel()
	svc, h, u, box := systemAdminFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	remote := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+box.DomainID+"&kind=receiving&provider=remotemx").Body.String(), "domain-receiving-dialog-"+box.DomainID)
	hostname := strings.Index(remote, "SMTP greeting hostname")
	if hostname < 0 {
		t.Fatal("Direct MX receiver editor missing from the receiving dialog")
	}
	gate := strings.LastIndex(remote[:hostname], "data-mx-panel")
	if gate < 0 {
		t.Fatal("Direct MX receiver editor is not wrapped in an mx-panel")
	}
	if !strings.HasPrefix(remote[gate:hostname], "data-mx-panel hidden") {
		t.Fatal("Direct MX receiver editor is not hidden while Remote MX is selected")
	}

	direct := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+box.DomainID+"&kind=receiving&provider=mx").Body.String(), "domain-receiving-dialog-"+box.DomainID)
	hostname = strings.Index(direct, "SMTP greeting hostname")
	gate = strings.LastIndex(direct[:hostname], "data-mx-panel")
	if gate < 0 || strings.HasPrefix(direct[gate:hostname], "data-mx-panel hidden") {
		t.Fatal("Direct MX receiver editor must be visible while Direct MX is selected")
	}
}
