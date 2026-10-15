package httpapp_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

func TestOneSelectiveImportMailboxAndDisconnect(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	f := onefixture.New(t)
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	rm.InstallRemoteBridges()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	setupID, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Store.OneImportSetup(ctx, u.AccountID, u.ID, setupID)
	if err != nil || strings.Contains(a.EncryptedKey, onefixture.Key) {
		t.Fatal("key not encrypted", err)
	}
	choices, err := rm.DiscoverOne(ctx, p, setupID)
	if err != nil || len(choices) != 3 || choices[0].Email != onefixture.Email || !choices[0].Available || choices[1].Available || choices[2].Available {
		t.Fatalf("discovery %+v %v", choices, err)
	}
	results, err := rm.ImportOne(ctx, p, setupID, []string{onefixture.ConnectionID, "fixture-outlook"}, nil)
	if err != nil || len(results) != 2 || results[0].InboxID == "" || results[1].Error == "" {
		t.Fatalf("selective import %+v %v", results, err)
	}
	inbox := results[0].InboxID
	again, err := rm.ImportOne(ctx, p, setupID, []string{onefixture.ConnectionID}, nil)
	if err != nil || again[0].InboxID != inbox {
		t.Fatal("duplicate import not idempotent", err)
	}
	b, err := svc.Store.OneBinding(ctx, u.AccountID, inbox)
	if err != nil || strings.Contains(b.EncryptedConnectionKey, onefixture.Selector) {
		t.Fatal("selector not encrypted", err)
	}
	// Wait for any scheduled pass by explicitly acquiring the same reconcile gate.
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, inbox); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	catalog := apiGet(t, srv.Handler(), "/v1/inboxes/"+inbox+"/label-catalog", adminKey(t, svc, u))
	if catalog.Code != http.StatusOK || !svc.Store.IsGoogle(ctx, u.AccountID, inbox) {
		t.Fatalf("One/Gmail should share unified label API: %d %s", catalog.Code, catalog.Body.String())
	}
	id, err := svc.Store.GoogleLocalID(ctx, u.AccountID, inbox, "message-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rm.GetRemoteMessage(ctx, model.Principal{AccountID: u.AccountID}, inbox, id); err == nil {
		t.Fatal("unscoped read allowed")
	}
	reader := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{inbox: "read"}}
	if _, err := rm.SetRemoteRead(ctx, reader, inbox, id, true); err == nil {
		t.Fatal("reader mutation allowed")
	}
	if _, err := rm.SetRemoteRead(ctx, p, inbox, id, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.StartGoogleResync(ctx, u.AccountID, inbox, "20"); err != nil {
		t.Fatal(err)
	}
	f.Change(func(f *onefixture.Fixture) {
		f.Messages = []map[string]string{{"id": "message-one"}, {"id": "message-two"}}
		f.NativeErrors = true
		f.Status["GET /v1/passthrough/gmail/v1/users/me/messages/message-two"] = 400
	})
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, inbox); err == nil {
		t.Fatal("failed backfill item ignored")
	}
	backfill, err := svc.Store.GmailSync(ctx, u.AccountID, inbox)
	if err != nil || backfill.BackfillOffset != 1 || backfill.BackfillSnapshot == "" || backfill.BackfillDone {
		t.Fatalf("backfill progress lost: %+v %v", backfill, err)
	}
	f.Change(func(f *onefixture.Fixture) {
		delete(f.Status, "GET /v1/passthrough/gmail/v1/users/me/messages/message-two")
		f.Calls = nil
	})
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, inbox); err != nil {
		t.Fatal(err)
	}
	f.Change(func(f *onefixture.Fixture) {
		for _, call := range f.Calls {
			if call == "GET /v1/passthrough/gmail/v1/users/me/messages/message-one" {
				t.Error("backfill repeated already persisted item")
			}
		}
	})
	// More items than a single paced batch: keep the original cursor and
	// persisted intra-page snapshot until all metadata is applied.
	f.Change(func(f *onefixture.Fixture) {
		var added []any
		for i := 0; i < 12; i++ {
			added = append(added, map[string]any{"message": map[string]string{"id": fmt.Sprintf("history-%02d", i)}})
		}
		f.History = map[string]any{"historyId": "30", "history": []any{map[string]any{"id": "25", "messagesAdded": added}}}
	})
	first, err := rm.ReconcileRemote(ctx, u.AccountID, inbox)
	if err != nil || first.Status != store.RemoteIndexPartial {
		t.Fatalf("history batch: %+v %v", first, err)
	}
	progress, err := svc.Store.GmailSync(ctx, u.AccountID, inbox)
	if err != nil || progress.HistoryID == "30" {
		t.Fatal("cursor advanced before all history items applied", err)
	}
	second, err := rm.ReconcileRemote(ctx, u.AccountID, inbox)
	if err != nil || second.Status != store.RemoteIndexComplete {
		t.Fatalf("history resume: %+v %v", second, err)
	}
	progress, err = svc.Store.GmailSync(ctx, u.AccountID, inbox)
	if err != nil || progress.HistoryID != "30" {
		t.Fatal("history did not finish", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := svc.Store.GoogleLocalID(ctx, u.AccountID, inbox, fmt.Sprintf("history-%02d", i)); err != nil {
			t.Fatal("history item lost", err)
		}
	}
	if _, err := svc.Send(ctx, reader, app.SendInput{InboxID: inbox, To: []string{"recipient@example.test"}, Subject: "Denied", Text: "fixture"}, ""); err == nil {
		t.Fatal("read-only principal sent mail")
	}
	sent, err := svc.Send(ctx, p, app.SendInput{InboxID: inbox, To: []string{"recipient@example.test"}, Subject: "One fixture send", Text: "Synthetic"}, "one-test-send")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx, u.AccountID, sent.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.Change(func(f *onefixture.Fixture) {
		if f.SendCalls != 1 {
			t.Errorf("ordinary outbox bypassed One: sends=%d", f.SendCalls)
		}
	})
	if err := rm.DeleteOneConnection(ctx, p, inbox); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rm.GoogleMailboxClient(ctx, u.AccountID, inbox); err == nil {
		t.Fatal("disconnected mailbox usable")
	}
	if _, err := svc.Store.GoogleLocalID(ctx, u.AccountID, inbox, "message-one"); err != nil {
		t.Fatal("disconnect purged metadata", err)
	}
	if err := rm.SaveOneInboxKey(ctx, p, inbox, onefixture.Key); err != nil {
		t.Fatal(err)
	}
	if err := rm.DeleteOneConnection(ctx, p, inbox); err != nil {
		t.Fatal(err)
	}
	if !svc.Store.IsOne(ctx, u.AccountID, inbox) || !svc.Store.IsGoogle(ctx, u.AccountID, inbox) {
		t.Fatal("removed One binding can fall back to IMAP")
	}
	if _, _, err := rm.GoogleMailboxClient(ctx, u.AccountID, inbox); err == nil {
		t.Fatal("removed credentials usable")
	}
	if _, err := rm.PrepareOneImport(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, onefixture.Key); err == nil {
		t.Fatal("API principal manages human-owned key")
	}
}

func TestOneFourthSelectorSettingsAndCSRF(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	var logs bytes.Buffer
	svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	f := onefixture.New(t)
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	token, csrf, err := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, values url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	root := request("GET", "/", nil)
	if root.Code != 200 || strings.Count(root.Body.String(), `data-standalone-provider="`) != 4 || !strings.Contains(root.Body.String(), `data-standalone-provider="one"`) {
		t.Fatalf("fourth provider missing: %d", root.Code)
	}
	entry := request("GET", "/ui/connections/one", nil)
	if entry.Code != 200 || !strings.Contains(entry.Body.String(), `name="api_key"`) || !strings.Contains(entry.Body.String(), `<div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><button>Next</button></div>`) {
		t.Fatal("wizard key-entry step missing")
	}
	// The One page carries the same left-aligned Back control as the wizard
	// steps, so the user can return to the provider list they came from.
	if !strings.Contains(entry.Body.String(), `data-one-page`) || !strings.Contains(entry.Body.String(), `data-one-back`) {
		t.Fatal("One setup page missing its Back control")
	}
	if strings.Contains(entry.Body.String(), "Imported Gmail mailboxes") || strings.Contains(entry.Body.String(), "No imported Gmail mailboxes.") {
		t.Fatal("empty imported-mailboxes section shown under Save")
	}
	blocked := request("POST", "/ui/connections/one/config", url.Values{"api_key": {onefixture.Key}})
	if blocked.Code != 403 {
		t.Fatalf("CSRF not enforced: %d", blocked.Code)
	}
	saved := request("POST", "/ui/connections/one/config", url.Values{"_csrf": {csrf}, "api_key": {onefixture.Key}})
	if saved.Code != 303 || strings.Contains(saved.Header().Get("Location"), onefixture.Key) {
		t.Fatalf("save: %d %s", saved.Code, saved.Header().Get("Location"))
	}
	page := request("GET", saved.Header().Get("Location"), nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), onefixture.Email) || !strings.Contains(page.Body.String(), "Not supported yet") || strings.Contains(page.Body.String(), onefixture.Key) || strings.Contains(page.Body.String(), onefixture.Selector) {
		t.Fatalf("settings/discovery: %d %s", page.Code, page.Body.String())
	}
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential settings cacheable")
	}
	selection := page.Body.String()
	if strings.Contains(selection, `name="api_key"`) || strings.Contains(selection, ">Next</button>") || strings.Contains(selection, "Back to inboxes") || strings.Contains(selection, "Imported Gmail mailboxes") || !strings.Contains(selection, "Select mailboxes") {
		t.Fatal("selection step still shows key entry or imported-mailboxes section")
	}
	// The mailbox table is a dense, wrapped table so cells align vertically
	// against the tall Index-history select.
	if !strings.Contains(selection, `<table class="dense one-import-table">`) || !strings.Contains(selection, `<th>Index history</th>`) || !strings.Contains(selection, `name="window_`) {
		t.Fatal("mailbox table is not a dense, aligned table with the history window")
	}
	// The Messages column shows the best-effort count and never a bare zero.
	if !strings.Contains(selection, "<th>Messages</th>") {
		t.Fatal("message-count column missing")
	}
	// The key-entry step no longer claims each inbox gets its own key copy.
	if strings.Contains(entry.Body.String(), "encrypted copy of the key and connection") {
		t.Fatal("key-entry step retains the removed encrypted-copy sentence")
	}
	selectionStart := strings.Index(selection, "<h2>Select mailboxes</h2>")
	if selectionStart < 0 {
		t.Fatal("selection heading missing")
	}
	selection = selection[selectionStart:]
	start := strings.Index(selection, `<div class="dialog-actions">`)
	if start < 0 {
		t.Fatal("selection action row missing")
	}
	end := strings.Index(selection[start:], "</div>")
	if end < 0 || !strings.Contains(selection[start:start+end], "Import selected") || !strings.Contains(selection[start:start+end], ">Refresh</a>") || strings.Contains(selection, "Refresh connections</a>") {
		t.Fatal("Refresh is not beside Import selected or has wrong label")
	}
	actions := selection[start : start+end]
	cancelPosition, refreshPosition, importPosition := strings.Index(actions, ">Cancel</a>"), strings.Index(actions, ">Refresh</a>"), strings.Index(actions, "Import selected")
	if cancelPosition < 0 || refreshPosition < cancelPosition || importPosition < refreshPosition || !strings.Contains(actions, `style="margin-right:auto"`) {
		t.Fatal("selection buttons not ordered Cancel, Refresh (left), Import selected (right)")
	}
	if strings.Contains(entry.Body.String(), "Back to inboxes") {
		t.Fatal("key-entry step retains back link")
	}
	for _, label := range []string{"Test stored key", "Test without saving", "Remove One account", "Test &amp; Save"} {
		if strings.Contains(page.Body.String(), label) {
			t.Fatalf("obsolete UX retained: %s", label)
		}
	}
	for _, path := range []string{"/ui/connections/one/test", "/ui/connections/one/remove"} {
		retired := request("POST", path, url.Values{"_csrf": {csrf}})
		if retired.Code != 404 && retired.Code != 405 {
			t.Fatalf("account-wide/test route retained: %s %d", path, retired.Code)
		}
	}
	setupURL, err := url.Parse(saved.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	imported := request("POST", "/ui/connections/one/import", url.Values{"_csrf": {csrf}, "setup_id": {setupURL.Query().Get("setup")}, "connection_id": {onefixture.ConnectionID}})
	if imported.Code != 303 {
		t.Fatalf("import: %d %s", imported.Code, imported.Body.String())
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 1 || records[0]["msg"] != "inbox.created" || records[0]["operation"] != "one_import" || records[0]["source"] != "ui" || records[0]["user_id"] != u.ID {
		t.Fatalf("One lifecycle attribution: %s", logs.String())
	}
	for _, private := range []string{onefixture.Key, onefixture.Selector, token, csrf} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("One credential leaked: %q", private)
		}
	}
	complete := request("GET", imported.Header().Get("Location"), nil)
	if !strings.Contains(entry.Body.String(), `name="api_key" type="text"`) {
		t.Fatal("API key entry remains masked")
	}
	if !strings.Contains(complete.Body.String(), onefixture.Name) || !strings.Contains(complete.Body.String(), onefixture.Email) || !strings.Contains(complete.Body.String(), ">Open inbox</a>") {
		t.Fatal("completion panel missing imported inbox details")
	}
	unselected := request("GET", "/ui/connections/one?imported=1&inbox=unknown-inbox", nil)
	if strings.Contains(unselected.Body.String(), onefixture.Email) {
		t.Fatal("completion lists inboxes outside selected import")
	}
	if complete.Code != 200 || !strings.Contains(complete.Body.String(), "1 selected mailbox imported.") || !strings.Contains(complete.Body.String(), `<div class="dialog-actions"><a class="btn" href="/">Close</a></div>`) {
		t.Fatalf("completion screen: %d %s", complete.Code, complete.Body.String())
	}
	for _, extra := range []string{`name="api_key"`, ">Next</button>", ">Save</button>", "Developers → API Keys", "Imported Gmail mailboxes", "Back to inboxes", "Import selected"} {
		if strings.Contains(complete.Body.String(), extra) {
			t.Fatalf("completion screen retains extra content: %s", extra)
		}
	}
}

// TestOneImportHistoryWindowBoundsBackfill covers the import-screen count and
// the optional per-inbox horizon: a chosen window is stored and applied to the
// progressive backfill as a Gmail "after:" boundary, while the default indexes
// everything.
func TestOneImportHistoryWindowBoundsBackfill(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	f := onefixture.New(t)
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	rm.InstallRemoteBridges()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}

	// A surfaced count is best-effort and must never block import.
	f.Change(func(f *onefixture.Fixture) { f.ProfileTotal = 12345 })
	setupID, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	choices, err := rm.DiscoverOne(ctx, p, setupID)
	if err != nil || len(choices) != 3 {
		t.Fatalf("discovery %+v %v", choices, err)
	}
	if choices[0].MessagesTotal != 12345 {
		t.Fatalf("message total not surfaced: %+v", choices[0])
	}
	// Unavailable connections report no estimate rather than a misleading zero.
	if choices[1].MessagesTotal != 0 || choices[2].MessagesTotal != 0 {
		t.Fatalf("unusable connection reported a count: %+v", choices)
	}

	// Default (unknown window key) imports the full history.
	results, err := rm.ImportOne(ctx, p, setupID, []string{onefixture.ConnectionID}, nil)
	if err != nil || len(results) != 1 || results[0].InboxID == "" {
		t.Fatalf("import %+v %v", results, err)
	}
	inbox := results[0].InboxID
	if after, err := svc.Store.GmailBackfillAfter(ctx, u.AccountID, inbox); err != nil || after != "" {
		t.Fatalf("default horizon should be unbounded: %q %v", after, err)
	}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, inbox); err != nil {
		t.Fatal(err)
	}
	f.Change(func(f *onefixture.Fixture) {
		if f.ListQuery != "" {
			t.Errorf("unbounded backfill sent a query: %q", f.ListQuery)
		}
	})

	// A second mailbox with a bounded window records and applies the horizon.
	f.Change(func(f *onefixture.Fixture) {
		f.Connections = append(f.Connections, map[string]any{"id": "fixture-one-second", "key": "fixture-second-selector", "platform": "gmail", "active": true, "state": "operational", "environment": "live", "email": "second-one@example.test"})
		f.SenderNames["second-one@example.test"] = "Second Owner"
		f.Calls = nil
	})
	setup2, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := rm.ImportOne(ctx, p, setup2, []string{"fixture-one-second"}, map[string]string{"fixture-one-second": "90d"})
	if err != nil || len(bounded) != 1 || bounded[0].InboxID == "" {
		t.Fatalf("bounded import %+v %v", bounded, err)
	}
	second := bounded[0].InboxID
	after, err := svc.Store.GmailBackfillAfter(ctx, u.AccountID, second)
	if err != nil || !strings.HasPrefix(after, "20") {
		t.Fatalf("horizon not stored: %q %v", after, err)
	}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, second); err != nil {
		t.Fatal(err)
	}
	f.Change(func(f *onefixture.Fixture) {
		if !strings.HasPrefix(f.ListQuery, "after:") {
			t.Errorf("bounded backfill missing horizon query: %q", f.ListQuery)
		}
	})

	// The window options are stable and start with the unbounded default.
	options := app.OneHistoryWindowOptions()
	if len(options) == 0 || options[0].Key != "all" || options[0].Days != 0 {
		t.Fatalf("unexpected window options: %+v", options)
	}
}
