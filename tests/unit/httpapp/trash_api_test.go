package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestAPIMessageTrashLifecycle exercises the message trash/restore/purge API:
// DELETE moves to Trash, the trashed filter lists it, restore returns it, and
// purge erases it.
func TestAPIMessageTrashLifecycle(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := seedInbound(t, svc, box, "api-trash-1", "<api-trash-1@test>", "Trash me", "body")
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// DELETE trashes.
	if rr := do("DELETE", "/v1/messages/"+m.ID, ""); rr.Code != 204 {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	// Hidden from the ordinary list.
	if rr := do("GET", "/v1/messages?inbox="+box.ID, ""); strings.Contains(rr.Body.String(), m.ID) {
		t.Fatalf("trashed message in default list: %s", rr.Body.String())
	}
	// Visible via the trashed filter.
	rr := do("GET", "/v1/messages?inbox="+box.ID+"&trashed=true", "")
	var env struct {
		Items []model.Message `json:"items"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 1 || env.Items[0].ID != m.ID || env.Items[0].DeletedAt == nil {
		t.Fatalf("trashed list %#v", env.Items)
	}

	// Restore returns it.
	if rr = do("POST", "/v1/messages/"+m.ID+"/restore", ""); rr.Code != 200 {
		t.Fatalf("restore %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("GET", "/v1/messages?inbox="+box.ID, ""); !strings.Contains(rr.Body.String(), m.ID) {
		t.Fatalf("restored message missing: %s", rr.Body.String())
	}

	// Purge requires trashed first.
	if rr = do("DELETE", "/v1/messages/"+m.ID+"/purge", ""); rr.Code != 409 {
		t.Fatalf("purge untrashed %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("DELETE", "/v1/messages/"+m.ID, ""); rr.Code != 204 {
		t.Fatalf("re-trash %d", rr.Code)
	}
	if rr = do("DELETE", "/v1/messages/"+m.ID+"/purge", ""); rr.Code != 204 {
		t.Fatalf("purge %d %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, m.ID); err == nil {
		t.Fatal("purged message still present")
	}
}

// TestAPIInboxTrashEmpty exercises the empty-trash route.
func TestAPIInboxTrashEmpty(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	a := seedInbound(t, svc, box, "api-et-1", "<api-et-1@test>", "a", "a")
	b := seedInbound(t, svc, box, "api-et-2", "<api-et-2@test>", "b", "b")
	keep := seedInbound(t, svc, box, "api-et-3", "<api-et-3@test>", "keep", "keep")
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := svc.Store.TrashMessage(ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", "/v1/inboxes/"+box.ID+"/trash/empty", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"purged":2`) {
		t.Fatalf("empty trash %d %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, keep.ID); err != nil {
		t.Fatalf("untrashed message removed: %v", err)
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, a.ID); err == nil {
		t.Fatal("trashed message not purged")
	}
}

// TestAPIAccountSettingsTrashRetention covers the account settings route.
func TestAPIAccountSettingsTrashRetention(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/account/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := do("GET", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"trash_retention_days":0`) {
		t.Fatalf("get settings %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("PATCH", `{"trash_retention_days":7}`); rr.Code != 200 {
		t.Fatalf("patch settings %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("GET", ""); !strings.Contains(rr.Body.String(), `"trash_retention_days":7`) {
		t.Fatalf("settings not updated: %s", rr.Body.String())
	}
}
