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
	provider := providerGroupHTML(t, body, "remotemx")

	for _, want := range []string{
		"MailMoose connects outbound to your account",
		"No webhook, DNS proof, or per-domain key is needed.",
	} {
		if !strings.Contains(provider, want) {
			t.Errorf("Remote MX provider instructions missing %q:\n%s", want, provider)
		}
	}
	for _, want := range []string{
		`<details class="remote-mx-editor" open>`,
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
		"Register this webhook URL with",
		"Register the webhook URL above",
	} {
		if strings.Contains(provider, unwanted) {
			t.Errorf("Remote MX dialog unexpectedly contains %q", unwanted)
		}
	}
}
