// Package onefixture provides a deterministic One/Gmail HTTP fixture. It never
// contacts One and uses only synthetic credentials and email content.
package onefixture

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport/one"
)

const Key = "fixture-one-key-never-live"
const Selector = "fixture-one-selector"
const ConnectionID = "fixture-one-uuid"
const Email = "gmail-one@example.test"
const Name = "Fixture Gmail Owner"

type Fixture struct {
	mu           sync.Mutex
	server       *httptest.Server
	Calls        []string
	Actions      []one.Action
	Connections  []map[string]any
	Status       map[string]int
	NativeErrors bool
	LabelIDs     []string
	Messages     []map[string]string
	History      map[string]any
	DraftRaw     string
	SendCalls    int
	Keys         []string
	SenderNames  map[string]string
	SentFrom     string
	// ProfileTotal is returned as messagesTotal by the fixture /profile call,
	// exercising the import screen's best-effort count.
	ProfileTotal int
	// ListQuery records the q parameter of the last /messages list call, so
	// tests can assert the history horizon is applied.
	ListQuery string
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func New(t *testing.T) *Fixture {
	t.Helper()
	f := &Fixture{Status: make(map[string]int), LabelIDs: []string{"INBOX", "UNREAD"}, History: map[string]any{"historyId": "20"}}
	f.SenderNames = map[string]string{Email: Name}
	f.Connections = []map[string]any{
		{"id": ConnectionID, "key": Selector, "platform": "gmail", "active": true, "state": "operational", "environment": "live", "identity": "opaque-not-an-email"},
		{"id": "fixture-outlook", "key": "fixture-outlook-selector", "platform": "outlook", "active": true, "state": "operational", "environment": "live"},
		{"id": "fixture-inactive", "key": "fixture-inactive-selector", "platform": "gmail", "active": false, "state": "revoked", "environment": "live"},
	}
	f.Actions = one.Operations()
	f.Actions = append(f.Actions, one.Action{Method: "GET", Path: "/gmail/v1/users/{{userId}}/settings/sendAs"})
	for i := range f.Actions {
		f.Actions[i].ID = f.Actions[i].Method + "-" + f.Actions[i].Path
	}
	f.Messages = []map[string]string{{"id": "message-one", "threadId": "thread-one"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		f.Calls = append(f.Calls, call)
		w.Header().Set("Content-Type", "application/json")
		accepted := r.Header.Get("X-One-Secret") == Key
		for _, key := range f.Keys {
			accepted = accepted || r.Header.Get("X-One-Secret") == key
		}
		if !accepted || r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "error", "message": "rejected " + r.Header.Get("X-One-Secret")})
			return
		}
		if status := f.Status[call]; status != 0 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(status)
			if f.NativeErrors {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "status": "NOT_FOUND", "message": "sensitive fixture message"}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"key": "error", "message": "sensitive fixture message " + Key})
			}
			return
		}
		switch r.URL.Path {
		case "/v1/vault/connections":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": 1, "pages": 1, "rows": f.Connections})
			return
		case "/v1/available-actions/gmail":
			_ = json.NewEncoder(w).Encode(map[string]any{"page": 1, "pages": 1, "rows": f.Actions})
			return
		}
		selectorOK := false
		profileEmail := Email
		for _, connection := range f.Connections {
			if connection["key"] == r.Header.Get("X-One-Connection-Key") {
				selectorOK = true
				if email, ok := connection["email"].(string); ok {
					profileEmail = email
				}
			}
		}
		if !selectorOK || r.Header.Get("X-One-Action-Id") == "" {
			t.Error("missing server-resolved selector/action")
			w.WriteHeader(403)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/passthrough/gmail/v1/users/me")
		switch path {
		case "/settings/sendAs":
			_ = json.NewEncoder(w).Encode(map[string]any{"sendAs": []any{map[string]string{"sendAsEmail": "other-alias@example.test", "displayName": "Wrong alias name"}, map[string]string{"sendAsEmail": profileEmail, "displayName": f.SenderNames[profileEmail]}}})
		case "/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": profileEmail, "historyId": "10", "messagesTotal": f.ProfileTotal})
		case "/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []any{map[string]string{"id": "INBOX", "name": "INBOX", "type": "system"}, map[string]string{"id": "DRAFT", "name": "DRAFT", "type": "system"}, map[string]string{"id": "SENT", "name": "SENT", "type": "system"}}})
		case "/messages":
			f.ListQuery = r.URL.Query().Get("q")
			if r.URL.Query().Get("labelIds") == "SENT" && f.SentFrom != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "sent-name"}}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": f.Messages})
		case "/messages/sent-name":
			if r.URL.Query().Get("format") != "metadata" && (r.URL.Query().Get("fields") == "" || strings.Contains(r.URL.Query().Get("fields"), "data")) {
				t.Error("naming fetched more than metadata")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sent-name", "payload": map[string]any{"headers": []map[string]string{{"name": "From", "value": f.SentFrom}}}})
		case "/history":
			_ = json.NewEncoder(w).Encode(f.History)
		case "/messages/message-one/modify":
			var v struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			for _, removed := range v.Remove {
				for i := len(f.LabelIDs) - 1; i >= 0; i-- {
					if f.LabelIDs[i] == removed {
						f.LabelIDs = append(f.LabelIDs[:i], f.LabelIDs[i+1:]...)
					}
				}
			}
			f.LabelIDs = append(f.LabelIDs, v.Add...)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "message-one"})
		case "/messages/send":
			f.SendCalls++
			var v struct {
				Raw string `json:"raw"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			if raw, err := base64.RawURLEncoding.DecodeString(v.Raw); err != nil || !strings.Contains(string(raw), "Subject:") {
				t.Error("native MIME send payload invalid")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "sent-one"})
		case "/drafts":
			if r.Method == "POST" {
				var v struct {
					Message struct {
						Raw string `json:"raw"`
					} `json:"message"`
				}
				_ = json.NewDecoder(r.Body).Decode(&v)
				f.DraftRaw = v.Message.Raw
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "draft-one", "message": map[string]string{"id": "draft-message"}})
			} else {
				_, _ = io.WriteString(w, `{"drafts":[]}`)
			}
		default:
			if strings.HasPrefix(path, "/messages/") {
				id := strings.TrimPrefix(path, "/messages/")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "threadId": "thread-one", "labelIds": f.LabelIDs, "internalDate": "1700000000000", "raw": base64.RawURLEncoding.EncodeToString([]byte("From: sender@example.test\r\nSubject: One fixture\r\nMessage-ID: <one-fixture@example.test>\r\n\r\nSynthetic fixture body")), "payload": map[string]any{"headers": []any{map[string]string{"name": "Subject", "value": "One fixture"}, map[string]string{"name": "Message-ID", "value": "<one-fixture@example.test>"}, map[string]string{"name": "From", "value": "sender@example.test"}}}})
			} else {
				t.Errorf("unexpected operation %s", call)
				w.WriteHeader(404)
			}
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// Change serializes fixture mutations with HTTP handlers for race-enabled tests.
func (f *Fixture) Change(fn func(*Fixture)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

func (f *Fixture) Client(t *testing.T) *http.Client {
	t.Helper()
	base, _ := url.Parse(f.server.URL)
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.withone.ai" {
			t.Error("credential destination is not fixed One HTTPS origin")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return f.server.Client().Transport.RoundTrip(copy)
	})}
}
