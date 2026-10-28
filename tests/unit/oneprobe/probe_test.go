package oneprobe_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/oneprobe"
)

const prefix = "/gmail/v1/users/{{userId}}"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func client(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.withone.ai" {
			t.Errorf("unexpected credential destination: %s", r.URL.Host)
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme = base.Scheme
		copy.URL.Host = base.Host
		return server.Client().Transport.RoundTrip(copy)
	})}
}

func opts(h *http.Client) oneprobe.Options {
	return oneprobe.Options{APIKey: "fixture-secret", HTTP: h, Wait: func(ctx context.Context, d time.Duration) error {
		if d <= 0 || d > 4*time.Second {
			return context.Canceled
		}
		return ctx.Err()
	}}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func vault(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"page": 1, "pages": 1, "rows": []any{map[string]any{
		"id": "fixture-uuid", "key": "fixture-selector", "platform": "gmail", "identity": "opaque-vault-identity",
		"active": true, "state": "operational", "environment": "live",
	}}})
}

func actions() []map[string]string {
	var result []map[string]string
	for _, operation := range []string{
		"GET /profile", "GET /messages", "GET /messages/{{id}}", "GET /history", "GET /labels",
		"POST /labels", "PATCH /labels/{{id}}", "DELETE /labels/{{id}}", "GET /messages/{{messageId}}/attachments/{{id}}",
		"POST /messages/{{id}}/modify", "POST /messages/{{id}}/trash", "POST /messages/{{id}}/untrash",
		"GET /drafts", "POST /drafts", "GET /drafts/{{id}}", "PUT /drafts/{{id}}", "DELETE /drafts/{{id}}",
		"POST /drafts/send", "POST /messages/send",
	} {
		method, path, _ := strings.Cut(operation, " ")
		result = append(result, map[string]string{"method": method, "path": prefix + path, "systemId": method + "-" + path})
	}
	return result
}

func TestGateNativeReadsDraftAttachmentAndApprovedSend(t *testing.T) {
	for _, mode := range []string{"read", "draft", "send"} {
		t.Run(mode, func(t *testing.T) {
			var draftRaw, attachment []byte
			var historyCalls, sendCalls, writes, deletes int
			h := client(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-One-Secret") != "fixture-secret" || r.Header.Get("Authorization") != "" {
					t.Error("incorrect authentication headers")
				}
				switch r.URL.Path {
				case "/v1/vault/connections":
					vault(w)
					return
				case "/v1/available-actions/gmail":
					writeJSON(w, map[string]any{"page": 1, "pages": 1, "rows": actions()})
					return
				}
				if r.Header.Get("X-One-Connection-Key") != "fixture-selector" || r.Header.Get("X-One-Action-Id") == "" {
					t.Error("missing connection selector or verified action ID")
				}
				path := strings.TrimPrefix(r.URL.Path, "/v1/passthrough/gmail/v1/users/me")
				switch path {
				case "/profile":
					writeJSON(w, map[string]string{"emailAddress": "fixture@example.test", "historyId": "100"})
				case "/messages":
					id, next := "msg1", "page2"
					if r.URL.Query().Get("pageToken") == "page2" {
						id, next = "msg2", ""
					}
					writeJSON(w, map[string]any{"messages": []any{map[string]string{"id": id}}, "nextPageToken": next})
				case "/messages/msg1":
					writeJSON(w, map[string]any{"id": "msg1", "threadId": "thread1", "payload": map[string]any{"headers": []any{}}, "raw": base64.RawURLEncoding.EncodeToString([]byte("From: fixture@example.test\r\n\r\nfixture"))})
				case "/labels":
					writeJSON(w, map[string]any{"labels": []any{map[string]string{"id": "INBOX"}}})
				case "/history":
					historyCalls++
					result := map[string]any{"historyId": "110"}
					if historyCalls > 1 {
						result["history"] = []any{map[string]string{"id": "101"}}
						if r.URL.Query().Get("pageToken") == "" {
							result["nextPageToken"] = "history-page2"
						}
					}
					writeJSON(w, result)
				case "/drafts":
					if r.Method == "GET" {
						writeJSON(w, map[string]any{"drafts": []any{}})
						return
					}
					writes++
					draftRaw, attachment = decodeDraft(t, r)
					writeJSON(w, map[string]any{"id": "draft1", "message": map[string]string{"id": "draftmsg1"}})
				case "/drafts/draft1":
					if r.Method == "DELETE" {
						deletes++
						w.WriteHeader(204)
						return
					}
					if r.Method == "PUT" {
						writes++
						draftRaw, _ = decodeDraft(t, r)
					}
					writeJSON(w, map[string]any{"id": "draft1", "message": map[string]any{"id": "draftmsg1", "raw": base64.RawURLEncoding.EncodeToString(draftRaw), "payload": map[string]any{"parts": []any{map[string]any{"filename": "one-probe.bin", "body": map[string]string{"attachmentId": "att1"}}}}}})
				case "/messages/draftmsg1/attachments/att1":
					writeJSON(w, map[string]any{"data": base64.RawURLEncoding.EncodeToString(attachment), "size": len(attachment)})
				case "/messages/send":
					sendCalls++
					var b struct{ Raw string }
					_ = json.NewDecoder(r.Body).Decode(&b)
					raw, _ := base64.RawURLEncoding.DecodeString(b.Raw)
					if !bytes.Contains(raw, []byte("To: controlled@example.test\r\n")) {
						t.Error("send recipient differs from explicit approval")
					}
					writeJSON(w, map[string]string{"id": "sent1"})
				case "/messages/sent1":
					writeJSON(w, map[string]any{"id": "sent1", "labelIds": []string{"SENT"}})
				default:
					t.Errorf("unexpected operation %s %s", r.Method, path)
					w.WriteHeader(404)
				}
			})
			o := opts(h)
			o.WriteDrafts = mode != "read"
			o.SendApproved = mode == "send"
			o.SendTo = "controlled@example.test"
			report, err := oneprobe.Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if !report.ReadPassed || !report.IdentityVerified || report.IdentityMatches || report.Environment != "live" || report.CleanupRequired {
				t.Fatalf("incorrect report: %+v", report)
			}
			if report.ReleaseGate != (mode == "send") || report.SendPassed != (sendCalls == 1) {
				t.Fatal("unapproved or unverified send gate")
			}
			if mode == "read" && writes != 0 || mode != "read" && (writes != 2 || deletes != 1 || !report.DraftPassed || historyCalls != 3) {
				t.Fatal("draft lifecycle/history mismatch")
			}
			encoded, _ := json.Marshal(report)
			for _, sensitive := range []string{"fixture-secret", "fixture-selector", "fixture@example.test", "controlled@example.test", "draftmsg1", "Disposable test data"} {
				if bytes.Contains(encoded, []byte(sensitive)) {
					t.Errorf("report leaked %s", sensitive)
				}
			}
		})
	}
}

func decodeDraft(t *testing.T, r *http.Request) ([]byte, []byte) {
	t.Helper()
	var b struct{ Message struct{ Raw string } }
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		t.Error(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(b.Message.Raw)
	if err != nil {
		t.Error(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Error(err)
		return raw, nil
	}
	_, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	parts := multipart.NewReader(m.Body, params["boundary"])
	for {
		part, err := parts.NextPart()
		if err != nil {
			break
		}
		if strings.Contains(part.Header.Get("Content-Disposition"), "one-probe.bin") {
			data, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
			return raw, data
		}
	}
	return raw, nil
}

func TestInvalidOptionsNeverRequest(t *testing.T) {
	h := client(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid options made request") })
	for _, o := range []oneprobe.Options{{HTTP: h}, {HTTP: h, APIKey: "bad\r\nkey"}, {HTTP: h, APIKey: "fixture-secret", SendApproved: true, SendTo: "controlled@example.test"}} {
		if _, err := oneprobe.Run(context.Background(), o); err == nil {
			t.Fatal("accepted unsafe options")
		}
	}
}

func TestErrorsBoundedRedactedAndRedirectsNotFollowed(t *testing.T) {
	for _, status := range []int{302, 401, 403, 404, 429, 503, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			h := client(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "https://evil.example/fixture-secret")
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = io.WriteString(w, strings.Repeat("x", (4<<20)+1))
				} else {
					_, _ = io.WriteString(w, `{"key":"error","message":"fixture-secret fixture-selector personal@example.test"}`)
				}
			})
			report, err := oneprobe.Run(context.Background(), opts(h))
			if err == nil || calls != 1 || report.ReleaseGate {
				t.Fatal("error retried, redirected or accepted")
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(err.Error()+string(encoded), "fixture-secret") || strings.Contains(string(encoded), "personal@example.test") {
				t.Fatal("error leaked secret or personal data")
			}
			if report.Results[0].Headers["Retry-After"] != "60" {
				t.Fatal("lost rate limit evidence")
			}
		})
	}
}

func TestDiscoveryFailsClosed(t *testing.T) {
	for _, mode := range []string{"inactive", "ambiguous", "missing-action", "duplicate-action", "wrong-method", "invalid-pagination", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			h := client(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/v1/vault/connections" {
					switch mode {
					case "inactive":
						writeJSON(w, map[string]any{"page": 1, "pages": 1, "rows": []any{map[string]any{"id": "one", "key": "selector", "platform": "gmail", "active": false, "state": "operational"}}})
					case "ambiguous":
						c := map[string]any{"id": "one", "key": "selector", "platform": "gmail", "active": true, "state": "operational"}
						writeJSON(w, map[string]any{"page": 1, "pages": 1, "rows": []any{c, c}})
					case "invalid-pagination":
						writeJSON(w, map[string]any{"page": 2, "pages": 1, "rows": []any{}})
					default:
						vault(w)
					}
					return
				}
				rows := actions()
				switch mode {
				case "duplicate-action":
					rows = append(rows, rows[0])
				case "wrong-method":
					rows[0]["method"] = "POST"
				default:
					rows = rows[1:]
				}
				writeJSON(w, map[string]any{"page": 1, "pages": 1, "rows": rows})
			})
			o := opts(h)
			if mode == "canceled" {
				o.Wait = func(context.Context, time.Duration) error { return context.Canceled }
			}
			if report, err := oneprobe.Run(context.Background(), o); err == nil || report.ReleaseGate || calls > 2 {
				t.Fatal("unsafe discovery accepted")
			}
		})
	}
}
