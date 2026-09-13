package cryptox_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"gatehouse-mail/internal/cryptox"
)

// TestDeriveKeysPassphraseUsesPBKDF2AndKeepsLegacyReadable verifies a
// passphrase gets the stronger primary key while the legacy single-SHA-256 key
// is retained so pre-upgrade ciphertext still decrypts.
func TestDeriveKeysPassphraseUsesPBKDF2AndKeepsLegacyReadable(t *testing.T) {
	const pass = "a-long-but-human-chosen-passphrase"
	primary, candidates, err := cryptox.DeriveKeys(pass)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2 (primary + legacy)", len(candidates))
	}
	legacy := sha256.Sum256([]byte(pass))

	// Ciphertext written with the legacy key must still decrypt via the
	// candidate list (this is the pre-upgrade data path).
	encLegacy, err := cryptox.Encrypt(legacy[:], []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cryptox.DecryptFirst(candidates, encLegacy)
	if err != nil {
		t.Fatalf("legacy ciphertext no longer decrypts: %v", err)
	}
	if string(got) != "provider-secret" {
		t.Fatalf("legacy plaintext = %q", got)
	}

	// New writes use the primary (PBKDF2) key, which must differ from the
	// legacy key.
	if string(primary) == string(legacy[:]) {
		t.Fatal("primary passphrase key must not equal the legacy SHA-256 key")
	}
	encNew, err := cryptox.Encrypt(primary, []byte("new-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cryptox.Decrypt(legacy[:], encNew); err == nil {
		t.Fatal("new ciphertext must not decrypt with the legacy key")
	}
	got, err = cryptox.DecryptFirst(candidates, encNew)
	if err != nil || string(got) != "new-secret" {
		t.Fatalf("new ciphertext decrypt = %q err=%v", got, err)
	}
}

// TestDeriveKeys32ByteKeyHasNoLegacyVariant proves a raw 32-byte key is used
// directly with a single candidate and no passphrase stretching.
func TestDeriveKeys32ByteKeyHasNoLegacyVariant(t *testing.T) {
	raw := strings.Repeat("ab", 32) // 64 hex chars -> 32 raw bytes
	primary, candidates, err := cryptox.DeriveKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(primary) != 32 {
		t.Fatalf("primary length %d", len(primary))
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
}
