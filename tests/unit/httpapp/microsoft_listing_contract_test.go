package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// microsoftFolders is the Graph folder tree the listing fixture serves: the
// system folders and a user-created one, each holding a single message.
var microsoftFolders = map[string]string{
	"inbox-id": "Inbox", "sent-id": "Sent Items", "drafts-id": "Drafts",
	"projects-id": "Projects", "junk-id": "Junk Email", "trash-id": "Deleted Items",
}

// microsoftListingFixture serves one Graph mailbox whose delta enumerates the
// folder the request names, so a whole-mailbox read must reach every folder the
// inbox mirrored.
func microsoftListingFixture(t *testing.T) *httptest.Server {
	t.Helper()
	wellKnown := map[string]string{"inbox": "inbox-id", "sentitems": "sent-id", "drafts": "drafts-id", "junkemail": "junk-id", "deleteditems": "trash-id"}
	var fixture *httptest.Server
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			parts := strings.Split(r.URL.Path, "/mailFolders/")
			folder := strings.TrimSuffix(parts[len(parts)-1], "/messages/delta")
			value := []any{}
			if name, ok := microsoftFolders[folder]; ok {
				// Distinct days keep the merged order deterministic.
				day := len(folder)%9 + 1
				value = append(value, map[string]any{
					"id": "prov-" + folder, "conversationId": "conv-" + folder, "parentFolderId": folder,
					"subject": name, "isRead": true, "receivedDateTime": "2026-10-0" + string(rune('0'+day)) + "T10:00:00Z",
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value, "@odata.deltaLink": fixture.URL + r.URL.Path + "?cursor=next"})
		case strings.Contains(r.URL.Path, "/mailFolders/"):
			id := r.URL.Path[strings.Index(r.URL.Path, "/mailFolders/")+len("/mailFolders/"):]
			if known := wellKnown[id]; known != "" {
				id = known
			}
			name, ok := microsoftFolders[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "displayName": name})
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			value := []any{}
			for id, name := range microsoftFolders {
				value = append(value, map[string]any{"id": id, "displayName": name, "totalItemCount": 1})
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

// TestMicrosoftListingSpansFoldersAndResolvesFolderIDs proves the connected
// Microsoft mailbox follows the shared listing contract: a plain read spans
// every folder (user-created ones included) except Spam and Trash,
// folder=<fld_id> scopes to that folder, and a role filter refines an explicit
// folder instead of replacing it.
func TestMicrosoftListingSpansFoldersAndResolvesFolderIDs(t *testing.T) {
	t.Parallel()
	st, dir := testdb.OpenDir(t)
	svc, err := app.New(testConfig(dir), st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "microsoft@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	fixture := microsoftListingFixture(t)
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	c := store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: "microsoft@example.net"}
	if err = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	handler := srv.Handler()
	key := adminKey(t, svc, u)
	refreshRemote(t, handler, box.ID, key)

	rr := apiGet(t, handler, "/v1/inboxes/"+box.ID+"/folders", key)
	var folders struct {
		Items []struct{ ID, Path, Role string } `json:"items"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &folders) != nil {
		t.Fatalf("folders: %d %s", rr.Code, rr.Body.String())
	}
	byRole := map[string]string{}
	for _, f := range folders.Items {
		byRole[f.Role+"|"+f.Path] = f.ID
	}
	inbox, sent, projects := byRole["inbox|inbox-id"], byRole["sent|sent-id"], byRole["folder|projects-id"]
	junk, trash := byRole["spam|junk-id"], byRole["trash|trash-id"]
	for name, id := range map[string]string{"inbox": inbox, "sent": sent, "projects": projects, "junk": junk, "trash": trash} {
		if id == "" {
			t.Fatalf("no %s folder: %s", name, rr.Body.String())
		}
	}

	folderIDs := func(query string) map[string]int {
		t.Helper()
		rr := apiGet(t, handler, "/v1/inboxes/"+box.ID+"/messages?"+query, key)
		var page struct {
			Items []struct {
				FolderID *string `json:"folder_id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", query, rr.Code, rr.Body.String())
		}
		out := map[string]int{}
		for _, item := range page.Items {
			id := "<nil>"
			if item.FolderID != nil {
				id = *item.FolderID
			}
			out[id]++
		}
		return out
	}

	seen := folderIDs("")
	if seen[junk] != 0 || seen[trash] != 0 {
		t.Fatalf("plain listing leaked Spam/Trash: %v", seen)
	}
	for name, id := range map[string]string{"inbox": inbox, "sent": sent, "projects": projects} {
		if seen[id] != 1 {
			t.Fatalf("plain listing missing %s (%s): %v", name, id, seen)
		}
	}
	for name, id := range map[string]string{"sent": sent, "projects": projects, "trash": trash} {
		if got := folderIDs("folder=" + id); len(got) != 1 || got[id] != 1 {
			t.Fatalf("folder=%s (%s) = %v", name, id, got)
		}
	}
	for name, tc := range map[string]struct {
		id   string
		want int
	}{"sent": {sent, 0}, "trash": {trash, 1}} {
		if got := folderIDs("folder=" + tc.id + "&trashed=true"); len(got) != tc.want {
			t.Fatalf("folder=%s&trashed=true = %v, want %d rows", name, got, tc.want)
		}
	}
	if got := folderIDs("folder=" + sent + "&direction=outbound"); len(got) != 1 || got[sent] != 1 {
		t.Fatalf("folder=Sent&direction=outbound = %v", got)
	}
	if got := folderIDs("folder=" + inbox + "&direction=outbound"); len(got) != 0 {
		t.Fatalf("folder=Inbox&direction=outbound = %v, want none", got)
	}
	// An unknown folder id selects nothing rather than the whole mailbox.
	if got := folderIDs("folder=fld_doesnotexist"); len(got) != 0 {
		t.Fatalf("unknown folder id = %v", got)
	}
}
