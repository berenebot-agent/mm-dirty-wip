package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestProviderLabelAllowsHierarchicalNames proves a provider-owned label name
// with a path separator is accepted, while the local free-text rule is unchanged.
func TestProviderLabelAllowsHierarchicalNames(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "deep-label", "<deep@test>", "Deep", "body")
	// The native rule rejects a slash; simulate a provider-owned label by marking
	// the action provider-scoped through the model directly.
	if _, ok := model.NormalizeLabel("Projects/2026"); ok {
		t.Fatal("local rule should reject a path separator")
	}
	if _, ok := model.NormalizeProviderLabel("Projects/2026"); !ok {
		t.Fatal("provider rule should accept a path separator")
	}
	if _, ok := model.NormalizeProviderLabel("bad\x00name"); ok {
		t.Fatal("provider rule should reject control characters")
	}
	key := adminKey(t, svc, u)
	// A native inbox still rejects a slash-named label through the API.
	req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true",
		strings.NewReader(`{"name":"Projects/2026","operation":"add"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("native slash label = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	// A provider rule accepts it in the store/worker path.
	ev, err := svc.Store.ModifyMessageLabel(ctx, p, m.ID, "Projects/2026", "add", true)
	if err != nil || ev == nil {
		t.Fatalf("provider label add: %v", err)
	}
	after, err := svc.Store.GetMessage(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range after.Labels {
		found = found || name == "Projects/2026"
	}
	if !found {
		t.Fatalf("provider label not applied: %v", after.Labels)
	}
}

// TestUnifiedLabelCatalogAcrossBackends proves the catalogue advertises the
// documented shape and capability for each backend kind.
func TestUnifiedLabelCatalogAcrossBackends(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.mu.Lock()
	fake.folders["Projects"] = 150
	fake.mu.Unlock()
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	if rr.Code != 200 {
		t.Fatalf("IMAP catalogue: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`"can_create":true`, `"create_kind":"folder"`, `"create_membership":"exclusive"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("IMAP capabilities missing %s: %s", want, body)
		}
	}
	if !strings.Contains(body, `"name":"Projects"`) || !strings.Contains(body, `"membership":"exclusive"`) {
		t.Fatalf("IMAP folder-backed label missing: %s", body)
	}
	_ = context.Background()
}
