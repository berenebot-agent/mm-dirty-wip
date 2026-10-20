package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStandaloneInboxCreateRequiresAccountAdmin proves creating a standalone
// inbox is an account-level action: a non-admin key scoped to an existing inbox
// is refused, and an account-admin key succeeds. Creating a mailbox (and its
// remote connector) is Admin-only, matching the domain-inbox creation rule.
func TestStandaloneInboxCreateRequiresAccountAdmin(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, adminKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A non-admin key holding Owner on an existing inbox is still not an
	// account Admin.
	_, scopedKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "scoped", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"kind":"standalone","address":"new@standalone.test","display_name":"Standalone"}`
	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := call(scopedKey); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin standalone create = %d %s, want 403", rr.Code, rr.Body.String())
	}
	if rr := call(adminKey); rr.Code != http.StatusCreated {
		t.Fatalf("admin standalone create = %d %s, want 201", rr.Code, rr.Body.String())
	}
}
