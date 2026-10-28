package httpapp_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/store"
)

func TestRelaySetupUIHistoryAndPendingCodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	create := func(method, name string) string {
		t.Helper()
		form := url.Values{"type": {"openclaw"}, "name": {name}, "inbox": {box.ID}, "setup": {method}, "ack": {"1"}, "_csrf": {csrf}}
		req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", method, rr.Code, rr.Body.String())
		}
		var result map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result["secret"]
	}
	create("manual", "Manual connector")
	command := create("code", "Pending <agent>")
	hermes, err := svc.Store.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes connector"}, "gw-history", "encrypted", "")
	if err != nil {
		t.Fatal(err)
	}
	conns, err := svc.Store.ListRelayConnections(ctx, u.AccountID, store.KindOpenClaw)
	if err != nil || len(conns) != 1 {
		t.Fatalf("manual/code must not create duplicate connectors: %#v %v", conns, err)
	}
	pending, err := svc.Store.ListPendingRelaySetups(ctx, u.AccountID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %#v %v", pending, err)
	}
	check := func(seen bool) string {
		t.Helper()
		body := uiGet(t, h, cookie, "/").Body.String()
		match := regexp.MustCompile(`data-connectors="([^"]*)"`).FindStringSubmatch(body)
		if len(match) != 2 {
			t.Fatal("missing connector metadata")
		}
		var rows []struct {
			Name            string
			LastConnectedAt *time.Time
		}
		if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("connector history: %#v", rows)
		}
		for _, row := range rows {
			if (row.Name != "Manual connector" && row.Name != "Hermes connector") || (row.LastConnectedAt != nil) != seen {
				t.Fatalf("connector history: %#v", rows)
			}
		}
		return body
	}
	body := check(false)
	for _, want := range []string{`id="pending-relay-setups"`, "Pending &lt;agent&gt;", "Pending enrollment", "Cancel setup", "/ui/openclaw/setup/" + pending[0].ID + "/cancel"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	template := regexp.MustCompile(`(?s)<template id="pending-relay-setups">(.*?)</template>`).FindStringSubmatch(body)
	if len(template) != 2 || strings.Contains(template[1], "<form") || !strings.Contains(template[1], `form="cancel-relay-`+pending[0].ID+`"`) {
		t.Fatal("pending button must target an external form, not nest a form in inbox settings")
	}
	code := command[strings.LastIndex(command, "/#")+2:]
	if strings.Contains(body, code) {
		t.Fatal("dashboard leaked enrollment code")
	}
	svc.Store.MarkHermesConnected(ctx, conns[0].ID)
	svc.Store.MarkHermesConnected(ctx, hermes.ID)
	check(true)
	if _, err := svc.Store.EnrollHermesConnection(ctx, code, "gw-ui-code", "encrypted", ""); err != nil {
		t.Fatal(err)
	}
	body = uiGet(t, h, cookie, "/").Body.String()
	if strings.Contains(body, "/ui/openclaw/setup/"+pending[0].ID+"/cancel") {
		t.Fatal("redeemed setup still shown as pending")
	}
	asset := uiGet(t, h, cookie, "/assets/app.js").Body.String()
	for _, want := range []string{"Never connected", "Last connected: ", "c.LastConnectedAt", "Pending setup codes", "source.dataset.inbox !== editInboxID", "Date.parse(source.dataset.expires) <= Date.now()", "Its one-time code will stop working."} {
		if !strings.Contains(asset, want) {
			t.Fatalf("connector rendering missing %q", want)
		}
	}
}

func TestRelaySetupUIDeleteNeverConnected(t *testing.T) {
	t.Parallel()
	for _, kind := range []store.RelayKind{store.KindHermes, store.KindOpenClaw} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			svc, h, u, _, box := httpFixture(t)
			conn, err := svc.Store.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Abandoned", Kind: kind}, "gw-abandoned", "encrypted", "")
			if err != nil {
				t.Fatal(err)
			}
			cookie, csrf := uiSession(t, svc, u.ID)
			form := url.Values{"_csrf": {csrf}, "inbox": {box.ID}, "confirm": {conn.Name}}
			req := httptest.NewRequest("POST", "/ui/"+string(kind)+"/"+conn.ID+"/delete", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(cookie)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusSeeOther {
				t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
			}
			if conns, err := svc.Store.ListRelayConnections(ctx, u.AccountID, kind); err != nil || len(conns) != 0 {
				t.Fatalf("abandoned connector survived deletion: %#v %v", conns, err)
			}
		})
	}
}

func TestRelaySetupUICancellationAuthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, h, u, _, box := httpFixture(t)
	code, err := svc.Store.CreateRelayEnrollToken(ctx, u.AccountID, box.ID, "Pending", store.KindOpenClaw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Store.ListPendingRelaySetups(ctx, u.AccountID)
	if err != nil || len(list) != 1 {
		t.Fatalf("pending: %#v %v", list, err)
	}
	path := "/ui/openclaw/setup/" + list[0].ID + "/cancel"
	ownerCookie, ownerCSRF := uiSession(t, svc, u.ID)
	member := accessMember(t, svc, u.AccountID, u.ID, "relay-member@example.com", box.ID)
	memberCookie, memberCSRF := uiSession(t, svc, member.ID)
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "Other", "relay-other@example.com", "correct horse battery staple", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	otherCookie, otherCSRF := uiSession(t, svc, other.ID)
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		csrf   string
		want   int
	}{
		{"no CSRF", ownerCookie, "", http.StatusForbidden},
		{"non-admin", memberCookie, memberCSRF, http.StatusForbidden},
		{"other account", otherCookie, otherCSRF, http.StatusNotFound},
		{"owner", ownerCookie, ownerCSRF, http.StatusSeeOther},
	} {
		req := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"_csrf": {tc.csrf}, "inbox": {box.ID}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(tc.cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.name, rr.Code, rr.Body.String())
		}
		if tc.name == "owner" && !strings.Contains(rr.Header().Get("Location"), "inbox_tab=connectors") {
			t.Fatal("cancellation did not return to Connectors")
		}
	}
	req := httptest.NewRequest("POST", "/relay/enroll", strings.NewReader(`{"enrollmentToken":"`+code+`","gatewayId":"gw-cancelled"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cancelled code redeemed: %d %s", rr.Code, rr.Body.String())
	}
	if pending, err := svc.Store.ListPendingRelaySetups(ctx, u.AccountID); err != nil || len(pending) != 0 {
		t.Fatalf("cancelled setup still pending: %#v %v", pending, err)
	}
}
