package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestAPIAccountSettingsTimezone covers the timezone field on the account
// settings route: default empty, set, invalid rejected, cleared.
func TestAPIAccountSettingsTimezone(t *testing.T) {
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

	// Default is empty and returned on GET.
	if rr := do("GET", ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"timezone":""`) {
		t.Fatalf("get default %d %s", rr.Code, rr.Body.String())
	}
	// Set a zone.
	if rr := do("PATCH", `{"timezone":"Europe/London"}`); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"timezone":"Europe/London"`) {
		t.Fatalf("patch tz %d %s", rr.Code, rr.Body.String())
	}
	if rr := do("GET", ""); !strings.Contains(rr.Body.String(), `"timezone":"Europe/London"`) {
		t.Fatalf("tz not persisted: %s", rr.Body.String())
	}
	// An unknown zone is a 400 and does not change the stored value.
	if rr := do("PATCH", `{"timezone":"Not/AZone"}`); rr.Code != 400 {
		t.Fatalf("invalid tz accepted: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do("GET", ""); !strings.Contains(rr.Body.String(), `"timezone":"Europe/London"`) {
		t.Fatalf("tz changed on invalid set: %s", rr.Body.String())
	}
	// Clear to UTC.
	if rr := do("PATCH", `{"timezone":""}`); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"timezone":""`) {
		t.Fatalf("clear tz %d %s", rr.Code, rr.Body.String())
	}
	// Trash retention and timezone can be updated together.
	rr := do("PATCH", `{"trash_retention_days":5,"timezone":"Asia/Tokyo"}`)
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || out["timezone"] != "Asia/Tokyo" || out["trash_retention_days"] != float64(5) {
		t.Fatalf("combined patch %d %s", rr.Code, rr.Body.String())
	}
}

// TestUISettingsTimezone covers the account-page form flow: setting the account
// default and the per-user override, and that both render their values back.
func TestUISettingsTimezone(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	post := func(path, form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := post("/ui/account/timezone", "_csrf="+csrf+"&timezone=Europe%2FLondon"); rr.Code != 303 {
		t.Fatalf("set account tz %d %s", rr.Code, rr.Body.String())
	}
	if rr := post("/ui/account/timezone/me", "_csrf="+csrf+"&timezone=Asia%2FTokyo"); rr.Code != 303 {
		t.Fatalf("set user tz %d %s", rr.Code, rr.Body.String())
	}
	// The account page renders the stored values back into the inputs.
	req := httptest.NewRequest("GET", "/account", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `value="Europe/London"`) {
		t.Fatalf("account tz not rendered: %s", body)
	}
	if !strings.Contains(body, `value="Asia/Tokyo"`) {
		t.Fatalf("user tz not rendered: %s", body)
	}
	// Invalid zone is rejected with a flash, not applied.
	if rr := post("/ui/account/timezone", "_csrf="+csrf+"&timezone=Not%2FAZone"); rr.Code != 303 {
		t.Fatalf("invalid account tz status %d", rr.Code)
	}
	if tz, _ := svc.Store.GetAccountTimezone(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}); tz != "Europe/London" {
		t.Fatalf("invalid set changed account tz: %q", tz)
	}
}

// TestUIRendersTimestampsInAccountZone proves the display conversion: a message
// timestamp is rendered in the configured zone, not UTC.
func TestUIRendersTimestampsInAccountZone(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := seedInbound(t, svc, box, "tz-render-1", "<tz-render-1@test>", "Zone me", "body")

	admin := model.Principal{AccountID: u.AccountID, Admin: true, UserID: u.ID}
	if err := svc.Store.SetAccountTimezone(ctx, admin, "Asia/Tokyo"); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	want := m.CreatedAt.In(loc).Format("15:04 2-Jan-06")
	if want == m.CreatedAt.Format("15:04 2-Jan-06") {
		// Guard against a test that would pass even without conversion.
		t.Skipf("UTC and Asia/Tokyo render identically at %s", m.CreatedAt)
	}

	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), want) {
		t.Fatalf("timestamp not rendered in account zone: want %q", want)
	}
}
