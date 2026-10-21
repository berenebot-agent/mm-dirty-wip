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
		"Point this domain's MX record at your receiver's SMTP hostname.",
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
