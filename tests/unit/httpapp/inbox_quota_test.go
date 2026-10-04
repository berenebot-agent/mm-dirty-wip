package httpapp_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIInboxStorageQuotaPatch exercises storage_quota_bytes on
// PATCH /v1/inboxes/{id}: an admin key sets and reads it, an explicit null
// clears it, a negative value is rejected, and the read model exposes both the
// cap and the maintained usage.
func TestAPIInboxStorageQuotaPatch(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, adminKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(key, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Set a 2 MB cap and confirm it is exposed on the read model.
	if rr := do(adminKey, "PATCH", "/v1/inboxes/"+box.ID, `{"storage_quota_bytes":2097152}`); rr.Code != 200 {
		t.Fatalf("patch quota %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StorageQuotaBytes == nil || *got.StorageQuotaBytes != 2097152 {
		t.Fatalf("quota = %v, want 2097152", got.StorageQuotaBytes)
	}
	if rr := do(adminKey, "GET", "/v1/inboxes/"+box.ID, ""); !strings.Contains(rr.Body.String(), `"storage_quota_bytes":2097152`) {
		t.Fatalf("quota not exposed: %s", rr.Body.String())
	}
	if rr := do(adminKey, "GET", "/v1/inboxes/"+box.ID, ""); !strings.Contains(rr.Body.String(), `"storage_used_bytes":0`) {
		t.Fatalf("usage not exposed: %s", rr.Body.String())
	}

	// A non-admin (mailbox-bound) key may not change the cap.
	_, userKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "box", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if rr := do(userKey, "PATCH", "/v1/inboxes/"+box.ID, `{"storage_quota_bytes":1024}`); rr.Code != 403 {
		t.Fatalf("non-admin quota patch code %d %s", rr.Code, rr.Body.String())
	}

	// A negative cap is rejected.
	if rr := do(adminKey, "PATCH", "/v1/inboxes/"+box.ID, `{"storage_quota_bytes":-1}`); rr.Code != 400 {
		t.Fatalf("negative quota code %d %s", rr.Code, rr.Body.String())
	}

	// An explicit null clears the cap.
	if rr := do(adminKey, "PATCH", "/v1/inboxes/"+box.ID, `{"storage_quota_bytes":null}`); rr.Code != 200 {
		t.Fatalf("clear quota %d %s", rr.Code, rr.Body.String())
	}
	got, _ = svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID)
	if got.StorageQuotaBytes != nil {
		t.Fatalf("quota after clear = %v, want nil", got.StorageQuotaBytes)
	}
}

// TestDashboardInboxSizeColourRendering proves the dashboard Size cell carries
// the near/over class and a usage tooltip once an inbox cap is set.
func TestDashboardInboxSizeColourRendering(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	// Store 4 MB and cap at 5 MB: 80% is below the 90% amber threshold, so the
	// first render has no warning class.
	seedInbound(t, svc, box, "d1", "<m1@test>", "Subject", strings.Repeat("x", 4<<20))
	quota := int64(5 << 20)
	if err := svc.Store.SetInboxStorageQuota(ctx, u.AccountID, box.ID, &quota); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	render := func() string {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(cookie)
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("dashboard %d: %s", rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	body := render()
	if !strings.Contains(body, "stored (80%)") {
		t.Fatalf("dashboard missing usage tooltip: %s", snippet(body, "size-"))
	}

	// Lower the cap to 4 MB (at usage): the cell turns red.
	quota = 4 << 20
	if err := svc.Store.SetInboxStorageQuota(ctx, u.AccountID, box.ID, &quota); err != nil {
		t.Fatal(err)
	}
	if body := render(); !strings.Contains(body, `class="size-over"`) {
		t.Fatalf("dashboard missing size-over class")
	}
}

// snippet returns a small window around the first occurrence of needle for
// failure messages.
func snippet(s, needle string) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	end := i + 120
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}
