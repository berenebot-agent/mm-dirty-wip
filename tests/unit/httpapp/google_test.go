package httpapp_test

import (
	"context"
	"encoding/json"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func TestGoogleCacheStableLabelsAndScopedAccess(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	box, e := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "google@example.net"})
	if e != nil {
		t.Fatal(e)
	}
	labels := []string{"INBOX", "UNREAD", "Label_1"}
	history := "10"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("missing bearer")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			json.NewEncoder(w).Encode(map[string]string{"emailAddress": "google@example.net", "historyId": history})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"SENT","name":"SENT","type":"system"},{"id":"DRAFT","name":"DRAFT","type":"system"},{"id":"TRASH","name":"TRASH","type":"system"},{"id":"SPAM","name":"SPAM","type":"system"},{"id":"Label_1","name":"Projects","type":"user"}]}`))
		case strings.HasSuffix(r.URL.Path, "/history"):
			json.NewEncoder(w).Encode(map[string]any{"historyId": history, "history": []any{map[string]any{"id": history, "labelsRemoved": []any{map[string]any{"message": map[string]string{"id": "a"}}}}}})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Write([]byte(`{"messages":[{"id":"a","threadId":"th"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/a/modify"):
			var v struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			json.NewDecoder(r.Body).Decode(&v)
			for _, remove := range v.Remove {
				var next []string
				for _, l := range labels {
					if l != remove {
						next = append(next, l)
					}
				}
				labels = next
			}
			labels = append(labels, v.Add...)
			w.Write([]byte(`{"id":"a"}`))
		case strings.HasSuffix(r.URL.Path, "/messages/a"):
			json.NewEncoder(w).Encode(map[string]any{"id": "a", "threadId": "th", "labelIds": labels, "internalDate": "1700000000000", "snippet": "preview", "payload": map[string]any{"headers": []any{map[string]string{"name": "Subject", "value": "Hello"}, map[string]string{"name": "From", "value": "Sender <sender@example.net>"}}}})
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	if e = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); e != nil {
		t.Fatal(e)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	page, e := rm.ListRemoteMessages(ctx, p, box.ID, "INBOX", 50, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(page.Items) != 1 || page.Items[0].Subject != "Hello" {
		t.Fatalf("page %#v", page)
	}
	id := page.Items[0].ID
	other, e := rm.ListRemoteMessages(ctx, p, box.ID, "Label_1", 50, "")
	if e != nil || len(other.Items) != 1 || other.Items[0].ID != id {
		t.Fatalf("label membership %#v %v", other, e)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	catalog := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/label-catalog", adminKey(t, svc, u))
	if catalog.Code != http.StatusOK || strings.Count(catalog.Body.String(), `"name":"Projects"`) != 1 ||
		!strings.Contains(catalog.Body.String(), `"membership":"multiple"`) {
		t.Fatalf("Gmail label should be additive and deduplicated: %d %s", catalog.Code, catalog.Body.String())
	}
	// A nonexistent Gmail label must agree with the cached listing and never
	// leak the provider-specific messages.list error through the search API.
	// This fixture deliberately returns a message even for an invalid label,
	// so the test also verifies that the invalid query is not sent upstream.
	key := adminKey(t, svc, u)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/search?q=Hello&folder=NoSuchFolder",
		"/v1/search?inbox=" + box.ID + "&q=Hello&folder=NoSuchFolder",
		"/v1/inboxes/" + box.ID + "/messages?folder=NoSuchFolder",
	} {
		rr := apiGet(t, srv.Handler(), path, key)
		var env struct {
			Items []json.RawMessage `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &env) != nil || len(env.Items) != 0 {
			t.Fatalf("unknown Google folder %s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	valid := apiGet(t, srv.Handler(), "/v1/inboxes/"+box.ID+"/search?q=Hello&folder=INBOX", key)
	var validEnv struct {
		Items []json.RawMessage `json:"items"`
	}
	if valid.Code != http.StatusOK || json.Unmarshal(valid.Body.Bytes(), &validEnv) != nil || len(validEnv.Items) != 1 {
		t.Fatalf("valid Google folder search must still reach provider: %d %s", valid.Code, valid.Body.String())
	}
	if _, e = rm.MoveRemoteMessage(ctx, p, box.ID, id, "ARCHIVE"); e != nil {
		t.Fatal(e)
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "INBOX", 50, "")
	if e != nil || len(page.Items) != 0 {
		t.Fatalf("archived inbox %#v %v", page, e)
	}
	page, e = rm.ListRemoteMessages(ctx, p, box.ID, "ARCHIVE", 50, "")
	if e != nil || len(page.Items) != 1 || page.Items[0].ID != id {
		t.Fatalf("archive %#v %v", page, e)
	}
	if _, e = rm.GetRemoteMessage(ctx, model.Principal{AccountID: u.AccountID}, box.ID, id); e == nil {
		t.Fatal("unauthorized read succeeded")
	}
	if _, e = rm.SetRemoteRead(ctx, model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}, box.ID, id, true); e == nil {
		t.Fatal("reader modified message")
	}
	c, e := svc.Store.GoogleConnection(ctx, u.AccountID, box.ID)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(c.EncryptedToken, "refresh") || strings.Contains(c.EncryptedSecret, "secret") {
		t.Fatal("plaintext credentials")
	}
}

func TestGoogleOAuthPasteExpiryAndReplay(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	svc.Config.BaseURL = "https://mail.example.com"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			r.ParseForm()
			if r.Form.Get("code_verifier") == "" {
				t.Error("missing PKCE verifier")
			}
			w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/gmail.modify","token_type":"Bearer"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/profile") {
			w.Write([]byte(`{"emailAddress":"oauth@example.net","historyId":"10"}`))
			return
		}
		if r.URL.Path == "/userinfo" {
			w.Write([]byte(`{}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/labels") {
			w.Write([]byte(`{"labels":[]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/history") {
			w.Write([]byte(`{"historyId":"10"}`))
			return
		}
		w.Write([]byte(`{"messages":[]}`))
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	rm.Google.TokenURL = fixture.URL + "/token"
	rm.Google.UserInfoURL = fixture.URL + "/userinfo"
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	token, csrf, e := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	begin := post("/ui/oauth/google/begin", url.Values{"client_id": {"client"}, "client_secret": {"secret"}})
	if begin.Code != 200 {
		t.Fatalf("begin %d %s", begin.Code, begin.Body.String())
	}
	var result map[string]string
	json.Unmarshal(begin.Body.Bytes(), &result)
	auth, e := url.Parse(result["authorization_url"])
	if e != nil {
		t.Fatal(e)
	}
	q := auth.Query()
	if q.Get("scope") != gmail.AuthorizationScopes {
		t.Fatalf("unexpected scopes: %s", q.Get("scope"))
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("access_type") != "offline" {
		t.Fatal("missing security/offline parameters")
	}
	callback := q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"once"}}.Encode()
	wrong := post("/ui/oauth/google/finish", url.Values{"callback_url": {"https://attacker.test/callback?code=once&state=" + q.Get("state")}})
	if wrong.Code != 400 {
		t.Fatal("accepted wrong redirect")
	}
	denied := post("/ui/oauth/google/finish", url.Values{"callback_url": {q.Get("redirect_uri") + "?error=access_denied&state=" + q.Get("state")}})
	if denied.Code != 400 || !strings.Contains(denied.Body.String(), "denied access") {
		t.Fatalf("access_denied not handled: %d %s", denied.Code, denied.Body.String())
	}
	finish := post("/ui/oauth/google/finish", url.Values{"callback_url": {callback}})
	if finish.Code != 200 {
		t.Fatalf("finish %d %s", finish.Code, finish.Body.String())
	}
	replay := post("/ui/oauth/google/finish", url.Values{"callback_url": {callback}})
	if replay.Code != 400 {
		t.Fatal("accepted callback replay")
	}
	boxes, e := svc.Store.ListStandaloneInboxes(context.Background(), u.AccountID)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, b := range boxes {
		if b.Address == "oauth@example.net" {
			found = true
			if b.DisplayName != "oauth@example.net" {
				t.Fatalf("label not defaulted from the address: %#v", b)
			}
		}
	}
	if !found {
		t.Fatal("verified Google inbox missing")
	}
	if e = svc.Store.SaveGoogleAttempt(context.Background(), store.GoogleAttempt{StateHash: "expired", AccountID: u.AccountID, UserID: u.ID, ClientID: "client", ExpiresAt: time.Now().Add(-time.Minute)}); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Store.ConsumeGoogleAttempt(context.Background(), "expired", u.AccountID, u.ID); e == nil {
		t.Fatal("expired attempt consumed")
	}
}

// TestGoogleReconnectReusesStoredAppCredentials proves reconnecting an existing
// Gmail inbox does not make the operator retype its Google app credentials: the
// wizard prefills the client ID, a blank client secret is taken from the stored
// connection, and a pasted secret for a different client ID is refused.
func TestGoogleReconnectReusesStoredAppCredentials(t *testing.T) {
	t.Parallel()
	svc, _, u, _, _, _ := standaloneFixture(t)
	svc.Config.BaseURL = "https://mail.example.com"
	ctx := context.Background()
	box, e := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "reconnect@example.net"})
	if e != nil {
		t.Fatal(e)
	}
	var exchange url.Values
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/token":
			r.ParseForm()
			exchange = r.PostForm
			w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/gmail.modify","token_type":"Bearer"}`))
		case r.URL.Path == "/userinfo":
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/profile"):
			w.Write([]byte(`{"emailAddress":"reconnect@example.net","historyId":"10"}`))
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.Write([]byte(`{"labels":[]}`))
		default:
			w.Write([]byte(`{"historyId":"10","messages":[]}`))
		}
	}))
	defer fixture.Close()
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	rm.Google.TokenURL = fixture.URL + "/token"
	rm.Google.UserInfoURL = fixture.URL + "/userinfo"
	if e = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); e != nil {
		t.Fatal(e)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	token, csrf, e := svc.Store.CreateSession(ctx, u.ID, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	cookie := &http.Cookie{Name: "mmm_session", Value: token}
	post := func(v url.Values) *httptest.ResponseRecorder {
		v.Set("_csrf", csrf)
		r := httptest.NewRequest("POST", "/ui/oauth/google/begin", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}

	page := httptest.NewRequest("GET", "/ui/inboxes/standalone/new?google_inbox="+box.ID, nil)
	page.AddCookie(cookie)
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, page)
	if pr.Code != 200 || !strings.Contains(pr.Body.String(), `name="client_id" value="client" required`) ||
		!strings.Contains(pr.Body.String(), `name="client_secret" type="password" autocomplete="new-password" placeholder="Saved`) {
		t.Fatalf("reconnect wizard did not prefill the stored client ID: %d %s", pr.Code, pr.Body.String())
	}

	wrong := post(url.Values{"inbox_id": {box.ID}, "client_id": {"other"}})
	if wrong.Code != 400 || !strings.Contains(wrong.Body.String(), "client secret for this client ID") {
		t.Fatalf("blank secret reused for a different client ID: %d %s", wrong.Code, wrong.Body.String())
	}
	begin := post(url.Values{"inbox_id": {box.ID}})
	if begin.Code != 200 {
		t.Fatalf("reconnect begin %d %s", begin.Code, begin.Body.String())
	}
	var result map[string]string
	json.Unmarshal(begin.Body.Bytes(), &result)
	auth, e := url.Parse(result["authorization_url"])
	if e != nil {
		t.Fatal(e)
	}
	if q := auth.Query(); q.Get("client_id") != "client" || q.Get("state") == "" {
		t.Fatalf("authorization URL missing stored client ID: %s", auth)
	}
	callback := auth.Query().Get("redirect_uri") + "?" + url.Values{"state": {auth.Query().Get("state")}, "code": {"once"}}.Encode()
	finish := httptest.NewRequest("POST", "/ui/oauth/google/finish", strings.NewReader(url.Values{"_csrf": {csrf}, "callback_url": {callback}}.Encode()))
	finish.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	finish.AddCookie(cookie)
	fr := httptest.NewRecorder()
	h.ServeHTTP(fr, finish)
	if fr.Code != 200 {
		t.Fatalf("reconnect finish %d %s", fr.Code, fr.Body.String())
	}
	if exchange.Get("client_id") != "client" || exchange.Get("client_secret") != "secret" {
		t.Fatalf("stored client secret was not used for the exchange: %v", exchange)
	}
}
