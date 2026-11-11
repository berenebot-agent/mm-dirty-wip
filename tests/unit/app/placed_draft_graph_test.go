package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/microsoft"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

const graphDraftMIME = "From: Agent <agent@remote.example>\r\nTo: someone@example.com\r\nSubject: Original\r\n" +
	"Message-ID: <graph-draft@remote.example>\r\nDate: Mon, 13 Oct 2026 09:10:00 +0000\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\noriginal body\r\n"

// fakeGraphDraft is a one-draft Graph mailbox that records conditional writes.
type fakeGraphDraft struct {
	mu        sync.Mutex
	changeKey int
	subject   string
	folder    string
	patches   int
	ifMatch   []string
	// beforePatch runs once, under the lock, just before the first PATCH is judged.
	beforePatch func(f *fakeGraphDraft)
}

func (f *fakeGraphDraft) etag() string { return fmt.Sprintf(`W/"CK-%d"`, f.changeKey) }

func (f *fakeGraphDraft) message() map[string]any {
	return map[string]any{
		"@odata.etag": f.etag(), "id": "DRAFT", "changeKey": fmt.Sprintf("CK-%d", f.changeKey),
		"conversationId": "conv", "parentFolderId": f.folder, "internetMessageId": "<graph-draft@remote.example>",
		"subject": f.subject, "isDraft": true, "isRead": true, "receivedDateTime": "2026-10-13T09:10:00Z",
		"from":          map[string]any{"emailAddress": map[string]string{"address": "agent@remote.example", "name": "Agent"}},
		"toRecipients":  []any{map[string]any{"emailAddress": map[string]string{"address": "someone@example.com"}}},
		"ccRecipients":  []any{},
		"bccRecipients": []any{map[string]any{"emailAddress": map[string]string{"address": "hidden@example.com"}}},
	}
}

func (f *fakeGraphDraft) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/mailFolders"):
			_, _ = w.Write([]byte(`{"value":[{"id":"inbox-id","displayName":"Inbox"},{"id":"drafts-id","displayName":"Drafts"}]}`))
		case strings.HasSuffix(path, "/mailFolders/inbox"):
			_, _ = w.Write([]byte(`{"id":"inbox-id","displayName":"Inbox"}`))
		case strings.HasSuffix(path, "/mailFolders/drafts"):
			_, _ = w.Write([]byte(`{"id":"drafts-id","displayName":"Drafts"}`))
		case strings.HasSuffix(path, "/messages/delta"):
			value := []any{}
			if strings.Contains(path, "/drafts-id/") {
				value = append(value, f.message())
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": value, "@odata.deltaLink": "http://" + r.Host + path + "?cursor=next"})
		case strings.Contains(path, "/mailFolders/"):
			// Every other well-known folder resolves to an empty folder of its own.
			well := path[strings.LastIndex(path, "/")+1:]
			_ = json.NewEncoder(w).Encode(map[string]string{"id": well + "-id", "displayName": well})
		case strings.HasSuffix(path, "/messages/DRAFT/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = w.Write([]byte(graphDraftMIME))
		case strings.HasSuffix(path, "/messages/DRAFT") && r.Method == http.MethodPatch:
			if f.beforePatch != nil {
				f.beforePatch(f)
				f.beforePatch = nil
			}
			got := r.Header.Get("If-Match")
			f.ifMatch = append(f.ifMatch, got)
			if got != "" && got != f.etag() {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"error":{"code":"ErrorIrresolvableConflict","message":"changed"}}`))
				return
			}
			var body struct {
				Subject string `json:"subject"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.subject = body.Subject
			f.changeKey++
			f.patches++
			_ = json.NewEncoder(w).Encode(f.message())
		case strings.HasSuffix(path, "/messages/DRAFT"):
			_ = json.NewEncoder(w).Encode(f.message())
		default:
			t.Errorf("unexpected Graph request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func graphDraftEnv(t *testing.T, f *fakeGraphDraft) (*app.Service, model.Principal, model.Inbox, *app.RemoteMailboxService, string) {
	t.Helper()
	ctx := context.Background()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 10, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	// No EnsureSystemFolders here: a Graph inbox gets its folders, and their
	// roles, from the provider.
	box, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		DisplayName: "Graph", Address: "agent@remote.example",
		Remote: &model.RemoteConnection{Host: "imap.remote.example", Username: "agent@remote.example", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	rm.Microsoft = microsoft.NewClient(srv.Client())
	rm.Microsoft.BaseURL = srv.URL
	if err = rm.SaveMicrosoftGrant(ctx, store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "c", Tenant: "common", IdentityID: "id", Mailbox: box.Address},
		microsoft.Token{AccessToken: "access", RefreshToken: "r", ExpiresIn: 3600, Scope: microsoft.Scope(false)}); err != nil {
		t.Fatal(err)
	}
	// Reconcile discovers the provider's folders and their roles.
	if _, err = rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	folder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleDrafts)
	if err != nil {
		t.Fatal(err)
	}
	// Index the draft as a delta pull would, in the provider's Drafts folder.
	row, err := svc.Store.UpsertMicrosoftMessage(ctx, u.AccountID, box.ID, "DRAFT", "conv", folder.Path, store.RemoteMessageInput{
		RFCMessageID: "<graph-draft@remote.example>", FromName: "Agent", FromAddress: "agent@remote.example",
		To: []string{"someone@example.com"}, Subject: f.subject, Read: true, Draft: true,
	})
	if err != nil {
		t.Fatalf("seed draft row: %v", err)
	}
	f.mu.Lock()
	f.folder = folder.Path
	f.mu.Unlock()
	return svc, model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}, box, rm, row.ID
}

// TestGraphPlacedDraftSaveIsConditionalAndRefreshesTheIndex proves an Outlook
// draft save sends the entity tag it read, keeps the Bcc recipients Graph holds
// separately from the MIME, and updates the cached row at once.
func TestGraphPlacedDraftSaveIsConditionalAndRefreshesTheIndex(t *testing.T) {
	t.Parallel()
	f := &fakeGraphDraft{changeKey: 1, subject: "Original"}
	svc, p, box, rm, id := graphDraftEnv(t, f)
	ctx := context.Background()

	pd, err := rm.LoadPlacedDraft(ctx, p, box.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if pd.Marker != "ms:CK-1" {
		t.Fatalf("marker = %q, want the live changeKey", pd.Marker)
	}
	if len(pd.Fields.BCC) != 1 || pd.Fields.BCC[0] != "hidden@example.com" {
		t.Fatalf("Bcc = %v, want the recipient Graph holds", pd.Fields.BCC)
	}
	fields := pd.Fields
	fields.Subject, fields.Text = "Edited", "new body"
	newID, err := rm.SavePlacedDraft(ctx, p, box.ID, id, fields, pd.Marker)
	if err != nil {
		t.Fatal(err)
	}
	if newID != id {
		t.Fatalf("Graph edits in place, row id moved: %s -> %s", id, newID)
	}
	f.mu.Lock()
	if len(f.ifMatch) != 1 || f.ifMatch[0] != `W/"CK-1"` {
		t.Fatalf("PATCH If-Match = %q, want the tag that was read", f.ifMatch)
	}
	f.mu.Unlock()
	rec, err := svc.Store.GetRemoteMessage(ctx, p.AccountID, box.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Subject != "Edited" {
		t.Fatalf("cached subject = %q, want the edit without waiting for the pull", rec.Subject)
	}
}

// TestGraphPlacedDraftSaveStopsWhenTheDraftMovesMidSave proves the provider-side
// precondition catches an edit that lands between the live check and the write.
func TestGraphPlacedDraftSaveStopsWhenTheDraftMovesMidSave(t *testing.T) {
	t.Parallel()
	f := &fakeGraphDraft{changeKey: 1, subject: "Original"}
	svc, p, box, rm, id := graphDraftEnv(t, f)
	ctx := context.Background()

	pd, err := rm.LoadPlacedDraft(ctx, p, box.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	// The operator saves in Outlook right after the check passed.
	f.mu.Lock()
	f.beforePatch = func(f *fakeGraphDraft) { f.changeKey++; f.subject = "Changed in Outlook" }
	f.mu.Unlock()
	fields := pd.Fields
	fields.Subject = "Edited"
	if _, err = rm.SavePlacedDraft(ctx, p, box.ID, id, fields, pd.Marker); !errors.Is(err, app.ErrPlacedDraftChanged) {
		t.Fatalf("save = %v, want ErrPlacedDraftChanged", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subject != "Changed in Outlook" || f.patches != 0 {
		t.Fatalf("the other client's edit was overwritten: subject=%q patches=%d", f.subject, f.patches)
	}
	_ = svc
}

// TestGraphPlacedDraftSaveRefusesAStaleMarker proves the live changeKey, not the
// cache, decides: the cached row is untouched but Graph's revision moved.
func TestGraphPlacedDraftSaveRefusesAStaleMarker(t *testing.T) {
	t.Parallel()
	f := &fakeGraphDraft{changeKey: 1, subject: "Original"}
	_, p, box, rm, id := graphDraftEnv(t, f)
	ctx := context.Background()

	pd, err := rm.LoadPlacedDraft(ctx, p, box.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.changeKey++
	f.mu.Unlock()
	if _, err = rm.SavePlacedDraft(ctx, p, box.ID, id, pd.Fields, pd.Marker); !errors.Is(err, app.ErrPlacedDraftChanged) {
		t.Fatalf("save = %v, want ErrPlacedDraftChanged", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches != 0 {
		t.Fatal("a stale save reached Graph")
	}
}
