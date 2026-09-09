package httpapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
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

func TestDashboardDomainSendingState(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	srv := New(svc, nil)
	domains := []model.Domain{{ID: "dom_1", Name: "a.example"}, {ID: "dom_2", Name: "b.example", OutboundCredentialID: "out_1"}}
	creds := []store.OutboundCredential{{ID: "out_1", Name: "Primary", Provider: "brevo"}}
	rr := httptest.NewRecorder()
	srv.render(rr, dashboardBody, pageData{
		CSRF:              "token",
		Domains:           domains,
		DomainSending:     domainSendingViews(domains, creds),
		PausedDomains:     []string{"a.example"},
		Outbound:          []outboundView{{ID: "out_1", Name: "Primary", Provider: "brevo"}},
		OutboundProviders: outboundProviderViews(),
	})
	body := rr.Body.String()
	for _, want := range []string{"Sending", "sending paused", "Primary", "domain-provider", "domain-provider-status", "domain-add-provider", "add-domain-provider", "No sending provider for a.example"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "Mail queues until a provider is set.") {
		t.Fatalf("static paused hint should be replaced by the dynamic status line")
	}
}
