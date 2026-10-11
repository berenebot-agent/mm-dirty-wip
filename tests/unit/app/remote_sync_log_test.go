package app_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// TestIMAPSyncIsLogged proves a manual IMAP detection pass emits one "remote
// sync" INFO line naming the provider, inbox, trigger, a duration, the number of
// newly-detected messages and the completion status. A subsequent pass that
// finds nothing still logs (the heartbeat), with new=0.
func TestIMAPSyncIsLogged(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

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

	// A quiet repeat logs new=0.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	final := sink.syncRecords()
	if q := final[len(final)-1]; q["new"] != "0" {
		t.Fatalf("quiet pass new = %q, want 0", q["new"])
	}
}

// TestGmailSyncIsLogged proves the Gmail history detection pass emits a "remote
// sync" line with provider=gmail, and reports the added-message count.
func TestGmailSyncIsLogged(t *testing.T) {
	svc, box, rm, fixture := googleLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	w := app.NewRemoteWorker(svc, rm, slog.New(sink))
	w.SetMaxInboxConnections(1)
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
}

// add appends a message to the inbox that the next delta pass will report.
func (f *microsoftLogFixture) add(id, folder, subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, map[string]any{"id": id, "conversationId": "c-" + id, "parentFolderId": folder, "subject": subject, "receivedDateTime": "2026-10-11T10:00:00Z"})
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
	case strings.HasSuffix(r.URL.Path, "/mailFolders"):
		json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "inbox-id", "displayName": "Inbox"}}})
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
	mu       sync.Mutex
	url      string
	messages []string
}

func (f *googleLogFixture) add(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, id)
}

func (f *googleLogFixture) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "/profile"):
		json.NewEncoder(w).Encode(map[string]any{"historyId": "100", "emailAddress": "agent@remote.example"})
	case strings.Contains(r.URL.Path, "/history"):
		var hist []any
		for _, id := range f.messages {
			hist = append(hist, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]any{"id": id}}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"history": hist, "historyId": "200"})
	case strings.Contains(r.URL.Path, "/messages/"):
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		json.NewEncoder(w).Encode(map[string]any{"id": id, "threadId": "t", "labelIds": []string{"INBOX"}, "internalDate": "1700000000000", "payload": map[string]any{"headers": []any{map[string]any{"name": "From", "value": "a@b.test"}, map[string]any{"name": "Subject", "value": "hello"}}}})
	case strings.Contains(r.URL.Path, "/labels"):
		json.NewEncoder(w).Encode(map[string]any{"labels": []any{}})
	case strings.HasSuffix(r.URL.Path, "/messages"):
		json.NewEncoder(w).Encode(map[string]any{"messages": []any{}})
	default:
		w.WriteHeader(404)
	}
}
