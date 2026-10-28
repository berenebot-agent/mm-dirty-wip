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

	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/internal/model"
)

func lifecycleRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["msg"] == "inbox.created" || r["msg"] == "inbox.deleted" {
			records = append(records, r)
		}
	}
	return records
}

func TestInboxTerminalLogsAPIAttribution(t *testing.T) {
	t.Parallel()
	svc, _, u, d, _ := httpFixture(t)
	ctx := context.Background()
	client, token, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "private-key-label", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	svc.Store.SetInboxLogger(log)
	h := httpapp.New(svc, log).Handler()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", "private-user-agent")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	body := `{"domain_id":"` + d.ID + `","local_part":"logged-api","display_name":"private-display"}`
	created := request("POST", "/v1/inboxes", body)
	if created.Code != 201 {
		t.Fatalf("create %d %s", created.Code, created.Body.String())
	}
	var box model.Inbox
	if err := json.Unmarshal(created.Body.Bytes(), &box); err != nil {
		t.Fatal(err)
	}
	if rr := request("POST", "/v1/inboxes", body); rr.Code == 201 {
		t.Fatal("duplicate created")
	}
	if rr := request("DELETE", "/v1/inboxes/"+box.ID, ""); rr.Code != 200 && rr.Code != 204 {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	records := lifecycleRecords(t, &logs)
	if len(records) != 2 {
		t.Fatalf("events: %s", logs.String())
	}
	for _, r := range records {
		if r["source"] != "api" || r["actor_type"] != "api_key" || r["api_key_id"] != client.ID || r["account_id"] != u.AccountID || r["inbox_id"] != box.ID {
			t.Fatalf("attribution: %v", r)
		}
	}
	for _, secret := range []string{token, "private-key-label", "private-display", "private-user-agent"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("private field leaked: %q", secret)
		}
	}
}

func TestInboxTerminalLogsUISessionAttribution(t *testing.T) {
	t.Parallel()
	for _, keySession := range []bool{false, true} {
		name := "user"
		if keySession {
			name = "api_key"
		}
		t.Run(name, func(t *testing.T) {
			svc, _, u, d, existing := httpFixture(t)
			ctx := context.Background()
			var token, csrf string
			var err error
			if keySession {
				client, _, e := svc.Store.CreateAPIKey(ctx, u.AccountID, "private-label", false, map[string]string{existing.ID: "owner"})
				if e != nil {
					t.Fatal(e)
				}
				token, csrf, err = svc.Store.CreateKeySession(ctx, client.ID, time.Hour)
			} else {
				token, csrf, err = svc.Store.CreateSession(ctx, u.ID, time.Hour)
			}
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			svc.Store.SetInboxLogger(log)
			h := httpapp.New(svc, log).Handler()
			form := url.Values{"_csrf": {csrf}, "domain": {d.ID}, "local": {"logged-ui"}, "display": {"private-display"}}
			r := httptest.NewRequest("POST", "/ui/inboxes", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
			if keySession {
				// Browser key sessions cannot create inboxes. Do not weaken that
				// boundary just to exercise lifecycle attribution.
				if rr.Code != 403 || len(lifecycleRecords(t, &logs)) != 0 {
					t.Fatalf("scoped key creation: %d %s logs=%s", rr.Code, rr.Body.String(), logs.String())
				}
				return
			}
			if rr.Code != 303 {
				t.Fatalf("create %d %s", rr.Code, rr.Body.String())
			}
			records := lifecycleRecords(t, &logs)
			if len(records) != 1 || records[0]["source"] != "ui" || records[0]["actor_type"] != name {
				t.Fatalf("attribution: %s", logs.String())
			}
			if records[0]["user_id"] != u.ID {
				t.Fatal("user identity missing")
			}
			id := records[0]["inbox_id"].(string)
			for _, confirm := range []string{"wrong confirmation", "logged-ui@example.com"} {
				form := url.Values{"_csrf": {csrf}, "confirm": {confirm}}
				r := httptest.NewRequest("POST", "/ui/inboxes/"+id+"/delete", strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, r)
				if rr.Code != 303 {
					t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
				}
				records = lifecycleRecords(t, &logs)
				if confirm == "wrong confirmation" && len(records) != 1 {
					t.Fatal("failed confirmation logged deletion")
				}
			}
			if len(records) != 2 || records[1]["msg"] != "inbox.deleted" || records[1]["source"] != "ui" || records[1]["user_id"] != u.ID || records[1]["address"] != "logged-ui@example.com" {
				t.Fatalf("UI deletion attribution: %s", logs.String())
			}
			for _, secret := range []string{token, csrf, "private-display"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("private field leaked: %q", secret)
				}
			}
		})
	}
}
