package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/transport/cloudflare"
)

func TestDomainReceivingConfigREST(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
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
	base := "/v1/admin/domains/" + dom.ID + "/receiving"

	// A configured mailgun fixture is visible with a redacted config and a
	// provider-specific webhook URL.
	rr := do("GET", base, "")
	if rr.Code != 200 {
		t.Fatalf("get configured %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), testMailgunKey) {
		t.Fatal("signing key leaked in receiving GET")
	}
	var view struct {
		DomainID   string         `json:"domain_id"`
		Configured bool           `json:"configured"`
		Provider   string         `json:"provider"`
		Config     map[string]any `json:"config"`
		WebhookURL string         `json:"webhook_url"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Configured || view.Provider != "mailgun" {
		t.Fatalf("configured view %+v", view)
	}
	if view.Config["signing_key"] != nil {
		t.Fatalf("secret field present in redacted config: %+v", view.Config)
	}
	if view.WebhookURL != "http://example.test/internal/ingest/mailgun/raw-mime" {
		t.Fatalf("webhook url %q", view.WebhookURL)
	}

	// Missing required secret on a new provider is a validation error.
	if rr = do("PUT", base, `{"provider":"resend","config":{}}`); rr.Code != 400 {
		t.Fatalf("missing secret %d %s", rr.Code, rr.Body.String())
	}
	// Explicit secret create succeeds; the secret is never echoed back.
	if rr = do("PUT", base, `{"provider":"cloudflare","config":{"webhook_secret":"top-secret"}}`); rr.Code != 200 {
		t.Fatalf("put receiving %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "top-secret") {
		t.Fatalf("secret leaked in receiving PUT: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"webhook_url":"http://example.test/internal/ingest/cloudflare"`) {
		t.Fatalf("missing cloudflare webhook url: %s", rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.ReceivingProvider != "cloudflare" {
		t.Fatalf("domain receiving provider %+v", got)
	}
	// Saving the same provider with a blank secret retains the stored secret.
	if rr = do("PUT", base, `{"provider":"cloudflare","config":{}}`); rr.Code != 200 {
		t.Fatalf("retain secret %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.ReceivingProvider != "cloudflare" {
		t.Fatalf("domain receiving provider after retain %+v", got)
	}
	// A foreign or missing domain is a 404.
	if rr = do("GET", "/v1/admin/domains/dom_missing/receiving", ""); rr.Code != 404 {
		t.Fatalf("missing domain %d", rr.Code)
	}
	// Non-admin keys are forbidden.
	_, plainKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", base, strings.NewReader(`{"provider":"cloudflare","config":{"webhook_secret":"x"}}`))
	req.Header.Set("Authorization", "Bearer "+plainKey)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin %d", rr.Code)
	}
	// Delete is idempotent and clears the config; a repeat delete is still 204.
	if rr = do("DELETE", base, ""); rr.Code != 204 {
		t.Fatalf("delete %d", rr.Code)
	}
	if rr = do("DELETE", base, ""); rr.Code != 204 {
		t.Fatalf("repeat delete %d", rr.Code)
	}
	rr = do("GET", base, "")
	if err = json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Configured || view.Provider != "" {
		t.Fatalf("cleared view %+v", view)
	}
}

func TestDomainReceivingGeneratedSecretReturnedOnce(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/domains/" + dom.ID + "/receiving"
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", base, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// Deleting the fixture config then creating cloudflare with no secret
	// generates one and returns it exactly once, with no-store.
	req := httptest.NewRequest("DELETE", base, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	rr = put(`{"provider":"cloudflare","config":{}}`)
	if rr.Code != 200 {
		t.Fatalf("generate %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("config responses must be no-store: %q", rr.Header().Get("Cache-Control"))
	}
	var result struct {
		Generated map[string]string `json:"generated"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	secret := result.Generated["webhook_secret"]
	if secret == "" {
		t.Fatalf("expected generated webhook_secret, got %s", rr.Body.String())
	}
	// A subsequent GET never returns the generated value.
	req = httptest.NewRequest("GET", base, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("generated secret leaked on GET: %s", rr.Body.String())
	}
}

func TestInboundCanonicalRoutesBothListeners(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	inbound := httpapp.New(svc, nil).InboundHandler()

	cfReq := func() *http.Request {
		raw := "From: sender@outside.test\r\nTo: hermes@example.com\r\nSubject: cf\r\n\r\nhi"
		r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader(raw))
		r.Header.Set("Content-Type", "message/rfc822")
		r.Header.Set(cloudflare.HeaderRecipient, box.Address)
		r.Header.Set("Authorization", "Bearer "+testCFSecret)
		return r
	}
	for name, handler := range map[string]http.Handler{"main": h, "inbound": inbound} {
		if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret}, false); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, cfReq())
		if rr.Code != http.StatusOK {
			t.Fatalf("%s cloudflare = %d %s", name, rr.Code, rr.Body.String())
		}
		if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey}, false); err != nil {
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

func TestUIDomainReceivingConfigRequiresCSRF(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	post := func(csrfValue string) *httptest.ResponseRecorder {
		body := "provider=cloudflare&_csrf=" + csrfValue
		r := httptest.NewRequest("POST", "/ui/domains/"+dom.ID+"/receiving", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	// Missing CSRF is rejected before any config work happens.
	if rr := post("wrong"); rr.Code != http.StatusForbidden {
		t.Fatalf("csrf %d", rr.Code)
	}
	// A valid token reaches the handler (which must not reject it as CSRF).
	if rr := post(csrf); rr.Code == http.StatusForbidden {
		t.Fatalf("valid csrf rejected: %d %s", rr.Code, rr.Body.String())
	}
}
