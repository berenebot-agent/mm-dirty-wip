package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// TestDomainSendingConfigREST exercises the canonical per-domain sending config
// API: unconfigured GET, redacted PUT/GET, validation, provider switching,
// non-admin rejection, idempotent delete, and no-store responses.
func TestDomainSendingConfigREST(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	base := "/v1/admin/domains/" + dom.ID + "/sending"

	// The fixture domain has a receive path but no sending config.
	rr := do("GET", base, "", key)
	if rr.Code != 200 {
		t.Fatalf("get unconfigured %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"configured":false`) || !strings.Contains(rr.Body.String(), `"config":{}`) {
		t.Fatalf("unconfigured view %s", rr.Body.String())
	}

	// Create a Brevo sending config; the secret is never echoed.
	rr = do("PUT", base, `{"provider":"brevo","config":{"api_key":"k","api_base":"https://api.brevo.com"}}`, key)
	if rr.Code != 200 {
		t.Fatalf("put sending %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("config responses must be no-store: %q", rr.Header().Get("Cache-Control"))
	}
	if strings.Contains(rr.Body.String(), `"api_key"`) {
		t.Fatalf("sending secret leaked: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"api_base":"https://api.brevo.com"`) || !strings.Contains(rr.Body.String(), `"provider":"brevo"`) {
		t.Fatalf("redacted sending view %s", rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.SendingProvider != "brevo" {
		t.Fatalf("domain sending provider %+v", got)
	}

	// Invalid SMTP port is a validation error.
	if rr = do("PUT", base, `{"provider":"smtp","config":{"host":"smtp.example.com","port":"nope"}}`, key); rr.Code != 400 {
		t.Fatalf("bad smtp port %d %s", rr.Code, rr.Body.String())
	}
	// Unknown schema keys are rejected.
	if rr = do("PUT", base, `{"provider":"brevo","config":{"api_key":"k","bogus":"x"}}`, key); rr.Code != 400 {
		t.Fatalf("unknown key %d %s", rr.Code, rr.Body.String())
	}
	// Unknown provider is a validation error, not a 500.
	if rr = do("PUT", base, `{"provider":"nope","config":{}}`, key); rr.Code != 400 {
		t.Fatalf("unknown provider %d %s", rr.Code, rr.Body.String())
	}
	// A provider switch never reuses old fields/secrets.
	if rr = do("PUT", base, `{"provider":"mailgun","config":{"api_key":"mg","domain":"mg.example.com"}}`, key); rr.Code != 200 {
		t.Fatalf("provider switch %d %s", rr.Code, rr.Body.String())
	}
	var view struct {
		Provider string         `json:"provider"`
		Config   map[string]any `json:"config"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Provider != "mailgun" || view.Config["domain"] != "mg.example.com" || view.Config["api_key"] != nil {
		t.Fatalf("switched view %+v", view)
	}

	// Missing domain -> 404; non-admin -> 403.
	if rr = do("GET", "/v1/admin/domains/dom_missing/sending", "", key); rr.Code != 404 {
		t.Fatalf("missing domain %d", rr.Code)
	}
	_, plainKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr = do("DELETE", base, "", plainKey); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin %d", rr.Code)
	}

	// Delete clears the config and is idempotent.
	if rr = do("DELETE", base, "", key); rr.Code != 204 {
		t.Fatalf("delete %d", rr.Code)
	}
	if rr = do("DELETE", base, "", key); rr.Code != 204 {
		t.Fatalf("repeat delete %d", rr.Code)
	}
	rr = do("GET", base, "", key)
	if !strings.Contains(rr.Body.String(), `"configured":false`) {
		t.Fatalf("not cleared: %s", rr.Body.String())
	}
}

// adminDo issues an authenticated admin API request.
func adminDo(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestDomainConfigCrossAccountIsolation proves another account can neither read
// nor mutate a domain's sending, receiving or delivery config.
func TestDomainConfigCrossAccountIsolation(t *testing.T) {
	svc, h, _, dom, _ := httpFixture(t)
	ctx := context.Background()
	b, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err := svc.Store.CreateAPIKey(ctx, b.AccountID, "b-admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/domains/" + dom.ID
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", base + "/sending", ""},
		{"PUT", base + "/sending", `{"provider":"brevo","config":{"api_key":"k","api_base":"https://api.brevo.com"}}`},
		{"DELETE", base + "/sending", ""},
		{"GET", base + "/receiving", ""},
		{"PUT", base + "/receiving", `{"provider":"cloudflare","config":{"webhook_secret":"x"}}`},
		{"DELETE", base + "/receiving", ""},
		{"GET", base + "/sending/deliveries", ""},
	} {
		if rr := adminDo(h, tc.method, tc.path, tc.body, keyB); rr.Code != http.StatusNotFound {
			t.Fatalf("foreign account %s %s = %d %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// TestDomainDeliveryHistoryIndependentOfConfig proves past attempts remain
// visible after the sending config is deleted or the provider is switched.
func TestDomainDeliveryHistoryIndependentOfConfig(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "History", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/domains/" + dom.ID + "/sending"
	hasHistory := func() bool {
		rr := adminDo(h, "GET", base+"/deliveries", "", key)
		return rr.Code == 200 && strings.Contains(rr.Body.String(), res.Message.ID)
	}
	if !hasHistory() {
		t.Fatalf("history missing while configured")
	}
	if rr := adminDo(h, "DELETE", base, "", key); rr.Code != 204 {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if !hasHistory() {
		t.Fatalf("history hidden after config delete")
	}
	if rr := adminDo(h, "PUT", base, `{"provider":"mailgun","config":{"api_key":"mg","domain":"mg.example.com"}}`, key); rr.Code != 200 {
		t.Fatalf("provider switch %d %s", rr.Code, rr.Body.String())
	}
	if !hasHistory() {
		t.Fatalf("history hidden after provider switch")
	}
}

// TestDomainConfigRepeatedGETNeverLeaksOrRotatesSecret proves repeated redacted
// GETs never expose a stored secret and never mint a new generated secret.
func TestDomainConfigRepeatedGETNeverLeaksOrRotatesSecret(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A supplied secret must not be echoed or replaced by repeated GETs.
	base := "/v1/admin/domains/" + dom.ID + "/receiving"
	if rr := adminDo(h, "PUT", base, `{"provider":"cloudflare","config":{"webhook_secret":"supplied-secret"}}`, key); rr.Code != 200 {
		t.Fatalf("put receiving %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < 3; i++ {
		rr := adminDo(h, "GET", base, "", key)
		if rr.Code != 200 {
			t.Fatalf("get receiving %d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), "supplied-secret") {
			t.Fatalf("stored secret leaked on GET %d", i)
		}
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, dom.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["webhook_secret"] != "supplied-secret" {
		t.Fatalf("GET rotated the stored secret: %+v", dec["webhook_secret"])
	}
	// Sending config behaves the same.
	sbase := "/v1/admin/domains/" + dom.ID + "/sending"
	if rr := adminDo(h, "PUT", sbase, `{"provider":"brevo","config":{"api_key":"brevo-secret","api_base":"https://api.brevo.com"}}`, key); rr.Code != 200 {
		t.Fatalf("put sending %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < 3; i++ {
		if rr := adminDo(h, "GET", sbase, "", key); strings.Contains(rr.Body.String(), "brevo-secret") {
			t.Fatalf("sending secret leaked on GET %d", i)
		}
	}
}

// TestRemovedConfigRoutesAreGone proves the retired connector routes and domain
// assignment selector no longer exist.
func TestRemovedConfigRoutesAreGone(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The UI home is registered as "GET /", so an unknown path with a non-GET
	// method is 405 rather than 404; either way the retired route is gone.
	absent := func(code int) bool {
		return code == http.StatusNotFound || code == http.StatusMethodNotAllowed
	}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/admin/outbound"},
		{"POST", "/v1/admin/inbound"},
		{"PATCH", "/v1/admin/outbound/out_x"},
		{"DELETE", "/v1/admin/outbound/out_x"},
		{"PATCH", "/v1/admin/inbound/in_x"},
		{"DELETE", "/v1/admin/inbound/in_x"},
	} {
		if rr := adminDo(h, tc.method, tc.path, "{}", key); !absent(rr.Code) {
			t.Fatalf("retired API route %s %s = %d", tc.method, tc.path, rr.Code)
		}
	}

	cookie, csrf := uiSession(t, svc, u.ID)
	for _, path := range []string{"/ui/outbound", "/ui/inbound", "/ui/inbound/in_x/regenerate", "/ui/domains/" + dom.ID + "/edit"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("_csrf="+csrf))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if !absent(rr.Code) {
			t.Fatalf("retired UI route %s = %d", path, rr.Code)
		}
	}
}

// TestAPIDomainPatchRejectsRetiredFields proves the domain PATCH DTO no longer
// accepts credential-assignment fields.
func TestAPIDomainPatchRejectsRetiredFields(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"outbound_credential_id", "inbound_credential_id"} {
		if rr := adminDo(h, "PATCH", "/v1/admin/domains/"+dom.ID, `{"`+field+`":"x"}`, key); rr.Code != http.StatusBadRequest {
			t.Fatalf("retired PATCH field %s should be rejected: %d %s", field, rr.Code, rr.Body.String())
		}
	}
}

// TestDomainSendingDeliveriesEmptyArray proves an empty history is serialised as
// [] rather than null.
func TestDomainSendingDeliveriesEmptyArray(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := adminDo(h, "GET", "/v1/admin/domains/"+dom.ID+"/sending/deliveries", "", key)
	if rr.Code != http.StatusOK {
		t.Fatalf("deliveries %d %s", rr.Code, rr.Body.String())
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "[]" {
		t.Fatalf("empty delivery history should be [], got %q", body)
	}
}
