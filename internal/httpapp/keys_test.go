package httpapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestAPIKeyScope(t *testing.T) {
	cases := []struct {
		name string
		key  model.APIKey
		want string
	}{
		{"admin", model.APIKey{Admin: true, Roles: map[string]string{"in_1": "read"}}, "Admin"},
		{"none", model.APIKey{}, "None"},
		{"owner", model.APIKey{Roles: map[string]string{"in_1": "owner"}}, "Owner"},
		{"mixed", model.APIKey{Roles: map[string]string{"in_1": "read", "in_2": "owner", "in_3": "owner"}}, "Owner, Read"},
	}
	for _, tc := range cases {
		if got := apiKeyScope(tc.key); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCredentialViews(t *testing.T) {
	views := credentialViews(
		[]model.APIKey{{Name: "Agent", Roles: map[string]string{"in_1": "assistant"}}},
		[]store.HermesConnection{{Name: "Hermes", GatewayID: "gw-abc"}},
	)
	if len(views) != 2 {
		t.Fatalf("got %d views", len(views))
	}
	if views[0] != (credentialView{Kind: "api", Name: "Agent", Type: "API key", Scope: "Assistant", RolesJSON: `{"in_1":"assistant"}`}) {
		t.Fatalf("api view %#v", views[0])
	}
	if views[1] != (credentialView{Kind: "hermes", Name: "Hermes", Type: "Hermes relay", Scope: "Owner"}) {
		t.Fatalf("hermes view %#v", views[1])
	}
}

func TestHermesEnvBlock(t *testing.T) {
	got := hermesEnvBlock("https://mail.example.test/", "gw-abc", "sekret", "deliver")
	want := "GATEWAY_RELAY_URL=https://mail.example.test/\nGATEWAY_RELAY_ID=gw-abc\nGATEWAY_RELAY_SECRET=sekret\nGATEWAY_RELAY_DELIVERY_KEY=deliver\nGATEWAY_RELAY_PLATFORMS=email\nGATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true"
	if got != want {
		t.Fatalf("env block:\n%s\nwant:\n%s", got, want)
	}
}

func TestDashboardRendersKeyDialog(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	srv := New(svc, nil)
	data := pageData{
		CSRF: "token",
		Credentials: []credentialView{
			{Name: "Agent", Type: "API key", Scope: "Admin"},
			{Name: "Hermes", Type: "Hermes relay", Scope: "Owner"},
		},
	}
	rr := httptest.NewRecorder()
	srv.render(rr, dashboardBody, data)
	body := rr.Body.String()
	for _, want := range []string{"Keys &amp; connections", `id="key-dialog"`, `id="key-form"`, `id="key-result"`, `id="key-copy"`, `data-type="hermes"`, "Hermes relay", "Create Key"} {
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
		t.Fatalf("create key = %d body=%s", rr.Code, rr.Body.String())
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
		t.Fatalf("create key = %d body=%s", rr.Code, rr.Body.String())
	}
}
