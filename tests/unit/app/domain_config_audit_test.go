package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// TestConfigTypeValidationNeverLeaksAValue verifies that every invalid-type
// field is rejected with the ErrInvalidConfig sentinel and that the error text
// never contains the supplied value (which could be a secret).
func TestConfigTypeValidationNeverLeaksAValue(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()

	const secret = "sk_live_do_not_leak"
	cases := []struct {
		name     string
		provider string
		cfg      map[string]any
	}{
		{"secret non-string", "brevo", map[string]any{"api_key": 12345}},
		{"secret nested", "brevo", map[string]any{"api_key": map[string]any{"leak": secret}}},
		{"non-secret non-string", "brevo", map[string]any{"api_key": "k", "api_base": 42}},
		{"number as string", "smtp", map[string]any{"host": "smtp.example.com", "port": "587"}},
		{"select non-string", "smtp", map[string]any{"host": "smtp.example.com", "security": 1}},
		{"bool for text", "brevo", map[string]any{"api_key": "k", "api_base": true}},
		{"object for text", "brevo", map[string]any{"api_key": "k", "api_base": map[string]any{"leak": secret}}},
	}
	for _, tc := range cases {
		_, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, tc.provider, tc.cfg)
		if !errors.Is(err, app.ErrInvalidConfig) {
			t.Fatalf("%s: err=%v, want ErrInvalidConfig", tc.name, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: error leaked the supplied value: %v", tc.name, err)
		}
	}
	// A receiving generated secret supplied as a non-string is a validation
	// error, not a 500 and not a silently generated replacement.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{"webhook_secret": 99}, false); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("generated secret non-string err=%v, want ErrInvalidConfig", err)
	}
	// Nothing was persisted by any rejected save.
	if _, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("rejected saves persisted a config: %v", err)
	}
}

// TestDomainConfigCrossAccountLookups proves message and attempt lookups are
// account-scoped: a foreign account never resolves or records against another
// account's message/domain.
func TestDomainConfigCrossAccountLookups(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k"})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "x", Text: "y"}, "")
	if err != nil {
		t.Fatal(err)
	}

	other, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DomainSendingConfigForMessage(ctx, other.AccountID, res.Message.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account config-for-message err=%v, want not found", err)
	}
	if err := svc.Store.RecordDeliveryAttempt(ctx, store.DeliveryAttempt{AccountID: other.AccountID, DomainID: d.ID, Provider: "brevo", Attempt: 1, Status: "failed"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account attempt err=%v, want not found", err)
	}
	if _, err := svc.Store.ListDomainDeliveryAttempts(ctx, other.AccountID, d.ID, 10, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account history err=%v, want not found", err)
	}
}
