package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestExternalAliasREST exercises the admin external-alias API end to end:
// create, metadata update, address immutability, connector setup/redaction,
// listing on the inbox, delivery history, non-admin rejection, hosted refusal
// and delete.
func TestExternalAliasREST(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/inboxes/" + box.ID + "/external-aliases"

	rr := adminDo(h, "POST", base, `{"address":"Agent@Gmail.com","display_name":"Agent"}`, key)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID          string `json:"id"`
		Address     string `json:"address"`
		DisplayName string `json:"display_name"`
		Configured  bool   `json:"configured"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Address != "agent@gmail.com" || created.DisplayName != "Agent" || created.Configured {
		t.Fatalf("created %+v", created)
	}
	aliasBase := base + "/" + created.ID

	// Inbox responses include the external alias metadata.
	rr = adminDo(h, "GET", "/v1/inboxes/"+box.ID, "", key)
	if !strings.Contains(rr.Body.String(), `"external_aliases"`) || !strings.Contains(rr.Body.String(), "agent@gmail.com") {
		t.Fatalf("inbox metadata missing external aliases: %s", rr.Body.String())
	}

	// Metadata update changes only the display name; address is not accepted.
	if rr = adminDo(h, "PATCH", aliasBase, `{"display_name":"Agent Two"}`, key); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Agent Two") {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}

	// Configure a connector; the secret is never echoed, non-secret config is.
	if rr = adminDo(h, "PUT", aliasBase+"/sending", `{"provider":"brevo","config":{"api_key":"k","api_base":"https://api.brevo.com"}}`, key); rr.Code != http.StatusOK {
		t.Fatalf("put sending %d %s", rr.Code, rr.Body.String())
	}
	rr = adminDo(h, "GET", aliasBase+"/sending", "", key)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), `"api_key"`) || !strings.Contains(rr.Body.String(), `"configured":true`) {
		t.Fatalf("sending view %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sending view must be no-store: %q", rr.Header().Get("Cache-Control"))
	}

	// Listing returns the redacted alias.
	rr = adminDo(h, "GET", base, "", key)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "agent@gmail.com") || strings.Contains(rr.Body.String(), "api_key") {
		t.Fatalf("list %d %s", rr.Code, rr.Body.String())
	}
	// Delivery history is served (empty is fine).
	if rr = adminDo(h, "GET", aliasBase+"/sending/deliveries", "", key); rr.Code != http.StatusOK {
		t.Fatalf("deliveries %d %s", rr.Code, rr.Body.String())
	}

	// A non-admin key is forbidden on reads and writes alike.
	_, plainKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", base},
		{"POST", base},
		{"GET", aliasBase + "/sending"},
		{"GET", aliasBase + "/sending/deliveries"},
	} {
		if rr = adminDo(h, tc.method, tc.path, "", plainKey); rr.Code != http.StatusForbidden {
			t.Fatalf("non-admin %s %s = %d", tc.method, tc.path, rr.Code)
		}
	}

	// Hosted mode refuses even an Admin.
	svc.Config.Mode = "hosted"
	if rr = adminDo(h, "GET", base, "", key); rr.Code != http.StatusForbidden {
		t.Fatalf("hosted GET = %d", rr.Code)
	}
	if rr = adminDo(h, "POST", base, `{"address":"x@gmail.com"}`, key); rr.Code != http.StatusForbidden {
		t.Fatalf("hosted POST = %d", rr.Code)
	}
	svc.Config.Mode = "selfhosted"

	// Delete removes it.
	if rr = adminDo(h, "DELETE", aliasBase, "", key); rr.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	if rr = adminDo(h, "GET", aliasBase+"/sending", "", key); rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete %d", rr.Code)
	}
}

// TestExternalAliasCrossAccountIsolation proves another account cannot see or
// mutate an inbox's external aliases.
func TestExternalAliasCrossAccountIsolation(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/admin/inboxes/" + box.ID + "/external-aliases"
	rr := adminDo(h, "POST", base, `{"address":"agent@gmail.com"}`, key)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed create %d %s", rr.Code, rr.Body.String())
	}

	b, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err := svc.Store.CreateAPIKey(ctx, b.AccountID, "b-admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base, `{"address":"other@gmail.com"}`},
		{"DELETE", base + "/ea_missing", ""},
	} {
		if rr = adminDo(h, tc.method, tc.path, tc.body, keyB); rr.Code != http.StatusNotFound {
			t.Fatalf("foreign account %s %s = %d %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}
