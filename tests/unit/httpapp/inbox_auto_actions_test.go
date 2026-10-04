package httpapp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIInboxAutoActionsPatch exercises the delivery-triggered auto-action
// fields on PATCH /v1/inboxes/{id}: booleans and integers round-trip, an
// explicit null clears the auto-trash window, an invalid trigger is rejected,
// and a non-positive hour count is rejected.
func TestAPIInboxAutoActionsPatch(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
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

	// Enable both actions and the all trigger.
	body := `{"auto_mark_read_on_delivery":true,"auto_trash_after_delivery_hours":48,"delivery_trigger":"all"}`
	if rr := do("PATCH", "/v1/inboxes/"+box.ID, body); rr.Code != 200 {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoMarkReadOnDelivery || got.AutoTrashAfterDeliveryHours == nil || *got.AutoTrashAfterDeliveryHours != 48 || got.DeliveryTrigger != "all" {
		t.Fatalf("auto-actions not applied: %#v", got)
	}
	// The fields are exposed on the inbox read model too.
	if rr := do("GET", "/v1/inboxes/"+box.ID, ""); !strings.Contains(rr.Body.String(), `"auto_trash_after_delivery_hours":48`) {
		t.Fatalf("auto-trash not exposed: %s", rr.Body.String())
	}

	// Clearing auto-trash with an explicit null keeps the other fields.
	if rr := do("PATCH", "/v1/inboxes/"+box.ID, `{"auto_trash_after_delivery_hours":null}`); rr.Code != 200 {
		t.Fatalf("clear %d %s", rr.Code, rr.Body.String())
	}
	got, _ = svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.AutoTrashAfterDeliveryHours != nil || !got.AutoMarkReadOnDelivery {
		t.Fatalf("clear side effects: %#v", got)
	}

	// Invalid trigger and non-positive hours are rejected.
	if rr := do("PATCH", "/v1/inboxes/"+box.ID, `{"delivery_trigger":"sometimes"}`); rr.Code != 400 && rr.Code != 403 {
		t.Fatalf("invalid trigger code %d %s", rr.Code, rr.Body.String())
	}
	if rr := do("PATCH", "/v1/inboxes/"+box.ID, `{"auto_trash_after_delivery_hours":0}`); rr.Code != 400 && rr.Code != 403 {
		t.Fatalf("zero hours code %d %s", rr.Code, rr.Body.String())
	}
}
