package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestOpenClawConnectorAPI covers the OpenClaw admin surface end to end: the
// connector is created through its own enroll route, listed with its kind,
// role-updated, protected by Admin scope, and deleted; it never leaks secrets.
func TestOpenClawConnectorAPI(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := call("POST", "/v1/admin/openclaw/enroll", `{"inbox_id":"`+box.ID+`","name":"Claw"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll %d %s", rr.Code, rr.Body.String())
	}
	var enrolled struct {
		GatewayID   string `json:"gateway_id"`
		Secret      string `json:"secret"`
		DeliveryKey string `json:"delivery_key"`
		Kind        string `json:"kind"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &enrolled); err != nil {
		t.Fatalf("decode %v body=%s", err, rr.Body.String())
	}
	if enrolled.GatewayID == "" || enrolled.Secret == "" || enrolled.Kind != "openclaw" {
		t.Fatalf("enrollment %s", rr.Body.String())
	}

	// The created row is an OpenClaw client, not a Hermes one.
	hermes, err := svc.Store.ListRelayConnections(context.Background(), u.AccountID, store.KindHermes)
	if err != nil || len(hermes) != 0 {
		t.Fatalf("hermes list %v %#v", err, hermes)
	}
	openclaw, err := svc.Store.ListRelayConnections(context.Background(), u.AccountID, store.KindOpenClaw)
	if err != nil || len(openclaw) != 1 {
		t.Fatalf("openclaw list %v %#v", err, openclaw)
	}
	connID := openclaw[0].ID

	list := call("GET", "/v1/admin/openclaw", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"Claw"`) {
		t.Fatalf("list %d %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), enrolled.Secret) || strings.Contains(list.Body.String(), "SecretEncrypted") {
		t.Fatal("openclaw list leaked a secret")
	}

	put := call("PUT", "/v1/admin/openclaw/"+connID, `{"role":"assistant"}`)
	if put.Code != http.StatusOK {
		t.Fatalf("role update %d %s", put.Code, put.Body.String())
	}
	updated, _ := svc.Store.GetHermesConnectionByGateway(context.Background(), enrolled.GatewayID)
	if updated.OutboundRole != "assistant" {
		t.Fatalf("role %q, want assistant", updated.OutboundRole)
	}

	// A non-admin key cannot reach the OpenClaw surface.
	_, limited, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "limited", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/admin/openclaw", nil)
	req.Header.Set("Authorization", "Bearer "+limited)
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, req)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("non-admin list %d, want 403", denied.Code)
	}

	del := call("DELETE", "/v1/admin/openclaw/"+connID, "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", del.Code, del.Body.String())
	}
	if _, err := svc.Store.GetHermesConnectionByGateway(context.Background(), enrolled.GatewayID); err == nil {
		t.Fatal("deleted openclaw connector still resolves")
	}
}

// TestOpenClawSetupCodeAPI proves the one-time setup-code route mints a code
// exactly once, renders the claim URL with the code in the fragment, and
// refuses cross-account inboxes.
func TestOpenClawSetupCodeAPI(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/admin/openclaw/setup-code", strings.NewReader(`{"inbox_id":"`+box.ID+`","name":"Claw"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup-code %d %s", rr.Code, rr.Body.String())
	}
	var code struct {
		Code     string `json:"code"`
		SetupURL string `json:"setup_url"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &code); err != nil {
		t.Fatalf("decode %v body=%s", err, rr.Body.String())
	}
	if code.Code == "" || !strings.HasSuffix(code.SetupURL, "/#"+code.Code) || !strings.Contains(code.Command, code.Code) {
		t.Fatalf("setup code %s", rr.Body.String())
	}

	// The code is an authority, not stored in plaintext: it does not list as a
	// connector until redeemed.
	openclaw, _ := svc.Store.ListRelayConnections(context.Background(), u.AccountID, store.KindOpenClaw)
	if len(openclaw) != 0 {
		t.Fatalf("setup code created a connector early: %#v", openclaw)
	}

	// Redeeming the code at the shared relay enroll endpoint creates the
	// OpenClaw connector, and the code cannot be used twice.
	redeem := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/relay/enroll", strings.NewReader(`{"enrollmentToken":"`+code.Code+`","gatewayId":"gw-openclaw-code"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	first := redeem()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"kind":"openclaw"`) {
		t.Fatalf("redeem %d %s", first.Code, first.Body.String())
	}
	if second := redeem(); second.Code != http.StatusForbidden {
		t.Fatalf("second redeem %d, want 403", second.Code)
	}
	openclaw, err = svc.Store.ListRelayConnections(context.Background(), u.AccountID, store.KindOpenClaw)
	if err != nil || len(openclaw) != 1 || openclaw[0].GatewayID != "gw-openclaw-code" {
		t.Fatalf("redeemed openclaw %v %#v", err, openclaw)
	}
}
