package httpapp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardLoadsGroupedSortableTablesForPrincipal(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, user.ID)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard = %d: %s", response.Code, response.Body.String())
	}

	wantScope := `data-group-scope="` + user.AccountID + `:` + user.ID + `"`
	if !strings.Contains(response.Body.String(), wantScope) {
		t.Fatalf("dashboard is missing principal-scoped group preferences %q", wantScope)
	}

	assetReq := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetReq)
	if assetResponse.Code != http.StatusOK {
		t.Fatalf("dashboard asset = %d: %s", assetResponse.Code, assetResponse.Body.String())
	}
	for _, want := range []string{
		"/ui/dashboard/preferences",
		"data-dashboard-groups-toggle",
		"data-dashboard-connectors-toggle",
		"data-dashboard-group-edit",
		"data-dashboard-group-head",
		"data-dashboard-sort-key",
		"mammothmailCombo",
		"combo-list",
	} {
		if !strings.Contains(assetResponse.Body.String(), want) {
			t.Fatalf("dashboard asset is missing grouping/sorting behavior %q", want)
		}
	}
}

// TestDashboardConnectorsPreference proves the Clients-card "Connectors"
// toggle persists through /ui/dashboard/preferences and that an unknown
// table is rejected. Absent or null means checked, so connectors show by
// default on instances saved before this preference existed.
func TestDashboardConnectorsPreference(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, user.ID)

	post := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/dashboard/preferences", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/ui/dashboard/preferences", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	read := func() map[string]bool {
		rr := get()
		if rr.Code != http.StatusOK {
			t.Fatalf("read preference = %d: %s", rr.Code, rr.Body.String())
		}
		var prefs struct {
			ShowConnectors map[string]bool `json:"showConnectors"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &prefs); err != nil {
			t.Fatal(err)
		}
		return prefs.ShowConnectors
	}

	// A fresh account has no saved connectors preference: null means the
	// client-side default (checked) applies, including on instances saved
	// before this preference existed.
	if fresh := get(); strings.Contains(fresh.Body.String(), `"showConnectors":{`) {
		t.Fatalf("fresh preference should leave showConnectors empty, got %s", fresh.Body.String())
	}

	if rr := post(`{"showConnectors":{"clients":false}}`); rr.Code != http.StatusNoContent {
		t.Fatalf("save preference = %d: %s", rr.Code, rr.Body.String())
	}

	if v := read()["clients"]; v {
		t.Fatalf("unchecked preference did not persist: %#v", read())
	}

	if rr := post(`{"showConnectors":{"clients":true}}`); rr.Code != http.StatusNoContent {
		t.Fatalf("save checked preference = %d: %s", rr.Code, rr.Body.String())
	}
	if v := read()["clients"]; !v {
		t.Fatalf("checked preference did not persist: %#v", read())
	}

	if rr := post(`{"showConnectors":{"inboxes":true}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid table = %d: %s", rr.Code, rr.Body.String())
	}
}
