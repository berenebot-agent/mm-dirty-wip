package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestDialMXProviderIsolationAndMXIdentity(t *testing.T) {
	svc, u, d, box := mxService(t)
	svc.Config.MXReceiveEnabled = false
	if _, _, err := svc.SaveDomainReceivingConfig(context.Background(), u.AccountID, d.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example"}, false); err != nil {
		t.Fatal(err)
	}
	resolved := svc.ResolveDialMXRecipients(context.Background(), []string{box.Address})
	if len(resolved) != 1 || !resolved[0].Accept {
		t.Fatalf("dialmx recipient should resolve: %+v", resolved)
	}
	if got := svc.ResolveMXRecipients(context.Background(), []string{box.Address}); len(got) != 1 || got[0].Accept {
		t.Fatalf("mx must refuse dialmx-bound recipient: %+v", got)
	}
	raw := strings.Replace(goodRaw, "direct smtp", "dialmx", 1)
	res, err := svc.IngestDialMX(context.Background(), mxInput(t, svc, box.Address, raw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if rr := mxResult(t, res); rr.MachineCode != mxwire.CodeOK {
		t.Fatalf("dialmx ingest: %+v", rr)
	}
	msgs, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
	if err != nil || len(msgs) != 1 || msgs[0].Provider != "mx" {
		t.Fatalf("stored identity should remain mx: messages=%+v err=%v", msgs, err)
	}
	if _, err = svc.IngestMX(context.Background(), mxInput(t, svc, box.Address, strings.Replace(raw, "dialmx", "blocked mx path", 1), mxwire.AuthResults{})); !errors.Is(err, app.ErrMXDisabled) {
		t.Fatalf("mx path should remain disabled: %v", err)
	}
	if _, err = svc.Store.LookupMXReceipt(context.Background(), u.AccountID, "mx", box.Address, mxwire.DeliveryFingerprint("sender@outside.test", box.Address, mxwire.BodyDigest([]byte(raw)))); err != nil {
		t.Fatalf("mx receipt identity missing: %v", err)
	}
	if _, err = svc.Store.LookupMXReceipt(context.Background(), u.AccountID, "dialmx", box.Address, mxwire.DeliveryFingerprint("sender@outside.test", box.Address, mxwire.BodyDigest([]byte(raw)))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("routing provider must not change receipt identity: %v", err)
	}
}

func TestDialMXReceiverURLValidationAndEncryptedCredentialRotation(t *testing.T) {
	svc, u, d, _ := testService(t)
	ctx := context.Background()
	for _, invalid := range []string{"http://receiver.example", "https://u:p@receiver.example", "https://receiver.example/path", "https://receiver.example/path?q=1", "https://receiver.example/#f", "https://receiver.example,not-a-url"} {
		if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{"receiver_urls": invalid}, false); !errors.Is(err, app.ErrInvalidConfig) {
			t.Fatalf("URL %q err=%v", invalid, err)
		}
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example/,https://receiver.example", "enforcement": "hard"}, false); err != nil {
		t.Fatal(err)
	}
	first, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first.EncryptedPrivateSeed, first.PublicKey) || first.EncryptedPrivateSeed == first.PublicKey {
		t.Fatal("private material is not encrypted separately")
	}
	seed, err := svc.DecryptSecret(first.EncryptedPrivateSeed)
	if err != nil || len(seed) != 32 {
		t.Fatalf("private seed decrypt len=%d err=%v", len(seed), err)
	}
	stored, _ := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	values, _ := svc.DecryptDomainReceivingConfig(stored)
	if values["receiver_urls"] != "https://receiver.example" {
		t.Fatalf("URL not normalized: %#v", values)
	}
	second, err := svc.RotateDialMXCredential(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID == second.KeyID || first.PublicKey == second.PublicKey {
		t.Fatal("rotation did not replace key")
	}
	if _, err := svc.Store.GetDialMXCredential(ctx, "foreign", d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account credential read: %v", err)
	}
	child, err := svc.Store.CreateDomain(ctx, u.AccountID, "child.example.com")
	if err != nil {
		t.Fatal(err)
	}
	childCredential, err := svc.EnsureDialMXCredential(ctx, u.AccountID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childCredential.KeyID == first.KeyID || childCredential.PublicKey == first.PublicKey {
		t.Fatal("inherited child reused parent credentials")
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, child.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("credential creation copied inherited configuration: %v", err)
	}
	childRotated, err := svc.RotateDialMXCredential(ctx, u.AccountID, child.ID)
	if err != nil || childRotated.KeyID == childCredential.KeyID {
		t.Fatalf("child rotation failed: %+v %v", childRotated, err)
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, child.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("rotation copied inherited configuration: %v", err)
	}
	current, _ := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if current.Revision != stored.Revision || current.EncryptedConfig != stored.EncryptedConfig {
		t.Fatal("key rotation changed receiving settings")
	}
}
