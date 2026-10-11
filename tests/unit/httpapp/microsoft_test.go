package httpapp_test

import (
	"context"
	"encoding/json"
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
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

func TestMicrosoftMailboxMovesAndPermissions(t *testing.T) {
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
	svc, _, u, _, _, _ := standaloneFixture(t)
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
			w.Write([]byte(`{"id":"identity","mail":"personal@example.net"}`))
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
}
