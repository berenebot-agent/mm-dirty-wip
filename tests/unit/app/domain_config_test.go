package app_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func configInt(t *testing.T, v any) int {
	t.Helper()
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		t.Fatalf("value %#v is not numeric", v)
		return 0
	}
}

// TestSendingConfigValidationWithoutMutation verifies that unknown options,
// wrong types, invalid select values and bad numbers are rejected before any
// write, leaving the stored configuration and revision untouched.
func TestSendingConfigValidationWithoutMutation(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com"}); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(before)
	if err != nil {
		t.Fatal(err)
	}
	if configInt(t, dec["port"]) != 587 || dec["security"] != "starttls" {
		t.Fatalf("defaults not applied: %#v", dec)
	}

	rejections := []struct {
		name string
		cfg  map[string]any
	}{
		{"unknown option", map[string]any{"host": "smtp.example.com", "bogus": "x"}},
		{"wrong type", map[string]any{"host": 123}},
		{"fractional port", map[string]any{"host": "smtp.example.com", "port": 587.5}},
		{"out of range port", map[string]any{"host": "smtp.example.com", "port": 70000}},
		{"invalid option", map[string]any{"host": "smtp.example.com", "security": "nope"}},
		{"missing required", map[string]any{"port": 25}},
	}
	for _, tc := range rejections {
		if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", tc.cfg); !errors.Is(err, app.ErrInvalidConfig) {
			t.Fatalf("%s: err=%v, want ErrInvalidConfig", tc.name, err)
		}
	}

	after, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.ID != before.ID || after.Provider != "smtp" {
		t.Fatalf("rejected save mutated config: before=%+v after=%+v", before, after)
	}
}

// TestSMTPSendingBlankPasswordRetained verifies same-provider secret retention
// for an optional (non-required) SMTP password, while non-secret fields follow
// whole-config replacement semantics.
func TestSMTPSendingBlankPasswordRetained(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com", "username": "u", "password": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	// Blank/omitted password retains the stored secret; username, a non-secret
	// field, is a whole-config value and is dropped when omitted.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(got)
	if err != nil {
		t.Fatal(err)
	}
	if dec["password"] != "s3cret" {
		t.Fatalf("password not retained: %#v", dec)
	}
	if _, ok := dec["username"]; ok {
		t.Fatalf("non-secret field should be replaced whole, got %#v", dec)
	}

	// An explicitly supplied password replaces the retained one.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com", "password": "new-secret"}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	dec, _ = svc.DecryptDomainSendingConfig(got)
	if dec["password"] != "new-secret" {
		t.Fatalf("password not replaced: %#v", dec)
	}
}

// TestProviderChangeDropsOldSecrets verifies that changing a domain's sending
// provider never reuses the previous provider's fields or secrets, and that a
// required secret must be supplied afresh.
func TestProviderChangeDropsOldSecrets(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "old-key", "api_base": "http://old"}); err != nil {
		t.Fatal(err)
	}
	// A provider with an optional secret can be configured without one: the old
	// api_key/api_base must not leak through.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com"}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	dec, _ := svc.DecryptDomainSendingConfig(got)
	if _, ok := dec["api_key"]; ok {
		t.Fatalf("old secret leaked across provider change: %#v", dec)
	}
	if _, ok := dec["api_base"]; ok {
		t.Fatalf("old field leaked across provider change: %#v", dec)
	}

	// A required secret on the new provider cannot be silently retained.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "resend", map[string]any{}); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("required secret on provider change: err=%v", err)
	}
	unchanged, _ := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if unchanged.Provider != "smtp" {
		t.Fatalf("failed provider change mutated config: %+v", unchanged)
	}

	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "mailgun", map[string]any{"api_key": "new-key", "domain": "mg.example.com"}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	dec, _ = svc.DecryptDomainSendingConfig(got)
	if dec["api_key"] != "new-key" {
		t.Fatalf("fresh secret not stored: %#v", dec)
	}
}

// TestDomainConfigsAreIsolated verifies that one domain's configuration is
// independent of another's: updates and deletes never affect the sibling.
func TestDomainConfigsAreIsolated(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	other, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "key-a", "api_base": "http://a"})
	seedSending(t, svc, u.AccountID, other.ID, "brevo", map[string]any{"api_key": "key-b", "api_base": "http://b"})

	before, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "key-a2"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.Provider != before.Provider {
		t.Fatalf("sibling domain changed: before=%+v after=%+v", before, after)
	}
	otherDec, _ := svc.DecryptDomainSendingConfig(after)
	if otherDec["api_key"] != "key-b" {
		t.Fatalf("sibling secret changed: %#v", otherDec)
	}
}

// TestCloudflareReceivingSecretLifecycle verifies generated-secret semantics:
// auto-generate on create, preserve on a normal same-provider save, regenerate
// only on explicit request, accept an explicitly supplied secret, and mint a
// fresh secret after a provider change.
func TestCloudflareReceivingSecretLifecycle(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	saved, generated, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	first := generated["webhook_secret"]
	if first == "" {
		t.Fatalf("no secret generated on create: %#v", generated)
	}
	dec, _ := svc.DecryptDomainReceivingConfig(saved)
	if dec["webhook_secret"] != first {
		t.Fatalf("stored secret mismatch: %#v", dec)
	}

	// A normal same-provider save preserves the secret and returns no generated
	// map.
	saved, generated, err = svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 0 {
		t.Fatalf("ordinary save returned generated secrets: %#v", generated)
	}
	dec, _ = svc.DecryptDomainReceivingConfig(saved)
	if dec["webhook_secret"] != first {
		t.Fatalf("ordinary save rotated the secret: %#v", dec)
	}

	// Explicit regenerate changes the secret and returns it exactly once.
	saved, generated, err = svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{}, true)
	if err != nil {
		t.Fatal(err)
	}
	second := generated["webhook_secret"]
	if second == "" || second == first {
		t.Fatalf("regenerate did not mint a fresh secret: first=%q generated=%#v", first, generated)
	}
	dec, _ = svc.DecryptDomainReceivingConfig(saved)
	if dec["webhook_secret"] != second {
		t.Fatalf("regenerated secret not stored: %#v", dec)
	}

	// Regenerating while also supplying a value is rejected without mutation.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{"webhook_secret": "supplied"}, true); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("regenerate with supplied value: err=%v", err)
	}

	// A caller may explicitly supply a Cloudflare secret on a normal save.
	saved, generated, err = svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{"webhook_secret": "explicit"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 0 {
		t.Fatalf("explicit secret reported as generated: %#v", generated)
	}
	dec, _ = svc.DecryptDomainReceivingConfig(saved)
	if dec["webhook_secret"] != "explicit" {
		t.Fatalf("explicit secret not stored: %#v", dec)
	}

	// Regenerate is invalid for a provider that is not currently configured.
	fresh, err := svc.Store.CreateDomain(ctx, u.AccountID, "fresh.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, fresh.ID, "cloudflare", map[string]any{}, true); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("regenerate unconfigured: err=%v", err)
	}

	// Switching providers drops the old secret; switching back mints a fresh
	// one rather than resurrecting the previous value.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "mailgun", map[string]any{"signing_key": "mg"}, false); err != nil {
		t.Fatal(err)
	}
	saved, generated, err = svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	third := generated["webhook_secret"]
	if third == "" || third == first || third == second {
		t.Fatalf("provider change did not mint a fresh secret: %#v", generated)
	}
	dec, _ = svc.DecryptDomainReceivingConfig(saved)
	if dec["webhook_secret"] != third {
		t.Fatalf("fresh secret not stored: %#v", dec)
	}
}

// TestProviderAcceptedWhileConfigDeletedStillRecordsSent verifies that a
// delivery whose config is resolved and accepted, then deleted while the
// provider call is in flight, still records the sent outcome and its delivery
// attempt against the message's domain.
func TestProviderAcceptedWhileConfigDeletedStillRecordsSent(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<in-flight>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Inflight", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- svc.Deliver(ctx, u.AccountID, res.Message.ID, "") }()

	<-started
	if err := svc.Store.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("delivery after config deletion: %v", err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || sent.Provider != "brevo" || sent.ProviderMessageID != "<in-flight>" {
		t.Fatalf("outcome lost: %+v", sent)
	}
	attempts, err := svc.Store.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Status != "sent" || attempts[0].DomainID != d.ID {
		t.Fatalf("attempt not recorded against domain: %+v", attempts)
	}
}

// TestNoConfigQueuesWithoutRetryConsumption verifies that a domain without a
// sending config holds its queued message without consuming a retry, and then
// delivers once a config is saved.
func TestNoConfigQueuesWithoutRetryConsumption(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Hold", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Status != "pending" || res.Message.LastError == "" {
		t.Fatalf("message not queued: %+v", res.Message)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	held, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "pending" || held.Attempts != 0 {
		t.Fatalf("hold consumed a retry: status=%q attempts=%d", held.Status, held.Attempts)
	}

	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<held-out>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || calls != 1 {
		t.Fatalf("message not delivered after config: status=%q calls=%d", sent.Status, calls)
	}
}

// TestDomainConfigCASConflict verifies that a stale revision is rejected with
// ErrConflict rather than overwriting a concurrent rotation.
func TestDomainConfigCASConflict(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	first, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "one"})
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent writer advances the revision.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "two"}); err != nil {
		t.Fatal(err)
	}
	stale := store.ConfigVersion{ID: first.ID, Revision: first.Revision}
	enc := first.EncryptedConfig
	if _, err := svc.Store.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", enc, stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale save err=%v, want ErrConflict", err)
	}
}
