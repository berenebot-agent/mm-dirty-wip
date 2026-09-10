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
	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Clients", `id="key-dialog"`, `id="key-form"`, `id="key-result"`, `id="key-copy"`, `data-type="hermes"`, "Hermes relay", "Add Client"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "Hermes Relay") {
		t.Fatal("standalone Hermes Relay card should be removed")
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline event handlers are blocked by CSP and must not be used")
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
	if got["notice"] != "API key created" || got["label"] == "" || got["secret"] == "" {
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
