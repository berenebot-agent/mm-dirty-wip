package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHermesEnvBlock(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/admin/hermes/enroll", strings.NewReader(`{"inbox_id":"`+box.ID+`","name":"gw"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll %d %s", rr.Code, rr.Body.String())
	}
	var got struct {
		GatewayID   string `json:"gateway_id"`
		Secret      string `json:"secret"`
		DeliveryKey string `json:"delivery_key"`
		Env         string `json:"env"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	want := "GATEWAY_RELAY_URL=http://example.test\n" +
		"GATEWAY_RELAY_ID=" + got.GatewayID + "\n" +
		"GATEWAY_RELAY_SECRET=" + got.Secret + "\n" +
		"GATEWAY_RELAY_DELIVERY_KEY=" + got.DeliveryKey + "\n" +
		"GATEWAY_RELAY_PLATFORMS=email\n" +
		"GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true"
	if got.Env != want {
		t.Fatalf("env block:\n%s\nwant:\n%s", got.Env, want)
	}
}

func TestDashboardRendersKeyDialog(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Clients", `id="key-dialog"`, `id="key-form"`, `id="key-result"`, `id="key-copy"`, `data-type="hermes"`, "Hermes relay", "Add Client", `data-inbox-tab="connectors"`, `id="inbox-connectors-list"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if !strings.Contains(body, `id="inbox-auto-trash"`) || !strings.Contains(body, `id="inbox-connectors-form"`) {
		t.Fatal("inbox-level delivery settings are missing from inbox settings")
	}
	assetReq := httptest.NewRequest("GET", "/assets/app.js", nil)
	assetRR := httptest.NewRecorder()
	h.ServeHTTP(assetRR, assetReq)
	asset := assetRR.Body.String()
	if assetRR.Code != http.StatusOK ||
		!strings.Contains(asset, "section.remove();") ||
		!strings.Contains(asset, "insertBefore(bearerFields, webhookAuth.nextElementSibling)") ||
		!strings.Contains(asset, "key-form-scroll") ||
		!strings.Contains(asset, "connector-config-row") ||
		!strings.Contains(asset, "inboxSaveButton.setAttribute('form', editing ? 'inbox-connector-edit-form' : 'inbox-connectors-form')") ||
		!strings.Contains(asset, "showInboxSubview") ||
		!strings.Contains(asset, "#key-dialog #key-form>.dialog-actions") {
		t.Fatalf("connector setup script does not remove inbox-level controls: status=%d", assetRR.Code)
	}
	if strings.Contains(body, "<h2>Hermes Relay</h2>") {
		t.Fatal("standalone Hermes Relay card should be removed")
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline event handlers are blocked by CSP and must not be used")
	}
}

// TestDashboardRendersClientDeleteDialog locks in that a client row routes its
// delete through the confirmation dialog (which names the client and type)
// rather than a data-confirm attribute, and that the delete route still works.
func TestDashboardRendersClientDeleteDialog(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="client-delete-dialog"`,
		`id="client-delete-form"`,
		`id="client-delete-label"`,
		`id="client-delete-submit"`,
		`class="secondary icon-btn danger open-delete-client" data-kind="keys" data-id="` + key.ID + `" data-name="Agent" data-type="API key"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	post := httptest.NewRecorder()
	delReq := httptest.NewRequest("POST", "/ui/keys/"+key.ID+"/delete", strings.NewReader("_csrf="+csrf))
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.AddCookie(cookie)
	h.ServeHTTP(post, delReq)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("delete key %d: %s", post.Code, post.Body.String())
	}
}

func TestCreateKeyReturnsJSONSecret(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"type": {"api"}, "name": {"Agent"}, "_csrf": {csrf}, "role_" + box.ID: {"read"}}
	req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create client = %d body=%s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got["notice"] != "API key created" || !strings.Contains(got["label"], "will not be shown again") || got["secret"] == "" {
		t.Fatalf("unexpected response %#v", got)
	}
}

func TestCreateKeyRedirectsWithoutJSONAccept(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"type": {"api"}, "name": {"Agent"}, "_csrf": {csrf}, "role_" + box.ID: {"read"}}
	req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create client = %d body=%s", rr.Code, rr.Body.String())
	}
}
