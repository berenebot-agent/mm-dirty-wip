package httpapp_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// TestDirectMXSendingGreetingPrefilledWithDomain proves the Direct MX sending
// editor pre-fills the HELO hostname with the domain's own name, so a freshly
// added sender can be Saved without any typing. The field is still required
// (marked with *), and an explicitly stored value is never overridden.
func TestDirectMXSendingGreetingPrefilledWithDomain(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	group := mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID)
	if !strings.Contains(group, `name="cfg_mx_helo"`) {
		t.Fatalf("the HELO field is missing from the Direct MX sending group:\n%s", group)
	}
	// The value is the domain name, not a gray placeholder, so the browser's
	// required attribute is satisfied and Save can submit untouched.
	if !strings.Contains(group, `value="`+d.Name+`"`) {
		t.Errorf("HELO field is not pre-filled with %q:\n%s", d.Name, group)
	}
	if strings.Contains(group, `placeholder="mail.example.com"`) {
		t.Error("the static mail.example.com placeholder is still shown")
	}
	// The required marker is retained.
	if !strings.Contains(group, `HELO hostname`) || !strings.Contains(group, `required`) {
		t.Errorf("the HELO field is not marked required:\n%s", group)
	}

	// An explicit typed value is stored verbatim and overrides the default.
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"}, "cfg_mx_helo": {"mail.other.com"},
	})
	if rr.Code != 303 {
		t.Fatalf("save %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainSendingConfig(context.Background(), u.AccountID, d.ID)
	if err != nil {
		t.Fatalf("get sending config: %v", err)
	}
	dec, derr := svc.DecryptDomainSendingConfig(cfg)
	if derr != nil {
		t.Fatalf("decrypt sending config: %v", derr)
	}
	if v, _ := dec["helo"].(string); v != "mail.other.com" {
		t.Errorf("helo = %q, want %q", v, "mail.other.com")
	}

	// A blank submission saves the domain name as the HELO hostname.
	rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"},
	})
	if rr.Code != 303 {
		t.Fatalf("blank save %d %s", rr.Code, rr.Body.String())
	}
	cfg, err = svc.Store.GetDomainSendingConfig(context.Background(), u.AccountID, d.ID)
	if err != nil {
		t.Fatalf("get sending config: %v", err)
	}
	dec, derr = svc.DecryptDomainSendingConfig(cfg)
	if derr != nil {
		t.Fatalf("decrypt sending config: %v", derr)
	}
	if v, _ := dec["helo"].(string); v != d.Name {
		t.Errorf("blank save stored helo = %q, want %q", v, d.Name)
	}

	// Re-opening the editor shows the saved value, not the placeholder.
	group = mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID)
	if !strings.Contains(group, `value="`+d.Name+`"`) {
		t.Errorf("the saved HELO is not shown on reopen:\n%s", group)
	}
}

// TestDirectMXSendingGreetingSubdomainPrefilled proves the pre-fill uses the
// domain verbatim, including any subdomain, rather than the parent.
func TestDirectMXSendingGreetingSubdomainPrefilled(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	sub, err := svc.Store.CreateDomain(context.Background(), u.AccountID, "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}

	group := mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+sub.ID).Body.String(), sub.ID)
	if !strings.Contains(group, `value="mail.example.com"`) {
		t.Errorf("subdomain HELO is not pre-filled verbatim:\n%s", group)
	}
}
