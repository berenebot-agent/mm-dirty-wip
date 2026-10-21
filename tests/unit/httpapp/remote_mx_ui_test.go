package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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
		// The receiver owns its SMTP-facing configuration, and the copy must say
		// so rather than duplicating it here.
		"configured on the receiver itself, not here",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Remote MX dialog missing %q:\n%s", want, body)
		}
	}
	start := strings.Index(body, `<form id="domain-receiving-form-`)
	end := strings.Index(body[start:], "</form>") + start
	if !strings.Contains(body[start:end], `name="rx_url"`) || !strings.Contains(body[start:end], `name="rx_bearer_key"`) {
		t.Fatal("receiver fields must belong to the domain receiving form")
	}
	for _, extra := range []string{"Save remote receiver", "Remove remote receiver", `action="/ui/account/mx`} {
		if strings.Contains(body, extra) {
			t.Fatalf("receiving dialog still contains an extra receiver action: %s", extra)
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

func TestRemoteMXSingleSaveAndRemove(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/domains/" + domain.ID + "/receiving"
	form := url.Values{"_csrf": {csrf}, "provider": {"remotemx"}, "rx_revision": {"0"}, "rx_url": {"https://mx.example.test"}, "rx_bearer_key": {"receiver-secret"}, "cfg_remotemx_enforcement": {"hard"}}
	if rr := uiPost(t, h, cookie, path, form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	receiver, err := svc.GetAccountMXReceiver(ctx, u.AccountID)
	if err != nil || receiver.URL != "https://mx.example.test" || !receiver.KeyConfigured {
		t.Fatalf("receiver not saved: %+v %v", receiver, err)
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, domain.ID)
	if err != nil || cfg.Provider != "remotemx" {
		t.Fatalf("domain not saved: %+v %v", cfg, err)
	}
	values, err := svc.DecryptDomainReceivingConfig(cfg)
	if err != nil || values["enforcement"] != "hard" {
		t.Fatalf("spam policy not saved: %v %v", values, err)
	}
	form.Set("rx_revision", strconv.FormatInt(receiver.Revision, 10))
	form.Set("rx_bearer_key", "")
	if rr := uiPost(t, h, cookie, path, form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("resave: %d", rr.Code)
	}
	after, _ := svc.GetAccountMXReceiver(ctx, u.AccountID)
	if after.Revision != receiver.Revision || !after.KeyConfigured {
		t.Fatal("unchanged receiver was rewritten or lost its key")
	}
	if rr := uiPost(t, h, cookie, path+"/clear", url.Values{"_csrf": {csrf}}.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("remove receiving: %d", rr.Code)
	}
	after, _ = svc.GetAccountMXReceiver(ctx, u.AccountID)
	if after.URL != receiver.URL || !after.KeyConfigured {
		t.Fatal("removing domain receiving must not delete the shared receiver")
	}
}

// A shared Remote MX receiver must not be replaced or lose its bearer key when
// an administrator saves receiving for a second domain in the same account.
func TestRemoteMXSharedReceiverAcrossDomains(t *testing.T) {
	t.Parallel()
	svc, h, u, first, _ := httpFixture(t)
	ctx := context.Background()
	second, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{
		"_csrf": {csrf}, "provider": {"remotemx"}, "rx_revision": {"0"},
		"rx_url": {"https://mx.example.test"}, "rx_bearer_key": {"shared-secret"},
		"cfg_remotemx_enforcement": {"hard"},
	}
	if rr := uiPost(t, h, cookie, "/ui/domains/"+first.ID+"/receiving", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("first domain save: %d %s", rr.Code, rr.Body.String())
	}
	receiver, err := svc.GetAccountMXReceiver(ctx, u.AccountID)
	if err != nil || receiver.URL != "https://mx.example.test" || !receiver.KeyConfigured {
		t.Fatalf("initial receiver: %+v, %v", receiver, err)
	}
	form.Set("rx_revision", strconv.FormatInt(receiver.Revision, 10))
	form.Set("rx_bearer_key", "")
	form.Set("cfg_remotemx_enforcement", "moderate")
	if rr := uiPost(t, h, cookie, "/ui/domains/"+second.ID+"/receiving", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("second domain save: %d %s", rr.Code, rr.Body.String())
	}
	after, err := svc.GetAccountMXReceiver(ctx, u.AccountID)
	if err != nil || after.Revision != receiver.Revision || !after.KeyConfigured || after.URL != receiver.URL {
		t.Fatalf("shared receiver unexpectedly rewritten: %+v, %v", after, err)
	}
	for _, tc := range []struct{ id, enforcement string }{{first.ID, "hard"}, {second.ID, "moderate"}} {
		cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, tc.id)
		if err != nil || cfg.Provider != "remotemx" {
			t.Fatalf("domain %s lost receiving: %+v, %v", tc.id, cfg, err)
		}
		values, err := svc.DecryptDomainReceivingConfig(cfg)
		if err != nil || values["enforcement"] != tc.enforcement {
			t.Fatalf("domain %s enforcement: %+v, %v", tc.id, values, err)
		}
	}
}

func TestRemoteMXSingleSaveValidation(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/domains/" + domain.ID + "/receiving"
	for _, bad := range []string{"url", "key", "policy"} {
		t.Run(bad, func(t *testing.T) {
			form := url.Values{"_csrf": {csrf}, "provider": {"remotemx"}, "rx_revision": {"0"}, "rx_url": {"https://mx.example.test"}, "rx_bearer_key": {"never-echo-this-secret"}, "cfg_remotemx_enforcement": {"hard"}}
			switch bad {
			case "url":
				form.Set("rx_url", "not-a-url")
			case "key":
				form.Set("rx_bearer_key", "")
			case "policy":
				form.Set("cfg_remotemx_enforcement", "bogus")
			}
			rr := uiPost(t, h, cookie, path, form.Encode())
			if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "domain="+domain.ID) {
				t.Fatalf("validation must reopen receiving: %d %s", rr.Code, rr.Header().Get("Location"))
			}
			body := uiGet(t, h, cookie, rr.Header().Get("Location")).Body.String()
			if !strings.Contains(body, `class="error"`) || strings.Contains(body, "never-echo-this-secret") {
				t.Fatal("validation must display an error without echoing the key")
			}
			d, _ := svc.Store.GetDomain(context.Background(), u.AccountID, domain.ID)
			if d.ReceivingProvider != "mailgun" {
				t.Fatal("failed save changed the existing provider")
			}
			receiver, _ := svc.GetAccountMXReceiver(context.Background(), u.AccountID)
			if receiver.URL != "" {
				t.Fatal("failed validation saved a receiver")
			}
		})
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
