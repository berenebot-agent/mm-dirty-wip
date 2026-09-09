package httpapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/transport/cloudflare"
)

func TestInboundRESTCRUDAndRedaction(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// Create.
	rr := do("POST", "/v1/admin/inbound", `{"name":"CF","provider":"cloudflare","config":{"webhook_secret":"top-secret"}}`)
	if rr.Code != 201 {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var created struct{ ID, Name, Provider string }
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create body %s err=%v", rr.Body.String(), err)
	}
	// List redacts the secret.
	rr = do("GET", "/v1/admin/inbound", "")
	if rr.Code != 200 {
		t.Fatalf("list %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "top-secret") {
		t.Fatal("secret leaked in list")
	}
	var list []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("list %s err=%v", rr.Body.String(), err)
	}
	// Update name with blank secret retains stored secret.
	rr = do("PATCH", "/v1/admin/inbound/"+created.ID, `{"name":"Renamed"}`)
	if rr.Code != 200 {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}
	cred, err := svc.Store.GetInboundCredential(ctx, u.AccountID, created.ID)
	if err != nil || cred.Name != "Renamed" {
		t.Fatalf("cred %+v err=%v", cred, err)
	}
	if cfg, err := svc.DecryptInboundCredential(cred); err != nil || cfg["webhook_secret"] != "top-secret" {
		t.Fatalf("secret not retained: %#v err=%v", cfg, err)
	}
	// Provider is immutable.
	if rr = do("PATCH", "/v1/admin/inbound/"+created.ID, `{"provider":"mailgun"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("provider change %d", rr.Code)
	}
	// Missing required secret is rejected.
	if rr = do("POST", "/v1/admin/inbound", `{"provider":"mailgun","config":{}}`); rr.Code != 400 {
		t.Fatalf("missing secret %d", rr.Code)
	}
	// Delete.
	if rr = do("DELETE", "/v1/admin/inbound/"+created.ID, ""); rr.Code != 204 {
		t.Fatalf("delete %d", rr.Code)
	}
	if _, err := svc.Store.GetInboundCredential(ctx, u.AccountID, created.ID); err == nil {
		t.Fatal("credential still present")
	}
}

func TestInboundDomainAssignmentREST(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := svc.SaveInboundCredential(ctx, u.AccountID, "", "CF", "cloudflare", map[string]any{"webhook_secret": "s"})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do("PATCH", "/v1/admin/domains/"+dom.ID, `{"inbound_credential_id":"`+cred.ID+`"}`); rr.Code != 200 {
		t.Fatalf("assign %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.InboundCredentialID != cred.ID {
		t.Fatalf("assignment %+v", got)
	}
	if rr := do("PATCH", "/v1/admin/domains/"+dom.ID, `{"inbound_credential_id":""}`); rr.Code != 200 {
		t.Fatalf("clear %d", rr.Code)
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.InboundCredentialID != "" {
		t.Fatalf("not cleared %+v", got)
	}
	// Foreign credential is forbidden.
	if rr := do("PATCH", "/v1/admin/domains/"+dom.ID, `{"inbound_credential_id":"inb_missing"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign assign %d", rr.Code)
	}
}

func TestInboundCanonicalRoutesBothListeners(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	cfCred, err := svc.SaveInboundCredential(ctx, u.AccountID, "", "CF", "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	if err != nil {
		t.Fatal(err)
	}
	creds, _ := svc.Store.ListInboundCredentials(ctx, u.AccountID)
	var mgID string
	for _, c := range creds {
		if c.Provider == "mailgun" {
			mgID = c.ID
		}
	}
	if mgID == "" {
		t.Fatal("mailgun credential missing")
	}
	inbound := New(svc, nil).InboundHandler()

	cfReq := func() *http.Request {
		raw := "From: sender@outside.test\r\nTo: hermes@example.com\r\nSubject: cf\r\n\r\nhi"
		r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader(raw))
		r.Header.Set("Content-Type", "message/rfc822")
		r.Header.Set(cloudflare.HeaderRecipient, box.Address)
		r.Header.Set("Authorization", "Bearer "+testCFSecret)
		return r
	}
	for name, handler := range map[string]http.Handler{"main": h, "inbound": inbound} {
		if err := svc.Store.SetDomainInboundCredential(ctx, u.AccountID, dom.ID, cfCred.ID); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, cfReq())
		if rr.Code != http.StatusOK {
			t.Fatalf("%s cloudflare = %d %s", name, rr.Code, rr.Body.String())
		}
		if err := svc.Store.SetDomainInboundCredential(ctx, u.AccountID, dom.ID, mgID); err != nil {
			t.Fatal(err)
		}
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, signedMGRequest(t, testMailgunKey, "route-"+name, box.Address, inboundRawMessage()))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s mailgun = %d %s", name, rr.Code, rr.Body.String())
		}
		// Legacy alias is gone.
		legacy := signedMGRequest(t, testMailgunKey, "legacy-"+name, box.Address, inboundRawMessage())
		legacy.URL.Path = "/internal/ingest/mailgun"
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, legacy)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s legacy mailgun = %d", name, rr.Code)
		}
	}
}

func TestUIInboundCSRFAndAssignment(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := func(csrfValue, assign string) *http.Request {
		body := "_csrf=" + csrfValue + "&name=UI&provider=cloudflare&icfg_cloudflare_webhook_secret=uisecret"
		if assign != "" {
			body += "&assign_domain=" + assign
		}
		r := httptest.NewRequest("POST", "/ui/inbound", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		return r
	}
	// Missing CSRF is rejected.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, form("wrong", dom.ID))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("csrf %d", rr.Code)
	}
	// Valid CSRF creates and assigns.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, form(csrf, dom.ID))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetDomain(context.Background(), u.AccountID, dom.ID)
	if err != nil || got.InboundCredentialID == "" {
		t.Fatalf("domain not assigned %+v err=%v", got, err)
	}
}

func TestDashboardRendersInboundControls(t *testing.T) {
	svc, h, _, _, _ := httpFixture(t)
	srv := New(svc, nil)
	data := pageData{CSRF: "token", BaseURL: "http://example.test", InboundProviders: inboundProviderViews()}
	rr := httptest.NewRecorder()
	srv.render(rr, dashboardBody, data)
	body := rr.Body.String()
	for _, want := range []string{
		"Add Receive Path",
		"inbound-provider-select",
		`data-provider="cloudflare"`,
		"tab=settings",
		"domain-receive",
		"add-domain-receive",
		"http://example.test/internal/ingest/mailgun/raw-mime",
		"http://example.test/internal/ingest/cloudflare",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "icfg_cloudflare_webhook_secret") {
		t.Fatal("generated Cloudflare secret field must not be rendered")
	}
	// The client script drives the receive-path select and inline dialog.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/assets/app.js", nil))
	for _, want := range []string{"inbound-dialog", "inbound-provider-select", "__add_inbound__", "domain-receive-status"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
}
