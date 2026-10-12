package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// partialFanoutFixture is a Resend-configured mailbox on the shared migrated
// test template (testdb), so it adds no per-test migration cost. It mirrors
// resendIngestFixture but reuses the cached schema.
func partialFanoutFixture(t *testing.T) (*app.Service, http.Handler, model.Principal, model.Inbox) {
	t.Helper()
	ctx := context.Background()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{
		DataDir: dir, Mode: "selfhosted", AllowPrivateOutbound: true, BaseURL: "http://example.test",
		AppEncryptionKey: "01234567890123456789012345678901",
		MaxMessageBytes:  5 << 20, DefaultQuotaBytes: 50 << 20,
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "Fanout test", "fanout-admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
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
	return svc, httpapp.New(svc, nil).Handler(), model.Principal{AccountID: u.AccountID, Admin: true}, box
}

// TestResendPartialFanOutIsRetryable is the regression guard for the
// fan-out acknowledgement bug: a single provider event addressing several
// MailMoose inboxes must not be acknowledged with 200 once any recipient
// failed transiently. Previously a delivery to inbox A succeeding while inbox B
// failed (for example on quota) returned 200, so the provider considered the
// event delivered and inbox B never received a copy.
//
// The fix answers a retryable failure instead. Because each recipient's copy
// deduplicates on (account, provider, envelope_recipient, provider_delivery_id),
// the retry stores only the inbox that was missing and leaves the already-stored
// one untouched.
func TestResendPartialFanOutIsRetryable(t *testing.T) {
	svc, handler, principal, box := partialFanoutFixture(t)
	ctx := context.Background()

	// A second inbox that will fail its delivery on a tiny per-inbox quota.
	dom, err := svc.Store.GetDomain(ctx, principal.AccountID, box.DomainID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Store.CreateInbox(ctx, principal.AccountID, dom.ID, "second", "Second")
	if err != nil {
		t.Fatal(err)
	}
	tiny := int64(16)
	if err := svc.Store.SetInboxStorageQuota(ctx, principal.AccountID, second.ID, &tiny); err != nil {
		t.Fatal(err)
	}

	raw := "From: sender@outside.test\r\nTo: hermes@example.com, second@example.com\r\nSubject: fanout\r\nMessage-ID: <fanout@outside.test>\r\n\r\nbody long enough to exceed the tiny quota\r\n"
	setResendRoundTripper(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "resend-api.example.test" {
			return resendFixtureResponse(r, http.StatusOK, `{"raw":{"download_url":"https://resend-raw.example.test/message.eml"}}`), nil
		}
		if r.URL.Host == "resend-raw.example.test" {
			return resendFixtureResponse(r, http.StatusOK, raw), nil
		}
		t.Fatalf("unexpected request: %s", r.URL)
		return nil, nil
	})

	body := `{"type":"email.received","data":{"email_id":"fanout-1","from":"sender@outside.test","to":["hermes@example.com","second@example.com"],"message_id":"<fanout@outside.test>"}}`

	// First attempt: inbox A stores, inbox B fails on quota -> retryable, not 200.
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, resendSignedRequest(t, body))
	if rr.Code == http.StatusOK {
		t.Fatalf("partial fan-out acknowledged 200; body=%s", rr.Body.String())
	}
	if rr.Code < 500 {
		t.Fatalf("partial fan-out status = %d, want a retryable 5xx", rr.Code)
	}

	// Inbox A has its copy; inbox B does not (yet).
	if first, err := svc.Store.ListMessages(ctx, principal, store.MessageFilter{InboxID: box.ID}); err != nil || len(first) != 1 {
		t.Fatalf("first inbox messages=%d err=%v, want 1", len(first), err)
	}
	if got, err := svc.Store.ListMessages(ctx, principal, store.MessageFilter{InboxID: second.ID}); err != nil || len(got) != 0 {
		t.Fatalf("second inbox messages=%d err=%v, want 0 before retry", len(got), err)
	}

	// Clear the quota obstacle and retry the same event: only the missing inbox
	// is stored, and the first is deduplicated rather than duplicated.
	if err := svc.Store.SetInboxStorageQuota(ctx, principal.AccountID, second.ID, nil); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, resendSignedRequest(t, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("retry status = %d body=%s", rr.Code, rr.Body.String())
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	for _, in := range []model.Inbox{box, second} {
		msgs, err := svc.Store.ListMessages(ctx, principal, store.MessageFilter{InboxID: in.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("inbox %s stored %d messages after retry, want 1", in.Address, len(msgs))
		}
	}
	if !strings.Contains(rr.Body.String(), `"accepted":true`) {
		t.Fatalf("retry body = %s", rr.Body.String())
	}
}
