package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func wizardLocation(t *testing.T, location, step string) string {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || u.Path != "/" || u.Query().Get("domain_wizard") != step {
		t.Fatalf("wizard redirect = %q, want step %s (parse error %v)", location, step, err)
	}
	id := u.Query().Get("domain")
	if id == "" {
		t.Fatal("wizard redirect has no domain")
	}
	return id
}

func TestUIDomainWizardCreatesBeforeSetupWithoutImplicitInheritance(t *testing.T) {
	t.Parallel()
	svc, h, u, parent, _ := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, parent.ID, "resend", map[string]any{"api_key": "parent-key"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, parent.ID, "cloudflare", nil, false); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains", url.Values{
		"_csrf": {csrf}, "name": {"child." + parent.Name}, "domain_wizard": {"1"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.String())
	}
	id := wizardLocation(t, rr.Header().Get("Location"), "receiving")
	d, err := svc.Store.GetDomain(ctx, u.AccountID, id)
	if err != nil || d.ParentDomainID != parent.ID || d.InheritSending || d.InheritReceiving || d.SendingProvider != "" || d.ReceivingProvider != "" {
		t.Fatalf("created domain = %+v, err %v", d, err)
	}
	rr = domainGet(t, h, cookie, rr.Header().Get("Location"))
	if rr.Code != http.StatusOK {
		t.Fatalf("receiving step = %d %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{`id="domain-wizard-state" data-step="receiving"`, `data-inherit="true"`, `data-open="1"`, `class="provider-select"`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("receiving missing %q", want)
		}
	}
	// Inheritance is now chosen independently on each provider step.
	rr = domainPost(t, h, cookie, "/ui/domains/"+id+"/receiving", url.Values{"_csrf": {csrf}, "provider": {"inherited"}, "domain_wizard": {"1"}})
	wizardLocation(t, rr.Header().Get("Location"), "sending")
	rr = domainPost(t, h, cookie, "/ui/domains/"+id+"/sending", url.Values{"_csrf": {csrf}, "provider": {"inherited"}, "domain_wizard": {"1"}})
	wizardLocation(t, rr.Header().Get("Location"), "summary")
	rr = domainGet(t, h, cookie, rr.Header().Get("Location"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "inherited from "+parent.Name) {
		t.Fatalf("inherited summary = %d %s", rr.Code, rr.Body.String())
	}
}

func TestUIDomainWizardSavesAndSkipPreservesConfiguration(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{
		"_csrf": {csrf}, "domain_wizard": {"1"}, "provider": {"resend"},
		"cfg_resend_api_key": {"receiving-secret"}, "cfg_resend_webhook_secret": {"whsec_dGVzdA=="},
	})
	wizardLocation(t, rr.Header().Get("Location"), "sending")
	rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "domain_wizard": {"1"}, "provider": {"resend"}, "cfg_resend_api_key": {"sending-secret"},
	})
	wizardLocation(t, rr.Header().Get("Location"), "summary")
	// Back, Skip and Close are navigation, not destructive save operations.
	for _, step := range []string{"receiving", "sending", "summary"} {
		rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&domain_wizard="+step)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", step, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "receiving-secret") || strings.Contains(rr.Body.String(), "sending-secret") {
			t.Fatal("wizard leaked saved secrets")
		}
	}
	for _, want := range []string{"Domain setup summary", "Resend — configuration saved", "does not by itself confirm delivery"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("summary missing %q", want)
		}
	}
	sending, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	values, err := svc.DecryptDomainSendingConfig(sending)
	if err != nil || values["api_key"] != "sending-secret" {
		t.Fatalf("saved sending changed: %v %+v", err, values)
	}
	receiving, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if err != nil || receiving.Provider != "resend" {
		t.Fatalf("saved receiving changed: %v %+v", err, receiving)
	}
}

func TestUIDomainWizardValidationKeepsCurrentStep(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains", url.Values{"_csrf": {csrf}, "name": {"not a domain"}, "domain_wizard": {"1"}})
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil || loc.Query().Get("domain_wizard") != "domain" || loc.Query().Get("domain_name") != "not a domain" {
		t.Fatalf("create error redirect = %s, err %v", rr.Header().Get("Location"), err)
	}
	rr = domainGet(t, h, cookie, loc.String())
	dialog := dialogHTML(t, rr.Body.String(), "add-domain-dialog")
	if !strings.Contains(dialog, `role="alert"`) || !strings.Contains(dialog, `value="not a domain"`) {
		t.Fatal("creation error lost input or inline error")
	}
	for _, kind := range []string{"receiving", "sending"} {
		rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/"+kind, url.Values{
			"_csrf": {csrf}, "provider": {"resend"}, "domain_wizard": {"1"}, "cfg_resend_api_base": {"https://api.resend.com"},
		})
		wizardLocation(t, rr.Header().Get("Location"), kind)
		rr = domainGet(t, h, cookie, rr.Header().Get("Location"))
		dialog = dialogHTML(t, rr.Body.String(), "domain-"+kind+"-dialog-"+d.ID)
		if rr.Code != http.StatusOK || !strings.Contains(dialog, `class="error"`) || !strings.Contains(dialog, `value="https://api.resend.com"`) {
			t.Fatalf("%s error missing or values lost: %d %s", kind, rr.Code, dialog)
		}
	}
}

func TestUIDomainWizardCanContinueWithPendingDNS(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{
		"_csrf": {csrf}, "domain_wizard": {"1"}, "provider": {"dialmx"},
		"cfg_dialmx_service": {"custom"}, "cfg_dialmx_receiver_urls": {"https://receiver.example"}, "cfg_dialmx_enforcement": {"moderate"},
	})
	wizardLocation(t, rr.Header().Get("Location"), "sending")
	// No runtime has reported a ready receiver and no DNS records were added.
	rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&domain_wizard=summary")
	if rr.Code != http.StatusOK {
		t.Fatalf("pending summary = %d %s", rr.Code, rr.Body.String())
	}
	summary := dialogHTML(t, rr.Body.String(), "domain-wizard-dialog")
	if !strings.Contains(summary, "Receiving checks:") || !strings.Contains(summary, "pending DNS does not prevent finishing") {
		t.Fatal("summary must report receiving checks without blocking completion")
	}
}

func TestUIDomainWizardReceiverSettingsValidationReturnsToReceiving(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/account/mx", url.Values{
		"_csrf": {csrf}, "domain_wizard_domain": {d.ID}, "rx_url": {"not a URL"}, "rx_bearer_key": {"do-not-echo"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid receiver = %d %s", rr.Code, rr.Body.String())
	}
	wizardLocation(t, rr.Header().Get("Location"), "receiving")
	if strings.Contains(rr.Header().Get("Location"), "do-not-echo") {
		t.Fatal("receiver return URL leaked a credential")
	}
	rr = domainGet(t, h, cookie, rr.Header().Get("Location"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `<option value="remotemx" selected`) {
		t.Fatalf("receiver error lost wizard/provider: %d %s", rr.Code, rr.Body.String())
	}
}

func TestUIDomainWizardGeneratedReceiverSetupRetainsNavigation(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"cloudflare", "postmark"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			svc, h, u, d, _ := httpFixture(t)
			cookie, csrf := uiSession(t, svc, u.ID)
			rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{
				"_csrf": {csrf}, "provider": {provider}, "domain_wizard": {"1"},
			})
			location := rr.Header().Get("Location")
			if rr.Code != http.StatusSeeOther || !strings.Contains(location, "domain_wizard=receiving") || !strings.Contains(location, "_flash=") {
				t.Fatalf("generated setup redirect = %d %s", rr.Code, location)
			}
			rr = domainGet(t, h, cookie, location)
			if rr.Code != http.StatusOK {
				t.Fatalf("setup = %d %s", rr.Code, rr.Body.String())
			}
			marker := `id="cf-code"`
			if provider == "postmark" {
				marker = `id="domain-credential-dialog"`
			}
			if !strings.Contains(rr.Body.String(), marker) {
				t.Fatalf("generated setup missing %s", marker)
			}
			rr = domainGet(t, h, cookie, location)
			if strings.Contains(rr.Body.String(), marker) {
				t.Fatal("generated credential was displayed more than once")
			}
		})
	}
}

func TestUIDomainWizardSummaryAllowsUnconfiguredAndRejectsForeignID(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	// The shared fixture has Mailgun receiving configured; skipping both steps
	// needs a genuinely unconfigured domain instead.
	d, err := svc.Store.CreateDomain(context.Background(), u.AccountID, "skipped.test")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	rr := domainGet(t, h, cookie, "/?domain="+d.ID+"&domain_wizard=summary")
	if rr.Code != http.StatusOK || strings.Count(dialogHTML(t, rr.Body.String(), "domain-wizard-dialog"), "Not configured — finish setup later") != 2 {
		t.Fatalf("unconfigured summary = %d %s", rr.Code, rr.Body.String())
	}
	for _, query := range []string{"domain=foreign-id&domain_wizard=summary", "domain=" + d.ID + "&domain_wizard=invalid", "domain_wizard=sending"} {
		rr = domainGet(t, h, cookie, "/?"+query)
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `id="domain-wizard-state"`) {
			t.Fatalf("invalid wizard state accepted: %s (%d)", query, rr.Code)
		}
	}
}
