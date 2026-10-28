package app_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

// syncLogSink captures the rendered "remote sync" completion records so a test
// can assert the provider, trigger, duration, new-count and status stamped on a
// pass.
type syncLogSink struct {
	mu    sync.Mutex
	msgs  []string
	attrs []map[string]string
}

func newSyncLogSink() *syncLogSink { return &syncLogSink{} }

func (h *syncLogSink) Enabled(context.Context, slog.Level) bool { return true }
func (h *syncLogSink) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.attrs = append(h.attrs, attrs)
	return nil
}
func (h *syncLogSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *syncLogSink) WithGroup(string) slog.Handler      { return h }

// syncRecords returns the attribute maps of every "remote sync" line in arrival
// order.
func (h *syncLogSink) syncRecords() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for i, m := range h.msgs {
		if m == "remote sync" {
			out = append(out, h.attrs[i])
		}
	}
	return out
}

func (h *syncLogSink) records(message string) []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for i, m := range h.msgs {
		if m == message {
			out = append(out, h.attrs[i])
		}
	}
	return out
}

// TestIMAPSyncIsLogged proves a manual IMAP detection pass emits one "remote
// sync" INFO line naming the provider, inbox, trigger, a duration, the number of
// newly-detected messages and the completion status. A subsequent pass that
// finds nothing still logs (the heartbeat), with new=0.
func TestIMAPSyncIsLogged(t *testing.T) {
	t.Parallel()
	svc, box, rm, fake, w := workerEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)
	w = app.NewRemoteWorker(svc, rm, slog.New(sink))
	w.SetDemandInterval(-1)

	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Baseline the inbox (no arrivals, but still a logged completion).
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: old\r\nMessage-ID: <old@remote>\r\n\r\nbody", "<old@remote>", "old")
	w.DetectInbox(ctx, inbox)
	if recs := sink.syncRecords(); len(recs) != 1 {
		t.Fatalf("baseline logged %d remote sync lines, want 1", len(recs))
	}

	// A manual pass with one genuinely new message.
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: fresh\r\nMessage-ID: <fresh@remote>\r\n\r\nbody", "<fresh@remote>", "fresh")
	w.DetectInbox(app.WithManualRemoteSync(ctx), inbox)
	recs := sink.syncRecords()
	if len(recs) != 2 {
		t.Fatalf("logged %d remote sync lines, want 2", len(recs))
	}
	last := recs[len(recs)-1]
	if last["provider"] != "imap" {
		t.Fatalf("provider = %q, want imap", last["provider"])
	}
	if last["inbox"] != box.Address {
		t.Fatalf("inbox = %q, want %q", last["inbox"], box.Address)
	}
	if last["trigger"] != "manual" {
		t.Fatalf("trigger = %q, want manual", last["trigger"])
	}
	if last["new"] != "1" {
		t.Fatalf("new = %q, want 1", last["new"])
	}
	if last["status"] != "complete" {
		t.Fatalf("status = %q, want complete", last["status"])
	}
	received := sink.records("remote received")
	if len(received) != 1 {
		t.Fatalf("logged %d remote received lines, want 1", len(received))
	}
	if received[0]["provider"] != "imap" || received[0]["source"] != "imap" || received[0]["from"] != "sender@elsewhere.test" || received[0]["to"] != box.Address {
		t.Fatalf("remote received attrs = %#v", received[0])
	}
	if strings.TrimSpace(last["duration"]) == "" {
		t.Fatalf("duration missing")
	}
	if _, ok := last["error"]; ok {
		t.Fatalf("unexpected error attr: %v", last["error"])
	}

	// A quiet pass still logs, with new=0 and trigger=auto.
	w.DetectInbox(ctx, inbox)
	final := sink.syncRecords()
	quiet := final[len(final)-1]
	if quiet["new"] != "0" || quiet["trigger"] != "auto" {
		t.Fatalf("quiet pass = %#v, want new=0 trigger=auto", quiet)
	}
}

// TestMicrosoftDeltaSyncIsLogged proves a Microsoft delta reconcile emits one
// "remote sync" line with provider=microsoft and the newly-detected arrival
// count, and that a repeat pass with no new mail logs new=0.
func TestMicrosoftDeltaSyncIsLogged(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := microsoftLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	recs := sink.syncRecords()
	if len(recs) != 1 {
		t.Fatalf("logged %d remote sync lines, want 1", len(recs))
	}
	if recs[0]["provider"] != "microsoft" {
		t.Fatalf("provider = %q, want microsoft", recs[0]["provider"])
	}
	if recs[0]["new"] != "0" {
		t.Fatalf("baseline new = %q, want 0", recs[0]["new"])
	}

	// A new message on the provider appears on the next pass as one arrival.
	fixture.add("FreshID", "inbox-id", "Fresh")
	if _, err := rm.ReconcileRemote(app.WithManualRemoteSync(ctx), box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	recs = sink.syncRecords()
	if len(recs) != 2 {
		t.Fatalf("logged %d remote sync lines, want 2", len(recs))
	}
	last := recs[len(recs)-1]
	if last["new"] != "1" || last["trigger"] != "manual" || last["provider"] != "microsoft" {
		t.Fatalf("arrival pass = %#v, want new=1 trigger=manual provider=microsoft", last)
	}
	if last["status"] != "complete" {
		t.Fatalf("status = %q, want complete", last["status"])
	}
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundPoll := false
	for _, arrival := range arrivals {
		if arrival.FolderPath == "microsoft:FreshID" {
			foundPoll = arrival.Source == "microsoft"
		}
	}
	if !foundPoll {
		t.Fatalf("poll-sourced arrival missing: %#v", arrivals)
	}

	// A quiet repeat logs new=0.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	final := sink.syncRecords()
	if q := final[len(final)-1]; q["new"] != "0" {
		t.Fatalf("quiet pass new = %q, want 0", q["new"])
	}

	fixture.add("PushID", "inbox-id", "Push")
	if _, err := rm.ReconcileRemote(app.WithPushRemoteSync(ctx), box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	final = sink.syncRecords()
	if got := final[len(final)-1]["trigger"]; got != "push" {
		t.Fatalf("push sync trigger = %q, want push", got)
	}
	arrivals, err = svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundPush := false
	for _, arrival := range arrivals {
		if arrival.FolderPath == "microsoft:PushID" {
			foundPush = arrival.Source == "microsoft push"
		}
	}
	if !foundPush {
		t.Fatalf("push-sourced arrival missing: %#v", arrivals)
	}
}

// TestMicrosoftQuickDetectionLogsAndCheap proves the demand/watcher Microsoft
// detection path runs the cheap Inbox-only delta: it logs one provider=microsoft
// line, detects a new Inbox arrival, and does NOT re-enumerate the folder tree
// (the full reconcile owns that).
func TestMicrosoftQuickDetectionLogsAndCheap(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := microsoftLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)
	w := app.NewRemoteWorker(svc, rm, nil)
	w.SetDemandInterval(-1)
	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Full reconcile first, so the Inbox folder row and its delta baseline exist.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	enumerationsAfterFull := fixture.folderEnumerations
	fixture.mu.Unlock()
	if enumerationsAfterFull == 0 {
		t.Fatal("full reconcile did not enumerate folders")
	}

	fixture.add("QuickID", "inbox-id", "Quick")
	w.DetectInbox(ctx, inbox)

	recs := sink.syncRecords()
	if len(recs) != 2 {
		t.Fatalf("logged %d remote sync lines, want 2 (full + quick)", len(recs))
	}
	last := recs[len(recs)-1]
	if last["provider"] != "microsoft" || last["new"] != "1" || last["status"] != "complete" {
		t.Fatalf("quick pass = %#v, want provider=microsoft new=1 complete", last)
	}
	fixture.mu.Lock()
	enumerationsAfterQuick := fixture.folderEnumerations
	fixture.mu.Unlock()
	if enumerationsAfterQuick != enumerationsAfterFull {
		t.Fatalf("quick detection re-enumerated folders (%d -> %d); it must use the cache", enumerationsAfterFull, enumerationsAfterQuick)
	}
}

// TestGmailSyncIsLogged proves the Gmail history detection pass emits a "remote
// sync" line with provider=gmail, and reports the added-message count.
func TestGmailSyncIsLogged(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := googleLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	w := app.NewRemoteWorker(svc, rm, slog.New(sink))
	w.SetMaxInboxConnections(1)
	w.SetDemandInterval(-1)
	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// First pass establishes the history baseline.
	w.DetectInbox(ctx, inbox)
	recs := sink.syncRecords()
	if len(recs) != 1 {
		t.Fatalf("baseline logged %d remote sync lines, want 1", len(recs))
	}
	if recs[0]["provider"] != "gmail" {
		t.Fatalf("provider = %q, want gmail", recs[0]["provider"])
	}
	if recs[0]["status"] != "complete" {
		t.Fatalf("status = %q, want complete", recs[0]["status"])
	}

	// A second pass with an added message logs new=1.
	fixture.add("msg-1")
	w.DetectInbox(ctx, inbox)
	final := sink.syncRecords()
	if len(final) != 2 {
		t.Fatalf("logged %d remote sync lines, want 2", len(final))
	}
	if last := final[len(final)-1]; last["new"] != "1" || last["provider"] != "gmail" {
		t.Fatalf("arrival pass = %#v, want new=1 provider=gmail", last)
	}
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 1 || arrivals[0].Source != "gmail" {
		t.Fatalf("Gmail arrival source = %#v, err=%v", arrivals, err)
	}
}

func TestConcurrentGmailDetectionRunsOnePass(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := googleLogEnv(t)
	w := app.NewRemoteWorker(svc, rm, nil)
	w.SetDemandInterval(-1)
	inbox, err := svc.Store.GetInboxInternal(context.Background(), box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}

	profileStarted := make(chan struct{}, 1)
	releaseProfile := make(chan struct{})
	fixture.profileStarted = profileStarted
	fixture.releaseProfile = releaseProfile

	firstDone := make(chan struct{})
	go func() {
		w.DetectInbox(context.Background(), inbox)
		close(firstDone)
	}()
	select {
	case <-profileStarted:
	case <-time.After(5 * time.Second):
		close(releaseProfile)
		t.Fatal("first Gmail detection did not reach profile request")
	}

	secondDone := make(chan struct{})
	go func() {
		w.DetectInbox(context.Background(), inbox)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		close(releaseProfile)
		<-firstDone
		t.Fatal("overlapping Gmail detection did not return promptly")
	}
	close(releaseProfile)
	<-firstDone

	if got := fixture.profileRequests.Load(); got != 1 {
		t.Fatalf("profile requests = %d, want 1 concurrent Gmail detection pass", got)
	}
}

// TestGmailDetectionToleratesVanishedMessage proves a history entry whose
// messages.get returns 404 (the message vanished between the history snapshot
// and the fetch) does not fail the detection pass: the pass completes, the stale
// cached row is dropped, and the detection cursor still advances.
func TestGmailDetectionToleratesVanishedMessage(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := googleLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	w := app.NewRemoteWorker(svc, rm, slog.New(sink))
	w.SetMaxInboxConnections(1)
	w.SetDemandInterval(-1)
	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Baseline the history cursor.
	w.DetectInbox(ctx, inbox)

	// Index a message first, so a stale cached row exists when the next pass
	// discovers the message is gone.
	fixture.add("msg-gone")
	w.DetectInbox(ctx, inbox)
	local, err := svc.Store.GoogleLocalID(ctx, box.AccountID, box.ID, "msg-gone")
	if err != nil {
		t.Fatalf("expected the cached row for msg-gone: %v", err)
	}

	// The next pass names msg-gone in history, but messages.get now 404s.
	fixture.vanish("msg-gone")
	w.DetectInbox(ctx, inbox)

	final := sink.syncRecords()
	last := final[len(final)-1]
	if last["status"] != "complete" {
		t.Fatalf("pass after a vanished message = %#v, want status=complete", last)
	}
	if _, ok := last["error"]; ok {
		t.Fatalf("pass after a vanished message logged an error: %#v", last)
	}
	// The stale cached row is dropped.
	if _, err := svc.Store.GetRemoteMessage(ctx, box.AccountID, box.ID, local); err == nil {
		t.Fatal("stale cached row for the vanished message was not removed")
	}
	// The detection cursor still advances, so the same window is not re-walked.
	w.DetectInbox(ctx, inbox)
	again := sink.syncRecords()
	if lastAgain := again[len(again)-1]; lastAgain["status"] != "complete" || lastAgain["new"] != "0" {
		t.Fatalf("re-pass = %#v, want status=complete new=0", lastAgain)
	}
}

// TestGmailReconcileToleratesVanishedMessage proves the deep index reconcile, not
// just cheap detection, survives a history entry whose messages.get returns 404.
// Gmail drops a deleted message's history entry asynchronously, so a change window
// can still name a message that is already gone; treating that as a failed pass
// left the index stuck on the same doomed window and errored a "remote sync" line
// on every pass.
func TestGmailReconcileToleratesVanishedMessage(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := googleLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)
	var passes atomic.Int32
	fixture.page = func() string {
		if passes.Add(1) == 1 {
			return "msg-gone"
		}
		return "msg-fine"
	}

	// Seed a cached row for the message that is about to vanish.
	fixture.add("msg-gone")
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	local, err := svc.Store.GoogleLocalID(ctx, box.AccountID, box.ID, "msg-gone")
	if err != nil {
		t.Fatalf("expected the cached row for msg-gone: %v", err)
	}

	// The next pass names msg-gone, but messages.get now 404s.
	fixture.add("msg-fine")
	fixture.vanish("msg-gone")
	status, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatalf("reconcile after a vanished message failed: %v", err)
	}
	if status.Status != store.RemoteIndexComplete {
		t.Fatalf("index status = %q, want complete", status.Status)
	}
	last := sink.syncRecords()[1]
	_, loggedError := last["error"]
	if last["status"] != "complete" || loggedError {
		t.Fatalf("reconcile after a vanished message = %#v, want status=complete with no error", last)
	}
	// The stale cached row is dropped and the rest of the window is still indexed.
	if _, err := svc.Store.GetRemoteMessage(ctx, box.AccountID, box.ID, local); err == nil {
		t.Fatal("stale cached row for the vanished message was not removed")
	}
	if _, err := svc.Store.GoogleLocalID(ctx, box.AccountID, box.ID, "msg-fine"); err != nil {
		t.Fatalf("live message in the same window was not indexed: %v", err)
	}
}

// ---- fixtures ----

// microsoftLogEnv builds a service with a Microsoft-connected standalone inbox
// backed by a mutable httptest Graph fixture.
func microsoftLogEnv(t *testing.T) (*app.Service, model.Inbox, *app.RemoteMailboxService, *microsoftLogFixture) {
	t.Helper()
	svc, _, box, rm := remoteTestEnv(t)
	fixture := &microsoftLogFixture{}
	srv := httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(srv.Close)
	fixture.url = srv.URL
	rm.Microsoft = microsoft.NewClient(srv.Client())
	rm.Microsoft.BaseURL = srv.URL
	c := store.MicrosoftConnection{AccountID: box.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: "agent@remote.example"}
	if err := rm.SaveMicrosoftGrant(context.Background(), c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)}); err != nil {
		t.Fatal(err)
	}
	return svc, box, rm, fixture
}

type microsoftLogFixture struct {
	mu       sync.Mutex
	url      string
	messages []map[string]any
	// folderEnumerations counts /mailFolders list calls, so a test can prove the
	// quick detection path does not re-enumerate the folder tree.
	folderEnumerations int
	// retired holds message ids the provider no longer accepts (its id was
	// replaced after a move), so a request naming one answers not found.
	retired map[string]bool
	// moves counts accepted and rejected /move calls, so a test can prove the
	// retired id was tried before the live one.
	moves int
}

// add appends a message to the inbox that the next delta pass will report.
func (f *microsoftLogFixture) add(id, folder, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, map[string]any{"id": id, "conversationId": "c-" + id, "parentFolderId": folder, "subject": subject, "receivedDateTime": "2026-10-11T10:00:00Z", "internetMessageId": "<" + id + "@graph>"})
}

// retire makes the provider reject one message id, as Microsoft does once it has
// re-issued the message under another id form.
func (f *microsoftLogFixture) retire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retired == nil {
		f.retired = map[string]bool{}
	}
	f.retired[id] = true
}

// find returns the stored message with this provider id, or nil.
func (f *microsoftLogFixture) find(id string) map[string]any {
	for _, m := range f.messages {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

func (f *microsoftLogFixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages/delta"):
		var value []any
		for _, m := range f.messages {
			value = append(value, m)
		}
		json.NewEncoder(w).Encode(map[string]any{"value": value, "@odata.deltaLink": f.url + r.URL.Path + "?cursor=1"})
	case strings.HasSuffix(r.URL.Path, "/move"):
		f.moves++
		var body struct {
			DestinationID string `json:"destinationId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := path.Base(strings.TrimSuffix(r.URL.Path, "/move"))
		if f.retired[id] || f.find(id) == nil {
			w.WriteHeader(404)
			return
		}
		moved := map[string]any{}
		for k, v := range f.find(id) {
			moved[k] = v
		}
		moved["parentFolderId"] = body.DestinationID
		json.NewEncoder(w).Encode(moved)
	case r.URL.Query().Get("$filter") != "":
		var value []any
		for _, m := range f.messages {
			if rfc, _ := m["internetMessageId"].(string); strings.Contains(r.URL.Query().Get("$filter"), "'"+rfc+"'") {
				value = append(value, m)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"value": value})
	case strings.HasSuffix(r.URL.Path, "/mailFolders"):
		f.folderEnumerations++
		json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "inbox-id", "displayName": "Inbox", "totalItemCount": len(f.messages)}}})
	case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
		json.NewEncoder(w).Encode(map[string]any{"id": "inbox-id", "displayName": "Inbox"})
	default:
		w.WriteHeader(404)
	}
}

// googleLogEnv builds a service with a Google-connected standalone inbox backed
// by a mutable httptest Gmail fixture.
func googleLogEnv(t *testing.T) (*app.Service, model.Inbox, *app.RemoteMailboxService, *googleLogFixture) {
	t.Helper()
	svc, _, box, rm := remoteTestEnv(t)
	fixture := &googleLogFixture{}
	srv := httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(srv.Close)
	fixture.url = srv.URL
	rm.Google = gmail.NewClient(srv.Client())
	rm.Google.BaseURL = srv.URL
	if err := rm.SaveGoogleGrant(context.Background(), box.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	return svc, box, rm, fixture
}

type googleLogFixture struct {
	mu              sync.Mutex
	url             string
	messages        []string
	profileStarted  chan struct{}
	releaseProfile  chan struct{}
	profileRequests atomic.Int32
	// gone holds message ids that history still names but whose fetch must 404,
	// simulating a message deleted between the history snapshot and the get.
	gone map[string]bool
	// page, when set, names the one id reported as added in the change history,
	// so a test can hand the reconcile a different window on each pass.
	page func() string
}

func (f *googleLogFixture) add(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, id)
}

// vanish makes every messages.get for id return a 404 while history still names
// it, so a detection pass must tolerate the vanished message.
func (f *googleLogFixture) vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone == nil {
		f.gone = map[string]bool{}
	}
	f.gone[id] = true
}

func (f *googleLogFixture) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/profile") {
		f.profileRequests.Add(1)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "/profile"):
		if f.profileStarted != nil {
			select {
			case f.profileStarted <- struct{}{}:
			default:
			}
			<-f.releaseProfile
		}
		json.NewEncoder(w).Encode(map[string]any{"historyId": "100", "emailAddress": "agent@remote.example"})
	case strings.Contains(r.URL.Path, "/history"):
		var hist []any
		// The first history page names f.page, which the test rotates so the next
		// pass walks a fresh change window rather than replaying the baseline one.
		if f.page != nil {
			hist = append(hist, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]any{"id": f.page()}}}})
		}
		for _, id := range f.messages {
			hist = append(hist, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]any{"id": id}}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"history": hist, "historyId": "200"})
	case strings.Contains(r.URL.Path, "/messages/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if f.gone[id] {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 404, "status": "NOT_FOUND", "errors": []any{map[string]any{"reason": "notFound"}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "threadId": "t", "labelIds": []string{"INBOX"}, "internalDate": "1700000000000", "payload": map[string]any{"headers": []any{map[string]any{"name": "From", "value": "a@b.test"}, map[string]any{"name": "Subject", "value": "hello"}}}})
	case strings.Contains(r.URL.Path, "/labels"):
		json.NewEncoder(w).Encode(map[string]any{"labels": []any{}})
	case strings.HasSuffix(r.URL.Path, "/messages"):
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	default:
		w.WriteHeader(404)
	}
}
