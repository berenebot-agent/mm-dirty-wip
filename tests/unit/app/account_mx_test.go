package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// adminP is the account-admin principal for a fixture user.
func adminP(u model.User) model.Principal {
	return model.Principal{UserID: u.ID, AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
}

// TestAccountMXReceiverLifecycle covers the per-account Remote MX receiver: an
// admin-only save with a bearer key, a read that redacts the key, a blank-key
// resave that retains it, CAS conflict handling, the global one-account-per-
// receiver rule, and the fail-closed clear while a domain still routes to it.
func TestAccountMXReceiverLifecycle(t *testing.T) {
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	ctx := context.Background()

	if _, err := svc.GetAccountMXReceiver(ctx, u.AccountID); err != nil {
		t.Fatalf("unconfigured GetAccountMXReceiver = %v", err)
	}

	saved, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{
		URL: "https://mx.example.test", BearerKey: "secret-key-1",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.URL != "https://mx.example.test" || !saved.KeyConfigured || saved.BearerKey != "" {
		t.Fatalf("saved (redaction) = %+v", saved)
	}
	if saved.Revision != 1 {
		t.Fatalf("revision = %d, want 1", saved.Revision)
	}

	// The runtime accessor exposes the decrypted key.
	rt, err := svc.AccountMXReceiverSettingsForRuntime(ctx, u.AccountID)
	if err != nil || rt.BearerKey != "secret-key-1" {
		t.Fatalf("runtime settings = %+v, %v", rt, err)
	}

	// A blank key retains the stored credential.
	saved2, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{
		URL: "https://mx.example.test", Revision: saved.Revision,
	})
	if err != nil {
		t.Fatalf("resave: %v", err)
	}
	if rt2, _ := svc.AccountMXReceiverSettingsForRuntime(ctx, u.AccountID); rt2.BearerKey != "secret-key-1" {
		t.Fatalf("blank-key resave lost the key: %q", rt2.BearerKey)
	}

	// A stale revision conflicts.
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{
		URL: "https://mx.example.test", BearerKey: "x", Revision: saved.Revision,
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision = %v, want ErrConflict", err)
	}

	// Selecting Remote MX for a domain binds it to the account receiver.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, app.RemoteMXProvider, map[string]any{}, false); err != nil {
		t.Fatalf("domain select: %v", err)
	}
	// Clear is refused while a domain still routes to it.
	if err := svc.ClearAccountMXReceiver(ctx, adminP(u), saved2.Revision); !errors.Is(err, app.ErrAccountMXInvalidInput) {
		t.Fatalf("clear in use = %v, want ErrAccountMXInvalidInput", err)
	}
}

// TestAccountMXReceiverGlobalUniqueness proves one physical single-mode receiver
// may be registered by exactly one account.
func TestAccountMXReceiverGlobalUniqueness(t *testing.T) {
	svc, u, _ := newMXServiceWithOutboundPolicy(t, true)
	ctx := context.Background()

	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: "https://shared.example.test", BearerKey: "k1"}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(other), app.AccountMXReceiverInput{URL: "https://shared.example.test", BearerKey: "k2"}); !errors.Is(err, app.ErrAccountMXInvalidInput) {
		t.Fatalf("duplicate receiver = %v, want ErrAccountMXInvalidInput", err)
	}
}

// TestAccountMXReceiverURLPolicy proves a public receiver must be https and
// public-routable, while a private opt-in permits an http/LAN receiver only when
// the operator allows private outbound.
func TestAccountMXReceiverURLPolicy(t *testing.T) {
	// Operator allows private outbound: the public policy still rejects private
	// hosts unless the account explicitly opts in.
	svc, u, _ := newMXServiceWithOutboundPolicy(t, true)
	ctx := context.Background()

	for _, bad := range []string{"http://mx.example.test", "https://127.0.0.1:8443", "https://10.0.0.5:8443"} {
		if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: bad, BearerKey: "k"}); !errors.Is(err, app.ErrAccountMXInvalidInput) {
			t.Fatalf("public save %q = %v, want ErrAccountMXInvalidInput", bad, err)
		}
	}
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: "http://10.0.0.5:8443", BearerKey: "k", AllowPrivate: true}); err != nil {
		t.Fatalf("private save with operator opt-in: %v", err)
	}
}

// TestAccountMXReceiverOperatorPolicyWins proves an account cannot re-enable
// private destinations when the operator confines outbound to the public
// internet: the per-account allow_private opt-in is ignored and the save is held
// to the public-only policy.
func TestAccountMXReceiverOperatorPolicyWins(t *testing.T) {
	svc, u, _ := newMXServiceWithOutboundPolicy(t, false)
	ctx := context.Background()

	for _, bad := range []string{"http://10.0.0.5:8443", "https://127.0.0.1:8443"} {
		if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: bad, BearerKey: "k", AllowPrivate: true}); !errors.Is(err, app.ErrAccountMXInvalidInput) {
			t.Fatalf("private save %q under public-only policy = %v, want ErrAccountMXInvalidInput", bad, err)
		}
	}
	// A public HTTPS origin still saves fine.
	if _, err := svc.SaveAccountMXReceiver(ctx, adminP(u), app.AccountMXReceiverInput{URL: "https://mx.example.test", BearerKey: "k"}); err != nil {
		t.Fatalf("public save: %v", err)
	}
}

// TestAccountMXReceiverAdminOnly proves a non-admin principal cannot save.
func TestAccountMXReceiverAdminOnly(t *testing.T) {
	svc, u, _ := newMXServiceWithOutboundPolicy(t, true)
	ctx := context.Background()
	nonAdmin := model.Principal{UserID: u.ID, AccountID: u.AccountID}
	if _, err := svc.SaveAccountMXReceiver(ctx, nonAdmin, app.AccountMXReceiverInput{URL: "https://mx.example.test", BearerKey: "k"}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("non-admin save = %v, want ErrForbidden", err)
	}
}

// TestRemoteMXDomainSelectRequiresReceiver proves selecting Remote MX for a
// domain fails closed when the account has no receiver configured.
func TestRemoteMXDomainSelectRequiresReceiver(t *testing.T) {
	svc, u, d := newMXServiceWithOutboundPolicy(t, true)
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, app.RemoteMXProvider, map[string]any{}, false); !errors.Is(err, app.ErrInvalidConfig) {
		t.Fatalf("select without receiver = %v, want ErrInvalidConfig", err)
	}
}
