package httpapp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestInboxEditSavesDashboardGroup proves the Identity tab's Dashboard group is
// saved through the inbox edit form and lands in the signed-in user's dashboard
// preferences, and that an empty value clears it. The group is a per-user
// display preference, not inbox policy, so it rides the main save but writes to
// the user's dashboard preferences.
func TestInboxEditSavesDashboardGroup(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	save := func(group string) *httptest.ResponseRecorder {
		body := url.Values{"_csrf": {csrf}, "display": {"Agent"}, "dashboard_group": {group}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/ui/inboxes/"+standalone.ID+"/edit", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	readGroups := func() map[string]string {
		req := httptest.NewRequest(http.MethodGet, "/ui/dashboard/preferences", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("prefs = %d: %s", rr.Code, rr.Body.String())
		}
		var prefs struct {
			Groups map[string]map[string]string `json:"groups"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &prefs); err != nil {
			t.Fatal(err)
		}
		return prefs.Groups["inboxes"]
	}

	if rr := save("Agents"); rr.Code != http.StatusSeeOther {
		t.Fatalf("save group = %d: %s", rr.Code, rr.Body.String())
	}
	if got := readGroups()[standalone.ID]; got != "Agents" {
		t.Fatalf("group = %q, want Agents", got)
	}

	if rr := save(""); rr.Code != http.StatusSeeOther {
		t.Fatalf("clear group = %d: %s", rr.Code, rr.Body.String())
	}
	if _, ok := readGroups()[standalone.ID]; ok {
		t.Fatalf("group not cleared: %#v", readGroups())
	}
}
