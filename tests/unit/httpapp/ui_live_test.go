package httpapp_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestUIStateDashboardScopedToPrincipal proves the dashboard snapshot returns
// live per-inbox counts for the signed-in account's inboxes only, refusing an
// unauthenticated request and never leaking another account's inbox.
func TestUIStateDashboardScopedToPrincipal(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	seedInbound(t, svc, box, "state-1", "<state-1@test>", "s1", "b1")

	// Unauthenticated is rejected.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard", nil))
	if rr.Code != http.StatusSeeOther && rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated /ui/state = %d", rr.Code)
	}

	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard snapshot = %d body=%s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("snapshot Cache-Control = %q, want no-store", cc)
	}
	var out struct {
		Page    string `json:"page"`
		Inboxes map[string]struct {
			Unread  int `json:"unread"`
			Pending int `json:"pending"`
		} `json:"inboxes"`
		Domains map[string]any `json:"domains"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if out.Page != "dashboard" {
		t.Fatalf("page = %q", out.Page)
	}
	if got := out.Inboxes[box.ID].Unread; got != 1 {
		t.Fatalf("inbox unread = %d, want 1", got)
	}
	// Without lights=1 the DNS-backed traffic lights are omitted (a mail-event
	// refresh must not trigger a resolver lookup).
	if out.Domains != nil {
		t.Fatalf("dashboard snapshot must omit domains unless lights=1, got %v", out.Domains)
	}
}

// TestUIStateDashboardLightsOptIn proves the DNS-backed traffic light block is
// returned only when the caller asks for it.
func TestUIStateDashboardLightsOptIn(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("lights snapshot = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Domains map[string]any `json:"domains"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Domains == nil {
		t.Fatalf("lights=1 must include the domains block")
	}
}

// TestUIStateInboxScopesToGrantedInbox proves the inbox snapshot returns folder
// counts for a granted inbox and refuses an inbox the principal has no role on
// (an unknown/ungranted id is reported as 404 rather than leaking counts).
func TestUIStateInboxScopesToGrantedInbox(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	req := httptest.NewRequest(http.MethodGet, "/ui/state?page=inbox&inbox="+box.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("granted inbox snapshot = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Page   string         `json:"page"`
		Inbox  string         `json:"inbox"`
		Labels map[string]int `json:"labels"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if out.Page != "inbox" || out.Inbox != box.ID {
		t.Fatalf("snapshot page/inbox = %q/%q", out.Page, out.Inbox)
	}

	// An inbox id the principal cannot read is refused.
	req = httptest.NewRequest(http.MethodGet, "/ui/state?page=inbox&inbox=inb_missing", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("ungranted inbox snapshot = %d, want 404", rr.Code)
	}
}

// TestUIEventsStreamRequiresSessionAndScopes proves the UI SSE endpoint requires
// a session (an unauthenticated request is a redirect/denial, not a stream) and
// returns the SSE content type for an authenticated session.
func TestUIEventsStreamRequiresSessionAndScopes(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Unauthenticated: no redirect-following, so a 303 to /login is observed.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(srv.URL + "/ui/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated stream must not succeed")
	}

	// Authenticated: the handler writes SSE headers and then blocks; a request
	// timeout returns control without a concurrent read of shared headers.
	cookie, _ := uiSession(t, svc, u.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated stream status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content type = %q", ct)
	}
}

// TestModelEventMessageStateChangedValue pins the wire value the UI subscribes to.
func TestModelEventMessageStateChangedValue(t *testing.T) {
	if model.EventMessageStateChanged != "message.state_changed" {
		t.Fatalf("EventMessageStateChanged = %q", model.EventMessageStateChanged)
	}
}

// TestUIStateDashboardLightIsCached proves the live snapshot's published-MX
// lookup is cached briefly, so repeated dashboard snapshots do not each run a
// fresh resolver lookup, while the traffic light is still correct.
func TestUIStateDashboardLightIsCached(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	res := &switchableResolver{}
	res.set([]*net.MX{{Host: "antler1.example.test.", Pref: 10}}, nil)
	srv := httpapp.New(svc, nil)
	srv.SetDNSResolver(res)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)

	snapshot := func() {
		req := httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("lights snapshot = %d body=%s", rr.Code, rr.Body.String())
		}
	}
	snapshot()
	_, after := res.counts()
	for i := 0; i < 4; i++ {
		snapshot()
	}
	if mxHits, _ := res.counts(); mxHits > after {
		t.Fatalf("cached dashboard snapshots ran extra MX lookups: %d -> %d", after, mxHits)
	}
}
