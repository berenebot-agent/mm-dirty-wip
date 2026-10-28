package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/gmail"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// folderEnvelope is the shared listing envelope the folder routes return.
type folderEnvelope struct {
	Items []struct {
		ID           string `json:"id"`
		Path         string `json:"path"`
		Name         string `json:"name"`
		Role         string `json:"role"`
		MessageCount int64  `json:"message_count"`
		UnreadCount  int64  `json:"unread_count"`
	} `json:"items"`
}

// TestDomainFolderAPISystemFoldersOnly proves a domain mailbox exposes system
// folders only and refuses folder mutations: it organises mail with labels.
func TestDomainFolderAPISystemFoldersOnly(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)

	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/folders", key)
	if listed.Code != http.StatusOK {
		t.Fatalf("folder list = %d %s", listed.Code, listed.Body.String())
	}
	var env folderEnvelope
	if err := json.Unmarshal(listed.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Items) == 0 {
		t.Fatal("domain folder list is empty")
	}
	for _, f := range env.Items {
		if f.Role == model.FolderRoleFolder {
			t.Fatalf("domain folder list carries a custom folder: %+v", f)
		}
	}

	// Creating a custom folder is refused: the mailbox classifies with labels.
	created := apiDo(h, "POST", "/v1/inboxes/"+box.ID+"/folders", key, `{"path":"Projects"}`)
	if created.Code != http.StatusConflict {
		t.Fatalf("domain folder create = %d %s", created.Code, created.Body.String())
	}

	// A system folder is protected: it cannot be renamed or deleted.
	inbox, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	renamed := apiDo(h, "PATCH", "/v1/inboxes/"+box.ID+"/folders/"+inbox.ID, key, `{"name":"Start"}`)
	if renamed.Code != http.StatusForbidden {
		t.Fatalf("system folder rename = %d %s", renamed.Code, renamed.Body.String())
	}
	deleted := apiDo(h, "DELETE", "/v1/inboxes/"+box.ID+"/folders/"+inbox.ID, key, "")
	if deleted.Code != http.StatusForbidden {
		t.Fatalf("system folder delete = %d %s", deleted.Code, deleted.Body.String())
	}
}

// TestGmailFolderAPISystemFoldersOnly proves a Gmail mailbox exposes its
// system folders only: its user labels are additive labels, not locations, and
// it refuses folder mutations.
func TestGmailFolderAPISystemFoldersOnly(t *testing.T) {
	t.Parallel()
	st, dir := testdb.OpenDir(t)
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "google@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"SENT","name":"SENT","type":"system"},{"id":"DRAFT","name":"DRAFT","type":"system"},{"id":"TRASH","name":"TRASH","type":"system"},{"id":"SPAM","name":"SPAM","type":"system"},{"id":"Label_1","name":"Projects","type":"user"}]}`))
		case strings.HasSuffix(r.URL.Path, "/profile"):
			w.Write([]byte(`{"emailAddress":"google@example.net","historyId":"10"}`))
		case strings.HasSuffix(r.URL.Path, "/history"):
			w.Write([]byte(`{"historyId":"10","history":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	if err = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	key := adminKey(t, svc, u)

	// Mirror the provider's folders into the local index.
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/folders", key)
	if listed.Code != http.StatusOK {
		t.Fatalf("folder list = %d %s", listed.Code, listed.Body.String())
	}
	var env folderEnvelope
	if err := json.Unmarshal(listed.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range env.Items {
		seen[f.Path] = true
		if f.Role == model.FolderRoleFolder {
			t.Fatalf("Gmail folder list carries a user label: %+v", f)
		}
	}
	if !seen["INBOX"] || !seen["SENT"] {
		t.Fatalf("Gmail folder list misses a system folder: %+v", env.Items)
	}
	if seen["Label_1"] {
		t.Fatalf("Gmail folder list carries a user label: %+v", env.Items)
	}
	if seen["ARCHIVE"] {
		t.Fatalf("Gmail folder list carries a virtual archive folder: %+v", env.Items)
	}
	created := apiDo(h, "POST", "/v1/inboxes/"+box.ID+"/folders", key, `{"path":"Projects"}`)
	if created.Code != http.StatusConflict {
		t.Fatalf("Gmail folder create = %d %s", created.Code, created.Body.String())
	}
}

// TestGmailUnfilteredListIncludesNullFolderMessages proves an unfiltered Gmail
// listing is the whole mailbox, including the mail that carries no system
// label, and that a user label is not a folder scope.
func TestGmailUnfilteredListIncludesNullFolderMessages(t *testing.T) {
	t.Parallel()
	st, dir := testdb.OpenDir(t)
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "google@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"SENT","name":"SENT","type":"system"},{"id":"DRAFT","name":"DRAFT","type":"system"},{"id":"TRASH","name":"TRASH","type":"system"},{"id":"SPAM","name":"SPAM","type":"system"},{"id":"Label_1","name":"Projects","type":"user"}]}`))
		case strings.HasSuffix(r.URL.Path, "/profile"):
			w.Write([]byte(`{"emailAddress":"google@example.net","historyId":"10"}`))
		case strings.HasSuffix(r.URL.Path, "/history"):
			w.Write([]byte(`{"historyId":"10","history":[]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/inboxed"):
			w.Write([]byte(`{"id":"inboxed","threadId":"t1","labelIds":["INBOX"],"internalDate":"1700000000000","snippet":"in","payload":{"headers":[{"name":"Subject","value":"Inboxed"},{"name":"From","value":"Sender <sender@example.net>"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/messages/nullfolder"):
			w.Write([]byte(`{"id":"nullfolder","threadId":"t2","labelIds":["Label_1"],"internalDate":"1700000000000","snippet":"nf","payload":{"headers":[{"name":"Subject","value":"No system label"},{"name":"From","value":"Sender <sender@example.net>"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Write([]byte(`{"messages":[{"id":"inboxed","threadId":"t1"},{"id":"nullfolder","threadId":"t2"}]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	if err = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	all, err := rm.ListRemoteMessages(ctx, p, box.ID, "", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 2 {
		t.Fatalf("unfiltered listing = %d items, want every cached message", len(all.Items))
	}
	inboxView, err := rm.ListRemoteMessages(ctx, p, box.ID, "INBOX", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(inboxView.Items) != 1 || inboxView.Items[0].Subject != "Inboxed" {
		t.Fatalf("inbox listing = %+v", inboxView.Items)
	}
	// A user label is an additive label, not a folder scope.
	labelView, err := rm.ListRemoteMessages(ctx, p, box.ID, "Label_1", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(labelView.Items) != 0 {
		t.Fatalf("user label listing = %+v, want no rows", labelView.Items)
	}
	// A client may address a system folder by its stable id.
	folders, err := svc.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	inboxID := ""
	for _, f := range folders {
		if f.Role == model.FolderRoleInbox {
			inboxID = f.ID
		}
	}
	if inboxID == "" {
		t.Fatalf("no inbox folder row: %+v", folders)
	}
	byID, err := rm.ListRemoteMessages(ctx, p, box.ID, inboxID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byID.Items) != 1 || byID.Items[0].Subject != "Inboxed" {
		t.Fatalf("folder id listing = %+v", byID.Items)
	}
}

// TestGmailSearchUsesSingleFolderPrecedence proves the live Gmail search
// resolves the same single folder view as the cached listing: the Inbox view
// excludes the views that outrank it.
func TestGmailSearchUsesSingleFolderPrecedence(t *testing.T) {
	t.Parallel()
	st, dir := testdb.OpenDir(t)
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "google@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lastQuery, lastLabel string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			mu.Lock()
			lastQuery, lastLabel = r.URL.Query().Get("q"), r.URL.Query().Get("labelIds")
			mu.Unlock()
			w.Write([]byte(`{"messages":[]}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	if err = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.SearchRemote(ctx, p, box.ID, app.RemoteSearchQuery{FolderPath: "INBOX"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	query, label := lastQuery, lastLabel
	mu.Unlock()
	if label != "INBOX" {
		t.Fatalf("search label = %q, want INBOX", label)
	}
	for _, want := range []string{"-in:trash", "-in:spam", "-in:drafts"} {
		if !strings.Contains(query, want) {
			t.Fatalf("search query %q misses %q", query, want)
		}
	}
	if strings.Contains(query, "-in:sent") {
		t.Fatalf("search query %q excludes a lower-precedence view", query)
	}
	// A user label is an additive label, not a folder: it selects no folder
	// view, so the provider search is not run for it.
	res, err := rm.SearchRemote(ctx, p, box.ID, app.RemoteSearchQuery{FolderPath: "Label_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("user label search = %+v, want no rows", res.Items)
	}
	_ = time.Now
}

// TestDomainFolderCountsMatchTheListing proves a domain inbox's folders report
// the size of the view a reader gets. The counts were only ever fed from the
// remote index, which holds nothing for a local mailbox, so every folder of a
// domain inbox reported 0 while its listing returned rows.
func TestDomainFolderCountsMatchTheListing(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)
	// One inbox message, and one classified into Spam.
	seedInbound(t, svc, box, "counts-inbox", "<counts-inbox@test>", "Inbox mail", "body")
	spammed := seedInbound(t, svc, box, "counts-spam", "<counts-spam@test>", "Spam mail", "body")
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, spammed.ID, true); err != nil {
		t.Fatal(err)
	}
	counts := func() map[string]struct{ total, unread int64 } {
		t.Helper()
		listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/folders", key)
		if listed.Code != http.StatusOK {
			t.Fatalf("folder list = %d %s", listed.Code, listed.Body.String())
		}
		var env folderEnvelope
		if err := json.Unmarshal(listed.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		out := map[string]struct{ total, unread int64 }{}
		for _, f := range env.Items {
			out[f.Role] = struct{ total, unread int64 }{f.MessageCount, f.UnreadCount}
		}
		return out
	}
	got := counts()
	if got[model.FolderRoleInbox].total != 1 {
		t.Fatalf("Inbox count = %d want 1 (the Spam-flagged mail is not Inbox mail)", got[model.FolderRoleInbox].total)
	}
	if got[model.FolderRoleInbox].unread != 1 {
		t.Fatalf("Inbox unread = %d want 1", got[model.FolderRoleInbox].unread)
	}
	if got[model.FolderRoleSpam].total != 1 {
		t.Fatalf("Spam count = %d want 1", got[model.FolderRoleSpam].total)
	}
	// The count is the size of the folder's listing, not an independent guess.
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?folder="+inboxFolder.ID, key)
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &page) != nil {
		t.Fatalf("inbox listing = %d %s", listed.Code, listed.Body.String())
	}
	if int64(len(page.Items)) != got[model.FolderRoleInbox].total {
		t.Fatalf("Inbox listing has %d rows, folder count says %d", len(page.Items), got[model.FolderRoleInbox].total)
	}
}
