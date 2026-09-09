package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestAPIDomainOutboundCredential(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/v1/admin/domains/"+d.ID, strings.NewReader(`{"outbound_credential_id":"`+cred.ID+`"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("patch domain %d %s", rr.Code, rr.Body.String())
	}
	dom, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != cred.ID {
		t.Fatalf("domain credential: %v %+v", err, dom)
	}
	req = httptest.NewRequest("GET", "/v1/admin/domains", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), cred.ID) {
		t.Fatalf("list domains %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIInboxNoOutboundCredentialField(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := patch(`{"display_name":"Renamed"}`)
	if rr.Code != 200 {
		t.Fatalf("patch inbox %d %s", rr.Code, rr.Body.String())
	}
	var got model.Inbox
	if err = json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Renamed" {
		t.Fatalf("display name not updated: %+v", got)
	}
	if strings.Contains(rr.Body.String(), "outbound_credential_id") {
		t.Fatalf("inbox response must not expose outbound_credential_id: %s", rr.Body.String())
	}

	// The deprecated field is rejected outright rather than silently ignored.
	rr = patch(`{"outbound_credential_id":"` + cred.ID + `"}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "outbound_credential_id") {
		t.Fatalf("removed field should be rejected: %d %s", rr.Code, rr.Body.String())
	}
}

func TestUIDomainProviderAssignment(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("POST", "/ui/domains/"+d.ID+"/edit", strings.NewReader("provider="+cred.ID+"&_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("update domain %d %s", rr.Code, rr.Body.String())
	}
	dom, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != cred.ID {
		t.Fatalf("domain credential: %v %+v", err, dom)
	}
}

const testAddProviderOption = "__add_provider__"

func TestUIDomainEditIgnoresAddProviderSentinel(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("POST", "/ui/domains/"+d.ID+"/edit", strings.NewReader("provider="+testAddProviderOption+"&_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("update domain %d %s", rr.Code, rr.Body.String())
	}
	dom, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != cred.ID {
		t.Fatalf("sentinel must not clear the provider: %v %+v", err, dom)
	}
}

func TestUIDomainCreateWithProvider(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"name": {"new.example"}, "provider": {cred.ID}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/domains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("create domain %d %s", rr.Code, rr.Body.String())
	}
	domains, err := svc.Store.ListDomains(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range domains {
		if d.Name == "new.example" {
			if d.OutboundCredentialID != cred.ID {
				t.Fatalf("domain provider = %q, want %q", d.OutboundCredentialID, cred.ID)
			}
			return
		}
	}
	t.Fatal("created domain not found")
}

func TestAPICreateDomainWithCredential(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/admin/domains", strings.NewReader(`{"name":"api.example","outbound_credential_id":"`+cred.ID+`"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("create domain %d %s", rr.Code, rr.Body.String())
	}
	var dom model.Domain
	if err = json.Unmarshal(rr.Body.Bytes(), &dom); err != nil {
		t.Fatal(err)
	}
	if dom.Name != "api.example" || dom.OutboundCredentialID != cred.ID {
		t.Fatalf("created domain %+v", dom)
	}
}

func TestUIDomainCreateWithAddProviderRedirect(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"name": {"addnew.example"}, "provider": {testAddProviderOption}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/domains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("create domain %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	const prefix = "/dashboard?tab=settings&add_provider_for="
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("location %q", loc)
	}
	domainID := strings.TrimPrefix(loc, prefix)
	dom, err := svc.Store.GetDomain(ctx, u.AccountID, domainID)
	if err != nil || dom.OutboundCredentialID != "" {
		t.Fatalf("domain should exist with no provider: %v %+v", err, dom)
	}
	req = httptest.NewRequest("GET", loc, nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`name="assign_domain" value="` + domainID + `"`, `<option value="smtp" selected>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

func TestDashboardDomainSendingState(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Primary", "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.CreateDomain(ctx, u.AccountID, "paused.example"); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Sending", "sending paused", "Primary", "domain-provider", "domain-provider-status", "__add_provider__", "add-domain-provider", "No sending provider for paused.example"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}
