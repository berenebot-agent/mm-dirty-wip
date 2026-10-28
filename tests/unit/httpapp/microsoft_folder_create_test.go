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
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/microsoft"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// TestMicrosoftFolderCreateFromUILandsAtRoot proves the New-folder dialog (which
// posts the folder name, not a path) creates a root folder. The name used to be
// sent as a Graph parent folder id, so Graph refused the request as "provider
// rejected the request" and no folder was ever created.
func TestMicrosoftFolderCreateFromUILandsAtRoot(t *testing.T) {
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
	var mu sync.Mutex
	created := ""
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/mailFolders"):
			var body struct{ DisplayName string }
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			created = r.URL.Path + " " + body.DisplayName
			mu.Unlock()
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]string{"id": "projects-id", "displayName": body.DisplayName})
		case strings.Contains(r.URL.Path, "/mailFolders/"):
			// System folder probes resolve; anything else (including a folder the
			// fixture was never told to create under) is not found, as in Graph.
			if id := r.URL.Path[strings.Index(r.URL.Path, "/mailFolders/")+len("/mailFolders/"):]; id == "projects-id" {
				json.NewEncoder(w).Encode(map[string]string{"id": id, "displayName": "Projects"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "ErrorInvalidIdMalformed"}})
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			w.Write([]byte(`{"value":[{"id":"projects-id","displayName":"Projects"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	c := store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: "microsoft@example.net"}
	if err = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	session, csrf, err := svc.Store.CreateSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	form := "_csrf=" + csrf + "&path=Projects"
	r := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/folders", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "mmm_session", Value: session})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create folder: %d %s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	got := created
	mu.Unlock()
	if got != "/users/microsoft@example.net/mailFolders Projects" {
		t.Fatalf("create route = %q, want the mailbox root", got)
	}
}
