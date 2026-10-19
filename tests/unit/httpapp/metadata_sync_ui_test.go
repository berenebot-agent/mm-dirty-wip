package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestActionQueueExposesMetadataSyncs proves GET /ui/actions carries the live
// metadata-sync status as an additive `syncs` array: one row per non-settled
// standalone inbox, labelled with its provider and safe state, and no row for a
// domain inbox. The existing items/csrf payload must be untouched.
func TestActionQueueExposesMetadataSyncs(t *testing.T) {
	t.Parallel()
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	if err := svc.Store.SaveMetadataSyncState(context.Background(), u.AccountID, standalone.ID, "waiting", "initial", "Waiting to retry provider request"); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, cookie, "/ui/actions")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/actions = %d: %s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(rr.Body.String(), `"syncs":[`) {
		t.Fatalf("syncs is not a JSON array: %s", rr.Body.String())
	}
	var out struct {
		Items []json.RawMessage `json:"items"`
		Syncs []struct {
			InboxID  string `json:"inbox_id"`
			Name     string `json:"name"`
			Address  string `json:"address"`
			Provider string `json:"provider"`
			State    string `json:"state"`
			Phase    string `json:"phase"`
			Indexed  int64  `json:"indexed"`
			Total    int64  `json:"total"`
			Error    string `json:"error"`
		} `json:"syncs"`
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Items == nil || out.CSRF == "" {
		t.Fatalf("queue payload lost items/csrf: items=%v csrf=%q", out.Items, out.CSRF)
	}
	if len(out.Syncs) != 1 {
		t.Fatalf("syncs = %+v, want exactly one row for the standalone inbox", out.Syncs)
	}
	row := out.Syncs[0]
	if row.InboxID != standalone.ID {
		t.Fatalf("sync inbox_id = %q, want %q", row.InboxID, standalone.ID)
	}
	if row.InboxID == domainBox.ID {
		t.Fatal("a domain inbox must not appear in syncs")
	}
	if row.Provider != "IMAP" || row.State != "waiting" || row.Address != standalone.Address {
		t.Fatalf("sync row = %+v", row)
	}
	if row.Name == "" {
		t.Fatalf("sync row must be labelled: %+v", row)
	}
	if row.Error == "" {
		t.Fatalf("a waiting sync must carry its safe retry message: %+v", row)
	}
}

// TestActionQueueSyncsEmptyArrayWithoutRemoteInboxes proves the field is a
// stable empty array (not null/absent) when the account has no remote inbox to
// report, so the client can iterate it unconditionally.
func TestActionQueueSyncsEmptyArrayWithoutRemoteInboxes(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, cookie, "/ui/actions")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/actions = %d", rr.Code)
	}
	var out struct {
		Syncs []json.RawMessage `json:"syncs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Syncs == nil {
		t.Fatalf("syncs must be [] not null: %s", rr.Body.String())
	}
	if len(out.Syncs) != 0 {
		t.Fatalf("syncs = %+v, want none without remote inboxes", out.Syncs)
	}
}

// TestInboxSyncBannerRenderedForStandaloneViews proves the top-of-view metadata
// sync banner is rendered on a standalone (remote) inbox's mailbox and folder
// views, and is absent from a domain inbox view where there is no remote
// metadata to sync.
func TestInboxSyncBannerRenderedForStandaloneViews(t *testing.T) {
	t.Parallel()
	svc, h, u, domainBox, standalone, fake := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	for _, path := range []string{
		"/ui/inboxes/" + standalone.ID,
		"/ui/inboxes/" + standalone.ID + "/trash",
	} {
		body := uiGet(t, h, cookie, path).Body.String()
		if !strings.Contains(body, "data-sync-banner") {
			t.Fatalf("%s: standalone view missing the sync banner", path)
		}
	}
	if body := uiGet(t, h, cookie, "/ui/inboxes/"+domainBox.ID).Body.String(); strings.Contains(body, "data-sync-banner") {
		t.Fatal("domain inbox view must not render the remote sync banner")
	}

	// The custom-folder view is a separate body template, so prove the banner is
	// there too once the standalone inbox has reconciled a remote folder.
	fake.folders["Archive"] = 100
	fake.add("Archive", "From: a@b.test\r\nSubject: Archived\r\nMessage-ID: <sync-banner-arc@remote>\r\n\r\nbody", "<sync-banner-arc@remote>", "Archived")
	key := adminKey(t, svc, u)
	refreshRemote(t, h, standalone.ID, key)
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/folders", key)
	var env struct {
		Items []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode folders: %v", err)
	}
	var archiveID string
	for _, it := range env.Items {
		if it.Path == "Archive" {
			archiveID = it.ID
		}
	}
	if archiveID == "" {
		t.Fatalf("Archive folder not in tree: %s", rr.Body.String())
	}
	body := uiGet(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/folder?folder="+archiveID).Body.String()
	if !strings.Contains(body, "data-sync-banner") {
		t.Fatal("standalone folder view missing the sync banner")
	}
}

// TestActionQueueScriptWiresSyncStatus pins the client wiring: app.js must find
// the banner, render sync rows that carry no action controls, and gate the
// count/percentage on a known total. Routine refreshes stay background-only:
// the banner, rows and counts read the initial-sync filter while the header
// spinner still reads every reported status.
func TestActionQueueScriptWiresSyncStatus(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d", rr.Code)
	}
	script := rr.Body.String()
	for _, want := range []string{
		"data-sync-banner",
		"aq-sync",
		"SYNC_STATE_TEXT",
		"syncProgress",
		"' indexed'",
		"metadata-sync-spin",
		"Inbox-wide",
		"prefers-reduced-motion",
		// Downloading wording and the estimate marker for Gmail/One totals.
		"Downloading metadata",
		"estimated_total",
		"syncDenominator",
		// Initial-sync-only banner/queue wiring: visible rows and counts use
		// the filtered list while the spinner reads every status.
		"shouldDisplaySync",
		"displaySyncs",
		"displaySyncs.forEach",
		"displaySyncs.slice",
		// The reconnect verdict rides the same poll: its own banner carries the
		// control, so the strip must not repeat the line and a verdict recorded
		// while the page is open must be actionable without a reload.
		"connections",
		"mailmooseReconnectBanner",
		"syncReconnectBanner",
		"data-inbox-reconnect",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("app.js missing sync wiring %q", want)
		}
	}
	// The old wait-for-slot phrasing and the "Indexing…" chip are gone.
	for _, gone := range []string{"Waiting for provider request slot", "'Indexing…'"} {
		if strings.Contains(script, gone) {
			t.Fatalf("app.js still contains %q", gone)
		}
	}
}
