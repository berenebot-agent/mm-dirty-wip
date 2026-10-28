package app_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/cryptox"
	"golang.org/x/crypto/pbkdf2"
)

// Service construction changes the process-global outbound policy.
func TestPassphraseEncryptionCompatibility(t *testing.T) {
	const passphrase = "a-long-but-human-chosen-passphrase"
	const aad = "cfg:account:domain"
	legacy := pbkdf2.Key([]byte(passphrase), []byte("github.com/dellarb/mailmoose/app-encryption-key/v2"), 210_000, 32, sha256.New)
	svc, err := app.New(config.Config{AppEncryptionKey: passphrase, DataDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := cryptox.EncryptWithAAD(legacy, []byte("old"), []byte(aad))
	if err != nil {
		t.Fatal(err)
	}
	current, err := svc.EncryptSecretAAD(aad, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	for ciphertext, want := range map[string]string{old: "old", current: "new"} {
		got, err := svc.DecryptSecretAAD(aad, ciphertext)
		if err != nil || string(got) != want {
			t.Fatalf("decrypt = %q, %v; want %q", got, err, want)
		}
		if _, err := svc.DecryptSecretAAD("wrong", ciphertext); err == nil {
			t.Fatal("wrong AAD accepted")
		}
	}
	if _, err := cryptox.DecryptWithAAD(legacy, current, []byte(aad)); err == nil {
		t.Fatal("new ciphertext used legacy key")
	}
	wrong, err := app.New(config.Config{AppEncryptionKey: "a-different-long-passphrase", DataDir: t.TempDir()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.DecryptSecretAAD(aad, old); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := svc.DecryptSecretAAD(aad, "unbound"); err == nil {
		t.Fatal("unbound ciphertext accepted")
	}
}

func TestLegacyDirectKey(t *testing.T) {
	t.Parallel()
	raw := hex.EncodeToString(make([]byte, 32))
	legacy, err := cryptox.DeriveLegacyKey(raw)
	if err != nil || legacy != nil {
		t.Fatalf("legacy direct key = %x, %v; want nil", legacy, err)
	}
}
