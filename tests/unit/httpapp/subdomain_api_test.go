package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPICreateSubdomainInheritsParent exercises the domain REST API for
// subdomain inheritance: a subdomain created under a configured parent reports
// the effective provider (inherited from the parent), and the inheritance flags
// can be toggled for a slot.
func TestAPICreateSubdomainInheritsParent(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "cloudflare", map[string]any{}, false); err != nil {
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

	rr := do("POST", "/v1/admin/domains", `{"name":"agent.example.com"}`)
	if rr.Code != 201 {
		t.Fatalf("create subdomain %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ParentDomain     string `json:"parent_domain"`
		InheritReceiving bool   `json:"inherit_receiving"`
		ReceivingFrom    string `json:"receiving_inherited_from"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ParentDomain != "example.com" || !created.InheritReceiving || created.ReceivingFrom != "example.com" {
		t.Fatalf("subdomain response %+v", created)
	}

	// Opt out of receiving inheritance.
	if rr = do("PATCH", "/v1/admin/domains/"+created.ID, `{"inherit_receiving":false}`); rr.Code != 200 {
		t.Fatalf("patch inherit %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetDomain(ctx, u.AccountID, created.ID)
	if err != nil || got.InheritReceiving || got.ReceivingProvider != "" {
		t.Fatalf("after opt-out %+v err=%v", got, err)
	}

	// Opt back in.
	if rr = do("PATCH", "/v1/admin/domains/"+created.ID, `{"inherit_receiving":true}`); rr.Code != 200 {
		t.Fatalf("patch inherit back %d %s", rr.Code, rr.Body.String())
	}
	got, err = svc.Store.GetDomain(ctx, u.AccountID, created.ID)
	if err != nil || !got.InheritReceiving || got.ReceivingProvider != "cloudflare" || got.ReceivingInheritedFrom != "example.com" {
		t.Fatalf("after opt-in %+v err=%v", got, err)
	}

	// Create with inheritance explicitly disabled for both slots.
	rr = do("POST", "/v1/admin/domains", `{"name":"solo.example.com","inherit_receiving":false,"inherit_sending":false}`)
	if rr.Code != 201 {
		t.Fatalf("create solo %d %s", rr.Code, rr.Body.String())
	}
	var solo struct {
		ID               string `json:"id"`
		InheritReceiving bool   `json:"inherit_receiving"`
		InheritSending   bool   `json:"inherit_sending"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &solo); err != nil {
		t.Fatal(err)
	}
	if solo.InheritReceiving || solo.InheritSending {
		t.Fatalf("explicit no-inherit ignored %+v", solo)
	}
}
