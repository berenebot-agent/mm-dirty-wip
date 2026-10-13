package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/apispec"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

const contractMIME = "Date: Sat, 10 Oct 2026 09:59:00 +0000\r\nFrom: sender@example.test\r\nTo: agent@remote.test\r\nCc: other@example.test\r\nSubject: Contract\r\nMessage-ID: <contract@example.test>\r\nIn-Reply-To: <parent@example.test>\r\nReferences: <parent@example.test>\r\nContent-Type: text/html\r\n\r\n<p>contract body</p><script>alert(1)</script>"

// Exercise actual HTTP routes and connector adapters, not private projection
// functions. The same OpenAPI contract validates every backend's responses.
func TestMessageContractAcrossInboxKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"domain", "imap", "google", "microsoft", "one"} {
		t.Run(kind, func(t *testing.T) {
			svc, h, u, domainBox, box, fake := standaloneFixture(t)
			ctx := context.Background()
			p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
			var id, folder string
			if kind == "domain" {
				box = domainBox
				m, _, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
					Inbox: box, Provider: "mailgun", ProviderDeliveryID: "contract",
					RFCMessageID: "<contract@example.test>", InReplyTo: "<parent@example.test>", References: []string{"<parent@example.test>"},
					From: model.Address{Address: "sender@example.test"}, To: []string{box.Address},
					Subject: "Contract", Text: "contract body", HTML: `<p>contract body</p><script>alert(1)</script>`,
					RawPath: "messages/contract.eml", SizeBytes: 100, ReceivedAt: time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC),
				})
				if err != nil {
					t.Fatal(err)
				}
				id = m.ID
			} else if kind == "imap" {
				fake.addAt("INBOX", contractMIME, "<contract@example.test>", "Contract", time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC))
				folder = "INBOX"
			} else {
				rm := app.NewRemoteMailboxService(svc)
				t.Cleanup(rm.Stop)
				rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
				if kind == "one" {
					f := onefixture.New(t)
					rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
					setup, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
					if err != nil {
						t.Fatal(err)
					}
					results, err := rm.ImportOne(ctx, p, setup, []string{onefixture.ConnectionID}, nil)
					if err != nil || len(results) != 1 || results[0].InboxID == "" {
						t.Fatalf("import: %+v %v", results, err)
					}
					box, err = svc.Store.GetInboxInternal(ctx, u.AccountID, results[0].InboxID)
					if err != nil {
						t.Fatal(err)
					}
					folder = "INBOX"
				} else {
					var err error
					box, err = svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: kind + "@example.test"})
					if err != nil {
						t.Fatal(err)
					}
					fixture := contractConnectorFixture(t, kind)
					if kind == "google" {
						rm.Google = gmail.NewClient(fixture.Client())
						rm.Google.BaseURL = fixture.URL
						err = rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope})
						folder = "INBOX"
					} else {
						rm.Microsoft = microsoft.NewClient(fixture.Client())
						rm.Microsoft.BaseURL = fixture.URL
						err = rm.SaveMicrosoftGrant(ctx, store.MicrosoftConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: box.Address}, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)})
						folder = "inbox-id"
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				srv := httpapp.New(svc, nil)
				srv.SetRemoteMailbox(rm)
				h = settledActionFixture(t, svc, rm, srv.Handler(), u.AccountID)
			}
			_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "contract", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			get := func(path string) map[string]any {
				t.Helper()
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer "+key)
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != 200 {
					t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
				}
				var out any
				if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if rows, ok := out.([]any); ok {
					return map[string]any{"messages": rows}
				}
				return out.(map[string]any)
			}
			page := get("/v1/inboxes/" + box.ID + "/messages?folder=" + url.QueryEscape(folder))
			items, ok := page["items"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("expected one message: %+v", page)
			}
			item := items[0].(map[string]any)
			if id != "" && item["id"] != id {
				t.Fatalf("wrong message: %+v", item)
			}
			id = item["id"].(string)
			assertMessageSchema(t, item)
			if _, ok := item["text"]; ok {
				t.Fatal("list unexpectedly contains body")
			}
			if _, ok := item["html"]; ok {
				t.Fatal("list unexpectedly contains HTML")
			}
			if kind == "domain" {
				if item["inbox_kind"] != "domain" || item["provider"] != "mailmoose" || item["message_transport"] != "mailgun" || item["direction"] != "inbound" || item["is_spam"] != false || item["snippet"] != "contract body" {
					t.Fatalf("lost local metadata: %+v", item)
				}
			} else {
				if item["inbox_kind"] != "standalone" || item["provider"] != kind || item["message_transport"] != kind || item["direction"] != "inbound" {
					t.Fatalf("wrong identity metadata: %+v", item)
				}
				for _, unknown := range []string{"status", "created_at", "is_spam"} {
					if _, ok := item[unknown]; ok {
						t.Errorf("fabricated %s: %+v", unknown, item)
					}
				}
				for _, known := range []string{"message_id", "indexed_at", "flagged", "answered", "draft"} {
					if _, ok := item[known]; !ok {
						t.Errorf("lost %s: %+v", known, item)
					}
				}
				if kind == "imap" && (item["in_reply_to"] != "<parent@example.test>" || item["snippet"] != "-") {
					t.Fatalf("lost IMAP/thread metadata: %+v", item)
				}
				if kind == "imap" && item["sent_at"] != "2026-10-10T09:59:00Z" {
					t.Fatalf("lost known sent timestamp: %+v", item)
				}
			}
			for _, removed := range []string{"uid", "uid_validity", "inbox_provider"} {
				if _, ok := item[removed]; ok {
					t.Errorf("removed field remains: %s", removed)
				}
				if _, ok := apispec.Schemas()["Message"].(map[string]any)["properties"].(map[string]any)[removed]; ok {
					t.Errorf("removed field remains in OpenAPI: %s", removed)
				}
			}
			for _, path := range []string{
				"/v1/messages?inbox=" + box.ID + "&folder=" + url.QueryEscape(folder),
				"/v1/messages?folder=" + url.QueryEscape(folder),
				"/v1/inboxes/" + box.ID + "/search?folder=" + url.QueryEscape(folder) + "&q=" + url.QueryEscape(item["subject"].(string)),
				"/v1/search?inbox=" + box.ID + "&folder=" + url.QueryEscape(folder) + "&q=" + url.QueryEscape(item["subject"].(string)),
			} {
				page := get(path)
				found := false
				for _, row := range page["items"].([]any) {
					m := row.(map[string]any)
					if m["id"] == id {
						found = true
						assertMessageSchema(t, m)
						if !sameMessageSnapshot(item, m) {
							t.Errorf("route mismatch %s:\nscoped=%+v\ncommon=%+v", path, item, m)
						}
					}
				}
				if !found {
					t.Errorf("message missing from %s: %+v", path, page)
				}
			}
			detail := get("/v1/inboxes/" + box.ID + "/messages/" + id)
			commonDetail := get("/v1/messages/" + id)
			assertMessageSchema(t, detail)
			if !sameMessageSnapshot(detail, commonDetail) {
				t.Errorf("detail route mismatch:\n%+v\n%+v", detail, commonDetail)
			}
			if _, ok := detail["text"].(string); !ok {
				t.Error("detail missing text body")
			}
			if html, _ := detail["html"].(string); strings.Contains(html, "script") || strings.Contains(html, "alert(1)") {
				t.Errorf("unsafe HTML: %s", html)
			}
			if kind != "one" && !strings.Contains(detail["html"].(string), "contract body") {
				t.Error("detail lost benign body content")
			}
			delete(detail, "text")
			delete(detail, "html")
			if !sameMessageSnapshot(item, detail) {
				t.Errorf("detail lost or changed list metadata:\n%+v\n%+v", item, detail)
			}
			thread, _ := item["thread_id"].(string)
			if thread == "" {
				t.Fatal("fixture missing thread id")
			}
			for _, path := range []string{
				"/v1/inboxes/" + box.ID + "/threads/" + url.PathEscape(thread),
				"/v1/threads/" + url.PathEscape(thread) + "?inbox=" + box.ID,
				"/v1/threads/" + url.PathEscape(thread) + "/messages?inbox=" + box.ID,
			} {
				view := get(path)
				messages := view["messages"].([]any)
				if len(messages) != 1 || !sameMessageSnapshot(item, messages[0].(map[string]any)) {
					t.Errorf("thread route mismatch %s: %+v", path, view)
				}
			}
		})
	}
}

// Header refreshes legitimately advance indexed_at between sequential requests.
// Its presence must match; all other values must be identical for this fixture.
func sameMessageSnapshot(a, b map[string]any) bool {
	copyWithoutIndex := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(m))
		for key, value := range m {
			if key == "indexed_at" {
				out[key] = "known indexing time"
			} else {
				out[key] = value
			}
		}
		return out
	}
	return reflect.DeepEqual(copyWithoutIndex(a), copyWithoutIndex(b))
}

func assertMessageSchema(t *testing.T, m map[string]any) {
	t.Helper()
	schema := apispec.Schemas()["Message"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	for _, field := range schema["required"].([]string) {
		if _, ok := m[field]; !ok {
			t.Errorf("missing required %s", field)
		}
	}
	for field, value := range m {
		property, ok := properties[field].(map[string]any)
		if !ok {
			t.Errorf("undeclared field %s", field)
			continue
		}
		valid := true
		switch property["type"] {
		case "string":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "integer":
			n, ok := value.(float64)
			valid = ok && n == float64(int64(n))
		case "array":
			_, valid = value.([]any)
		case "object":
			_, valid = value.(map[string]any)
		}
		if !valid {
			t.Errorf("%s violates schema type: %#v", field, value)
		}
		if property["format"] == "date-time" {
			s, ok := value.(string)
			if _, err := time.Parse(time.RFC3339Nano, s); !ok || err != nil {
				t.Errorf("invalid timestamp %s: %#v", field, value)
			}
		}
	}
}

func contractConnectorFixture(t *testing.T, kind string, nativeLabels ...string) *httptest.Server {
	t.Helper()
	labels := []string{"INBOX", "STARRED"}
	if len(nativeLabels) > 0 {
		labels = nativeLabels
	}
	var fixture *httptest.Server
	fixture = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if kind == "google" {
			switch {
			case strings.HasSuffix(r.URL.Path, "/profile"):
				json.NewEncoder(w).Encode(map[string]string{"emailAddress": "google@example.test", "historyId": "10"})
			case strings.HasSuffix(r.URL.Path, "/labels"):
				w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"SENT","name":"SENT","type":"system"},{"id":"DRAFT","name":"DRAFT","type":"system"}]}`))
			case strings.HasSuffix(r.URL.Path, "/history"):
				w.Write([]byte(`{"historyId":"10"}`))
			case strings.HasSuffix(r.URL.Path, "/messages"):
				w.Write([]byte(`{"messages":[{"id":"a","threadId":"th"}]}`))
			case strings.HasSuffix(r.URL.Path, "/messages/a"):
				json.NewEncoder(w).Encode(map[string]any{"id": "a", "threadId": "th", "labelIds": labels, "internalDate": "1700000000000", "snippet": "contract preview", "raw": base64.RawURLEncoding.EncodeToString([]byte(contractMIME)), "payload": map[string]any{"headers": []map[string]string{{"name": "Subject", "value": "Contract"}, {"name": "From", "value": "sender@example.test"}, {"name": "Message-ID", "value": "<contract@example.test>"}, {"name": "In-Reply-To", "value": "<parent@example.test>"}, {"name": "References", "value": "<parent@example.test>"}}}})
			default:
				t.Errorf("unexpected Google request: %s", r.URL)
				w.WriteHeader(404)
			}
			return
		}
		message := map[string]any{"id": "a", "conversationId": "th", "parentFolderId": "inbox-id", "subject": "Contract", "internetMessageId": "<contract@example.test>", "receivedDateTime": "2026-10-10T10:00:00Z", "sentDateTime": "2026-10-10T09:59:00Z", "bodyPreview": "contract preview", "isRead": false, "isDraft": false, "flag": map[string]string{"flagStatus": "flagged"}, "from": map[string]any{"emailAddress": map[string]string{"address": "sender@example.test"}}}
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/a/$value"):
			w.Write([]byte(contractMIME))
		case strings.HasSuffix(r.URL.Path, "/messages/a"):
			json.NewEncoder(w).Encode(message)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			json.NewEncoder(w).Encode(map[string]any{"value": []any{message}})
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			json.NewEncoder(w).Encode(map[string]any{"value": []any{message}, "@odata.deltaLink": fixture.URL + r.URL.Path + "?cursor=next"})
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			w.Write([]byte(`{"value":[{"id":"inbox-id","displayName":"Inbox"}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			w.Write([]byte(`{"id":"inbox-id","displayName":"Inbox"}`))
		case strings.Contains(r.URL.Path, "/mailFolders/"):
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected Microsoft request: %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(fixture.Close)
	return fixture
}
