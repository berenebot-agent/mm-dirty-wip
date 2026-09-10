package httpapp_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

const resendFixtureKey = "resend-webhook-fixture-key"

// Keep this fixture self-contained so it can also be run while the existing
// HTTP tests are being moved to external test packages.
func resendIngestFixture(t *testing.T) (*app.Service, *httpapp.Server, model.Principal, model.Inbox) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		DataDir: dir, Mode: "selfhosted", BaseURL: "http://example.test",
		AppEncryptionKey: "01234567890123456789012345678901",
		MaxMessageBytes:  5 << 20, DefaultQuotaBytes: 50 << 20,
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Resend test", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	dom, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, dom.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "resend", map[string]any{
		"api_key": "re_fixture", "webhook_secret": "whsec_" + base64.StdEncoding.EncodeToString([]byte(resendFixtureKey)),
		"api_base": "https://resend-api.example.test",
	}, false); err != nil {
		t.Fatal(err)
	}
	return svc, httpapp.New(svc, nil), model.Principal{AccountID: u.AccountID, Admin: true}, box
}

func resendWebhookRequest() *http.Request {
	body := `{"type":"email.received","data":{"email_id":"bdf8f02a-dd50-44b5-822b-ef60de313bea","from":"sender@outside.test","to":["hermes@example.com"],"message_id":"<resend-fixture@outside.test>"}}`
	r := httptest.NewRequest(http.MethodPost, "/internal/ingest/resend", strings.NewReader(body))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(resendFixtureKey))
	mac.Write([]byte("evt_fixture." + ts + "." + body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("svix-id", "evt_fixture")
	r.Header.Set("svix-timestamp", ts)
	r.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return r
}

type resendRoundTripFunc func(*http.Request) (*http.Response, error)

func (f resendRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func setResendRoundTripper(t *testing.T, f resendRoundTripFunc) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func resendFixtureResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestResendIngestSurvivesWebhookCancellation(t *testing.T) {
	for _, listener := range []string{"main", "inbound"} {
		for _, cancelAt := range []string{"metadata", "raw"} {
			t.Run(listener+"/"+cancelAt, func(t *testing.T) {
				svc, server, principal, box := resendIngestFixture(t)
				handler := server.Handler()
				if listener == "inbound" {
					handler = server.InboundHandler()
				}
				requestCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				raw := "From: sender@outside.test\r\nTo: hermes@example.com\r\nSubject: Survives disconnect\r\nMessage-ID: <resend-fixture@outside.test>\r\n\r\nReceived despite webhook disconnect.\r\n"
				calls := 0
				setResendRoundTripper(t, func(r *http.Request) (*http.Response, error) {
					calls++
					stage, body := "raw", raw
					if r.URL.Host == "resend-api.example.test" {
						stage = "metadata"
						if r.URL.Path != "/emails/receiving/bdf8f02a-dd50-44b5-822b-ef60de313bea" || r.URL.Query().Get("html_format") != "cid" {
							t.Fatalf("unexpected metadata URL: %s", r.URL)
						}
						if r.Header.Get("Authorization") != "Bearer re_fixture" {
							t.Fatal("missing API authorization")
						}
						body = `{"raw":{"download_url":"https://resend-raw.example.test/message.eml"}}`
					} else if r.URL.Host != "resend-raw.example.test" {
						t.Fatalf("unexpected request: %s", r.URL)
					}
					if stage == cancelAt {
						cancel()
					}
					// Model the HTTP client's behavior when its request is canceled.
					if err := r.Context().Err(); err != nil {
						return nil, err
					}
					if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 3*time.Minute {
						t.Fatal("provider fetch must have a bounded deadline")
					}
					return resendFixtureResponse(r, http.StatusOK, body), nil
				})

				var firstID string
				for attempt := 0; attempt < 2; attempt++ {
					r := resendWebhookRequest()
					if attempt == 0 {
						r = r.WithContext(requestCtx)
					}
					rr := httptest.NewRecorder()
					handler.ServeHTTP(rr, r)
					if rr.Code != http.StatusOK {
						t.Fatalf("attempt %d: status=%d body=%s", attempt, rr.Code, rr.Body.String())
					}
					var result struct {
						MessageID string `json:"message_id"`
						Duplicate bool   `json:"duplicate"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.MessageID == "" || result.Duplicate != (attempt == 1) {
						t.Fatalf("attempt %d: response=%+v", attempt, result)
					}
					if attempt == 0 {
						firstID = result.MessageID
					} else if result.MessageID != firstID {
						t.Fatal("retry created a second message")
					}
				}
				if requestCtx.Err() != context.Canceled || calls != 4 {
					t.Fatalf("cancellation=%v provider calls=%d", requestCtx.Err(), calls)
				}
				messages, err := svc.Store.ListMessages(context.Background(), principal, store.MessageFilter{InboxID: box.ID})
				if err != nil || len(messages) != 1 || messages[0].Subject != "Survives disconnect" {
					t.Fatalf("stored messages=%+v err=%v", messages, err)
				}
				stored, err := os.ReadFile(filepath.Join(svc.Config.DataDir, messages[0].RawPath))
				if err != nil || string(stored) != raw {
					t.Fatalf("stored MIME mismatch: %v", err)
				}
				events, err := svc.Store.ListEvents(context.Background(), principal, 0, box.ID, 100)
				if err != nil {
					t.Fatal(err)
				}
				received := 0
				for _, event := range events {
					if event.Type == "message.received" {
						received++
					}
				}
				if received != 1 {
					t.Fatalf("received events=%d, want 1", received)
				}
				staged, err := os.ReadDir(filepath.Join(svc.Config.DataDir, "messages", ".tmp"))
				if err != nil || len(staged) != 0 {
					t.Fatalf("staged files=%v err=%v", staged, err)
				}
			})
		}
	}
}

func TestResendIngestRejectsInvalidSignatureAndRetriesFetchFailure(t *testing.T) {
	svc, server, principal, box := resendIngestFixture(t)
	calls := 0
	setResendRoundTripper(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return resendFixtureResponse(r, http.StatusServiceUnavailable, `{"message":"temporarily unavailable"}`), nil
	})
	r := resendWebhookRequest()
	r.Header.Set("svix-signature", "v1,invalid")
	rr := httptest.NewRecorder()
	server.InboundHandler().ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("invalid signature: status=%d provider calls=%d", rr.Code, calls)
	}
	rr = httptest.NewRecorder()
	server.InboundHandler().ServeHTTP(rr, resendWebhookRequest())
	if rr.Code != http.StatusInternalServerError || calls != 1 {
		t.Fatalf("fetch failure: status=%d provider calls=%d", rr.Code, calls)
	}
	messages, err := svc.Store.ListMessages(context.Background(), principal, store.MessageFilter{InboxID: box.ID})
	if err != nil || len(messages) != 0 {
		t.Fatalf("failed fetch persisted messages=%+v err=%v", messages, err)
	}
}
