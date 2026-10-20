package httpapp_test

import (
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
		"data-dashboard-group-edit",
		"data-dashboard-group-head",
		"data-dashboard-sort-key",
		"mailmooseCombo",
		"combo-list",
	} {
		if !strings.Contains(assetResponse.Body.String(), want) {
			t.Fatalf("dashboard asset is missing grouping/sorting behavior %q", want)
		}
	}
}
