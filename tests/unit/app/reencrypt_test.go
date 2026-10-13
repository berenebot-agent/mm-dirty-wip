package app_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/store"
)

// legacySeal writes a pre-AAD envelope exactly as the previous release did: GCM
// over the key with a nil AAD, base64url, no version prefix.
func legacySeal(t *testing.T, key []byte, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatal(err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, ct...))
}

// TestReencryptLegacySecretsRewritesPreAADBlobs proves the one-time walk
// converts the pre-AAD values a previous release wrote — a webhook signing
// secret, a relay secret and a relay delivery key — into AAD-bound ciphertext
// readable through the normal path, including a blob sealed under the
// pre-PBKDF2 key derivation. It is idempotent: a second pass rewrites nothing.
func TestReencryptLegacySecretsRewritesPreAADBlobs(t *testing.T) {
	t.Parallel()
	svc, u, _, box := testService(t)
	ctx := context.Background()

	hookSealed := legacySeal(t, svc.EncryptionKey, "hook-secret")
	created, err := svc.Store.CreateWebhookClient(ctx, u.AccountID, box.ID, "legacy-hook", "https://hooks.example.test/x", "notify", "signature", hookSealed)
	if err != nil {
		t.Fatal(err)
	}

	legacyKey, ok := cryptox.DeprecatedLegacyKey(svc.Config.AppEncryptionKey)
	if !ok {
		t.Fatal("the test key must have a legacy derivation")
	}
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	relaySecret := legacySeal(t, svc.EncryptionKey, "relay-secret")
	// The delivery key is sealed under the pre-PBKDF2 derivation, so the walk
	// has to fall back to it.
	relayDelivery := legacySeal(t, legacyKey, "delivery-secret")
	if _, err := svc.Store.CreateHermesConnection(ctx, rec, "gw-legacy", relaySecret, relayDelivery); err != nil {
		t.Fatal(err)
	}

	migrated, failed := svc.ReencryptLegacySecrets(ctx)
	if migrated != 2 || failed != 0 {
		t.Fatalf("walk migrated=%d failed=%d, want 2 and 0", migrated, failed)
	}

	hook, err := svc.Store.GetWebhookClient(ctx, u.AccountID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hook.SecretEncrypted == hookSealed || !strings.HasPrefix(hook.SecretEncrypted, "v2.") {
		t.Fatalf("webhook secret was not rewritten: %q", hook.SecretEncrypted)
	}
	plain, err := svc.DecryptSecretAAD(app.WebhookSecretAAD(hook.AccountID, hook.InboxID), hook.SecretEncrypted)
	if err != nil || string(plain) != "hook-secret" {
		t.Fatalf("rewritten webhook secret = %q err=%v", plain, err)
	}

	conn, err := svc.Store.GetHermesConnectionByGateway(ctx, "gw-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(conn.SecretEncrypted, "v2.") || !strings.HasPrefix(conn.DeliveryKeyEncrypted, "v2.") {
		t.Fatalf("relay secrets were not rewritten: %q / %q", conn.SecretEncrypted, conn.DeliveryKeyEncrypted)
	}
	aad := app.RelaySecretAAD("gw-legacy")
	if plain, err = svc.DecryptSecretAAD(aad, conn.SecretEncrypted); err != nil || string(plain) != "relay-secret" {
		t.Fatalf("rewritten relay secret = %q err=%v", plain, err)
	}
	if plain, err = svc.DecryptSecretAAD(aad, conn.DeliveryKeyEncrypted); err != nil || string(plain) != "delivery-secret" {
		t.Fatalf("rewritten delivery key = %q err=%v", plain, err)
	}

	if migrated, failed = svc.ReencryptLegacySecrets(ctx); migrated != 0 || failed != 0 {
		t.Fatalf("second walk migrated=%d failed=%d, want 0 and 0", migrated, failed)
	}
}
