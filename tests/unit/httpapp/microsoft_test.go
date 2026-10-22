package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

func TestMicrosoftMailboxMovesAndPermissions(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	box, e := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "shared@example.net"})
	if e != nil {
		t.Fatal(e)
	}
	folder := "inbox-id"
	provider := "StableID"
	expired := false
	read := false
	var categories []string
	var fixture *httptest.Server
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
			t.Errorf("missing Graph headers")
		}
		if !strings.HasPrefix(r.URL.Path, "/users/shared@example.net/") {
			t.Errorf("escaped mailbox boundary: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		message := map[string]any{"id": provider, "conversationId": "thread", "parentFolderId": folder, "subject": "Hello", "isRead": read, "categories": categories, "receivedDateTime": "2026-10-10T10:00:00Z"}
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/StableID/move"):
			var body struct{ DestinationID string }
			json.NewDecoder(r.Body).Decode(&body)
			folder = body.DestinationID
			message["parentFolderId"] = folder
			json.NewEncoder(w).Encode(message)
		case strings.HasSuffix(r.URL.Path, "/messages/StableID"):
			if r.Method == "PATCH" {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				if b := body["isRead"]; b != nil {
					json.Unmarshal(b, &read)
				}
				if b := body["categories"]; b != nil {
					json.Unmarshal(b, &categories)
				}
				message["isRead"] = read
				message["categories"] = categories
			}
			json.NewEncoder(w).Encode(message)
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			if expired && strings.Contains(r.URL.Path, "/inbox-id/") {
				expired = false
				w.WriteHeader(410)
				return
			}
			value := []any{}
			if strings.Contains(r.URL.Path, "/"+folder+"/") {
				value = append(value, message)
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value, "@odata.deltaLink": fixture.URL + r.URL.Path + "?cursor=next"})
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			w.Write([]byte(`{"value":[{"id":"inbox-id","displayName":"Posteingang"},{"id":"archive-id","displayName":"Archiv"}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			w.Write([]byte(`{"id":"inbox-id","displayName":"Posteingang"}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/archive"):
			w.Write([]byte(`{"id":"archive-id","displayName":"Archiv"}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value := []any{}
			if strings.Contains(r.URL.Path, "/"+folder+"/") {
				value = append(value, message)
			}
			json.NewEncoder(w).Encode(map[string]any{"value": value})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	c := store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: "shared@example.net", Shared: true}
	if e = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)}); e != nil {
		t.Fatal(e)
	}
	boxes, e := svc.Store.ListInboxes(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if e != nil {
		t.Fatal(e)
	}
	ready := false
	for _, b := range boxes {
		if b.ID == box.ID {
			ready = b.Capabilities != nil && b.Capabilities.Outbound && !b.Capabilities.Realtime
		}
	}
	if !ready {
		t.Fatal("Microsoft inbox missing sending capability in dashboard list")
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	page, e := rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 1 {
		t.Fatalf("list: %#v %v", page, e)
	}
	id := page.Items[0].ID
	arrivals, e := svc.Store.ListRemoteArrivalsForInbox(ctx, u.AccountID, box.ID, 100)
	if e != nil || len(arrivals) != 0 {
		t.Fatalf("backfill emitted arrivals: %#v %v", arrivals, e)
	}
	if _, e = rm.GetRemoteMessage(ctx, model.Principal{AccountID: u.AccountID}, box.ID, id); e == nil {
		t.Fatal("unscoped read accepted")
	}
	reader := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, e = rm.SetRemoteRead(ctx, reader, box.ID, id, true); e == nil {
		t.Fatal("reader mutation accepted")
	}
	v, e := rm.SetRemoteRead(ctx, p, box.ID, id, true)
	if e != nil || !v.Read {
		t.Fatalf("read %#v %v", v, e)
	}
	v, e = rm.AddRemoteLabels(ctx, p, box.ID, id, []string{"Projects"})
	if e != nil || len(v.Labels) != 1 {
		t.Fatalf("categories %#v %v", v, e)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	catalog := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", adminKey(t, svc, u))
	if catalog.Code != http.StatusOK || !strings.Contains(catalog.Body.String(), `"name":"Projects"`) ||
		!strings.Contains(catalog.Body.String(), `"membership":"multiple"`) {
		t.Fatalf("Graph categories should be additive labels: %d %s", catalog.Code, catalog.Body.String())
	}
	var labels struct {
		Items []struct{ ID, Role, Membership string } `json:"items"`
	}
	if err := json.Unmarshal(catalog.Body.Bytes(), &labels); err != nil {
		t.Fatal(err)
	}
	var inboxLabel string
	for _, label := range labels.Items {
		if label.Role == "inbox" {
			inboxLabel = label.ID
			if label.Membership != "exclusive" {
				t.Fatalf("Outlook folder must stay exclusive: %+v", label)
			}
		}
	}
	if inboxLabel == "" {
		t.Fatal("missing Outlook Inbox classification")
	}
	key := adminKey(t, svc, u)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?", "/v1/messages?inbox=" + box.ID + "&",
		"/v1/inboxes/" + box.ID + "/search?", "/v1/search?inbox=" + box.ID + "&",
	} {
		w := apiGet(t, srv.Handler(), path+"label_id="+url.QueryEscape(inboxLabel)+"&label_id=tag:Projects", key)
		var page struct {
			Items []struct {
				ID       string
				LabelIDs []string `json:"label_ids"`
			} `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != id || !stringSliceContains(page.Items[0].LabelIDs, inboxLabel) || !stringSliceContains(page.Items[0].LabelIDs, "tag:Projects") {
			t.Fatalf("Outlook label filter %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	v, e = rm.MoveRemoteMessage(ctx, p, box.ID, id, "archive-id")
	if e != nil || v.ID != id || v.FolderPath != "archive-id" {
		t.Fatalf("move %#v %v", v, e)
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 0 {
		t.Fatalf("stale inbox %#v %v", page, e)
	}
	c, e = svc.Store.MicrosoftConnection(ctx, u.AccountID, box.ID)
	if e != nil || strings.Contains(c.EncryptedToken, "refresh") {
		t.Fatalf("credentials %v", e)
	}
	c.Mailbox = "another@example.net"
	if e = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)}); e == nil {
		t.Fatal("rebind accepted")
	}
	provider = "NewArrival"
	folder = "inbox-id"
	for n := 0; n < 2; n++ {
		if _, e = rm.ReconcileRemote(ctx, u.AccountID, box.ID); e != nil {
			t.Fatal(e)
		}
	}
	arrivals, e = svc.Store.ListRemoteArrivalsForInbox(ctx, u.AccountID, box.ID, 100)
	if e != nil || len(arrivals) != 1 {
		t.Fatalf("deduplication %#v %v", arrivals, e)
	}
	r, e := svc.Store.RemoteMessageForArrival(ctx, u.AccountID, arrivals[0].ID)
	if e != nil || r.Subject != "Hello" {
		t.Fatalf("arrival resolution %#v %v", r, e)
	}
	expired = true
	provider = "AfterRecovery"
	for n := 0; n < 3; n++ {
		if _, e = rm.ReconcileRemote(ctx, u.AccountID, box.ID); e != nil {
			t.Fatal(e)
		}
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "inbox-id", 50, "")
	if e != nil || len(page.Items) != 1 {
		t.Fatalf("resync left stale metadata %#v %v", page, e)
	}
}

func TestMicrosoftOAuthPasteStateAndReplay(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	var logs bytes.Buffer
	svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	svc.Config.MicrosoftClientID = "client"
	svc.Config.MicrosoftTenantID = "common"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			r.ParseForm()
			if r.Form.Get("client_secret") != "" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != microsoft.RedirectURI {
				t.Error("bad public-client exchange")
			}
			json.NewEncoder(w).Encode(microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)})
			return
		}
		if r.URL.Path == "/me" {
			w.Write([]byte(`{"id":"identity","mail":"personal@example.net","displayName":"Personal Owner"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/mailFolders/inbox") {
			w.Write([]byte(`{"id":"inbox-id"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	rm.Microsoft.AuthBaseURL = fixture.URL
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	session, csrf, e := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: session})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	rr := post("/ui/oauth/microsoft/begin", url.Values{"mailbox_type": {"personal"}})
	if rr.Code != 200 {
		t.Fatalf("begin %d %s", rr.Code, rr.Body)
	}
	var result map[string]string
	json.Unmarshal(rr.Body.Bytes(), &result)
	auth, _ := url.Parse(result["authorization_url"])
	if auth.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	state := auth.Query().Get("state")
	for _, callback := range []string{"http://evil.example/?state=" + state + "&code=secret", "http://localhost/?state=wrong&code=secret"} {
		rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
		if rr.Code != 400 {
			t.Fatalf("invalid callback accepted %d", rr.Code)
		}
	}
	callback := "http://localhost/?state=" + url.QueryEscape(state) + "&code=secret"
	rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if rr.Code != 200 {
		t.Fatalf("finish %d %s", rr.Code, rr.Body)
	}
	rr = post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if rr.Code != 400 {
		t.Fatal("callback replay accepted")
	}
	boxes, e := svc.Store.ListStandaloneInboxes(context.Background(), u.AccountID)
	if e != nil {
		t.Fatal(e)
	}
	if len(boxes) != 2 {
		t.Fatalf("personal inbox missing: %#v", boxes)
	}
	if boxes[1].DisplayName != "Personal Owner" || boxes[1].Address != "personal@example.net" {
		t.Fatalf("personal label not defaulted from the identity: %#v", boxes[1])
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 1 || records[0]["msg"] != "inbox.created" || records[0]["operation"] != "microsoft_connect" || records[0]["source"] != "ui" || records[0]["user_id"] != u.ID {
		t.Fatalf("Microsoft lifecycle attribution: %s", logs.String())
	}
	for _, private := range []string{"refresh", "secret", session, csrf, state} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("Microsoft credential leaked: %q", private)
		}
	}
}

// TestMicrosoftSharedMailboxSelection covers the two-phase shared flow: the
// sign-in returns before mailbox choice, accessible shared mailboxes are
// discovered from the directory, and the chosen address completes the inbox.
func TestMicrosoftSharedMailboxSelection(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	var logs bytes.Buffer
	svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	svc.Config.MicrosoftClientID = "client"
	svc.Config.MicrosoftTenantID = "common"
	var probeMu sync.Mutex
	probes := map[string]bool{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			json.NewEncoder(w).Encode(microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(true)})
		case r.URL.Path == "/me":
			w.Write([]byte(`{"id":"identity","mail":"owner@example.net","displayName":"Owner"}`))
		case r.URL.Path == "/users":
			if got := r.URL.Query().Get("$select"); !strings.Contains(got, "displayName") {
				t.Errorf("directory select missing displayName: %s", got)
			}
			w.Write([]byte(`{"value":[{"id":"u2","displayName":"Team Box","mail":"shared@example.net"},{"id":"u3","displayName":"Other","mail":"other@example.net"}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			probeMu.Lock()
			probes[r.URL.Path] = true
			probeMu.Unlock()
			if r.URL.Path != "/users/shared@example.net/mailFolders/inbox" {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte(`{"id":"inbox-id"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Microsoft = microsoft.NewClient(fixture.Client())
	rm.Microsoft.BaseURL = fixture.URL
	rm.Microsoft.AuthBaseURL = fixture.URL
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	session, csrf, e := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: session})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	begin := post("/ui/oauth/microsoft/begin", url.Values{"mailbox_type": {"shared"}, "display": {"Shared Team"}})
	if begin.Code != 200 {
		t.Fatalf("begin %d %s", begin.Code, begin.Body)
	}
	var started map[string]string
	json.Unmarshal(begin.Body.Bytes(), &started)
	auth, _ := url.Parse(started["authorization_url"])
	if !strings.Contains(auth.Query().Get("scope"), "Mail.ReadWrite.Shared") || !strings.Contains(auth.Query().Get("scope"), "User.ReadBasic.All") {
		t.Fatalf("shared scopes missing: %s", auth.Query().Get("scope"))
	}
	callback := "http://localhost/?state=" + url.QueryEscape(auth.Query().Get("state")) + "&code=secret"
	phase1 := post("/ui/oauth/microsoft/finish", url.Values{"callback_url": {callback}})
	if phase1.Code != 200 {
		t.Fatalf("shared finish %d %s", phase1.Code, phase1.Body)
	}
	var choice struct {
		State     string `json:"selection_state"`
		Mailboxes []struct{ Address, Name string }
	}
	if json.Unmarshal(phase1.Body.Bytes(), &choice); choice.State == "" || len(choice.Mailboxes) != 1 || choice.Mailboxes[0].Address != "shared@example.net" || choice.Mailboxes[0].Name != "Team Box" {
		t.Fatalf("shared discovery %s", phase1.Body)
	}
	probeMu.Lock()
	otherProbed := probes["/users/other@example.net/mailFolders/inbox"]
	probeMu.Unlock()
	if !otherProbed {
		t.Fatal("inaccessible candidate was not probed")
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"not-an-address"}}); rr.Code != 400 {
		t.Fatal("invalid shared address accepted")
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"other@example.net"}}); rr.Code != 400 {
		t.Fatal("inaccessible shared mailbox accepted")
	}
	done := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"shared@example.net"}})
	if done.Code != 200 {
		t.Fatalf("selection %d %s", done.Code, done.Body)
	}
	var finished map[string]string
	json.Unmarshal(done.Body.Bytes(), &finished)
	if finished["inbox_id"] == "" {
		t.Fatal("no inbox returned")
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 1 || records[0]["operation"] != "microsoft_connect" || records[0]["source"] != "ui" || records[0]["user_id"] != u.ID || records[0]["inbox_id"] != finished["inbox_id"] {
		t.Fatalf("shared Microsoft attribution: %s", logs.String())
	}
	if rr := post("/ui/oauth/microsoft/finish", url.Values{"state": {choice.State}, "mailbox": {"shared@example.net"}}); rr.Code != 400 {
		t.Fatal("selection replay accepted")
	}
	c, e := svc.Store.MicrosoftConnection(context.Background(), u.AccountID, finished["inbox_id"])
	if e != nil || !c.Shared || c.Mailbox != "shared@example.net" {
		t.Fatalf("shared connection %#v %v", c, e)
	}
	box, e := svc.Store.GetInbox(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, finished["inbox_id"])
	if e != nil || box.DisplayName != "Shared Team" {
		t.Fatalf("typed label not kept: %#v %v", box, e)
	}
}
