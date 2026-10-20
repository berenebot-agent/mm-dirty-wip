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
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func TestGoogleOAuthIdentityNaming(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, identity, override, existing, want string
		status                                   int
	}{
		{"profile", `{"name":" Google Person "}`, "", "", "Google Person", 200},
		{"override", `{"name":"Google Person"}`, " My label ", "", "My label", 200},
		{"missing", `{}`, "", "", "oauth@example.net", 200},
		{"blank", `{"name":"  "}`, "", "", "oauth@example.net", 200},
		{"declined", `{}`, "", "", "oauth@example.net", 403},
		{"unavailable", `{}`, "", "", "oauth@example.net", 503},
		{"reconnect fallback", `{"name":"Google Person"}`, "", "oauth@example.net", "Google Person", 200},
		{"reconnect custom", `{"name":"Google Person"}`, "", "Custom label", "Custom label", 200},
		{"reconnect unavailable", `{}`, "", "oauth@example.net", "oauth@example.net", 503},
		{"setup rollback", `{}`, "", "", "oauth@example.net", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, u, _, _, _ := standaloneFixture(t)
			svc.Config.BaseURL = "https://mail.example.com"
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/token":
					// Only Gmail is required, even if identity permission was declined.
					if tc.name == "setup rollback" {
						w.Write([]byte(`{"access_token":"private-access-token","expires_in":3600,"scope":"https://www.googleapis.com/auth/gmail.modify","token_type":"Bearer"}`))
						return
					}
					w.Write([]byte(`{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"https://www.googleapis.com/auth/gmail.modify","token_type":"Bearer"}`))
				case r.URL.Path == "/userinfo":
					bearer := "Bearer access"
					if tc.name == "setup rollback" {
						bearer = "Bearer private-access-token"
					}
					if r.Header.Get("Authorization") != bearer {
						t.Error("missing identity bearer")
					}
					w.WriteHeader(tc.status)
					w.Write([]byte(tc.identity))
				case strings.HasSuffix(r.URL.Path, "/profile"):
					w.Write([]byte(`{"emailAddress":"oauth@example.net","historyId":"10"}`))
				case strings.HasSuffix(r.URL.Path, "/labels"):
					w.Write([]byte(`{"labels":[]}`))
				case strings.HasSuffix(r.URL.Path, "/history"):
					w.Write([]byte(`{"historyId":"10"}`))
				default:
					w.Write([]byte(`{"messages":[]}`))
				}
			}))
			defer fixture.Close()
			rm := app.NewRemoteMailboxService(svc)
			defer rm.Stop()
			rm.Google = gmail.NewClient(fixture.Client())
			rm.Google.BaseURL = fixture.URL
			rm.Google.TokenURL = fixture.URL + "/token"
			rm.Google.UserInfoURL = fixture.URL + "/userinfo"
			inbox := ""
			if tc.existing != "" {
				box, err := svc.Store.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{Address: "oauth@example.net", DisplayName: tc.existing})
				if err != nil {
					t.Fatal(err)
				}
				inbox = box.ID
				if err = rm.SaveGoogleGrant(context.Background(), u.AccountID, inbox, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
					t.Fatal(err)
				}
			}
			srv := httpapp.New(svc, nil)
			var logs bytes.Buffer
			svc.Store.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
			srv.SetRemoteMailbox(rm)
			token, csrf, err := svc.Store.CreateSession(context.Background(), u.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			post := func(path string, values url.Values) *httptest.ResponseRecorder {
				values.Set("_csrf", csrf)
				r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, r)
				return rr
			}
			begin := post("/ui/oauth/google/begin", url.Values{"client_id": {"client"}, "client_secret": {"secret"}, "display": {tc.override}, "inbox_id": {inbox}})
			if begin.Code != 200 {
				t.Fatalf("begin %d %s", begin.Code, begin.Body.String())
			}
			var result map[string]string
			if err = json.Unmarshal(begin.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			auth, err := url.Parse(result["authorization_url"])
			if err != nil {
				t.Fatal(err)
			}
			q := auth.Query()
			if q.Get("scope") != gmail.AuthorizationScopes {
				t.Fatalf("scopes %q", q.Get("scope"))
			}
			callback := q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"once"}}.Encode()
			finish := post("/ui/oauth/google/finish", url.Values{"callback_url": {callback}})
			records := lifecycleRecords(t, &logs)
			if tc.name == "setup rollback" {
				if finish.Code != 400 || len(records) != 2 || records[0]["msg"] != "inbox.created" || records[1]["msg"] != "inbox.deleted" || records[1]["reason"] != "setup_rollback" {
					t.Fatalf("rollback %d logs=%s", finish.Code, logs.String())
				}
			} else if tc.existing == "" {
				if len(records) != 1 || records[0]["msg"] != "inbox.created" {
					t.Fatalf("creation logs: %s", logs.String())
				}
			} else if len(records) != 0 {
				t.Fatalf("reconnect logged creation: %s", logs.String())
			}
			for _, record := range records {
				if record["source"] != "ui" || record["operation"] != "google_connect" || record["user_id"] != u.ID {
					t.Fatalf("Google attribution: %v", record)
				}
			}
			for _, private := range []string{"private-access-token", "refresh", "secret", token, csrf, q.Get("state")} {
				if strings.Contains(logs.String(), private) {
					t.Fatalf("OAuth credential leaked: %q", private)
				}
			}
			if tc.name == "setup rollback" {
				return
			}
			if finish.Code != 200 {
				t.Fatalf("finish %d %s", finish.Code, finish.Body.String())
			}
			boxes, err := svc.Store.ListStandaloneInboxes(context.Background(), u.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, box := range boxes {
				if box.Address != "oauth@example.net" {
					continue
				}
				found++
				if box.DisplayName != tc.want || (inbox != "" && box.ID != inbox) {
					t.Fatalf("inbox %#v, want name %q", box, tc.want)
				}
			}
			if found != 1 {
				t.Fatalf("found %d connected inboxes", found)
			}
		})
	}
}
