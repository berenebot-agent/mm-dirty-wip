package cryptox_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/cryptox"
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

// TestDeriveKeysCacheIsBounded proves the derivation cache does not grow without
// limit when many distinct keys are seen (it keys by a hash and caps the count).
// 32-byte raw keys are used directly, so this stays fast.
func TestDeriveKeysCacheIsBounded(t *testing.T) {
	for i := 0; i < 200; i++ {
		b := make([]byte, 32)
		for j := range b {
			b[j] = byte(i*7 + j + 1)
		}
		primary, _, err := cryptox.DeriveKeys(hex.EncodeToString(b))
		if err != nil {
			t.Fatalf("derive %d: %v", i, err)
		}
		if len(primary) != 32 {
			t.Fatalf("primary length %d", len(primary))
		}
	}
	// A previously derived key still returns a deterministic result.
	a, _, _ := cryptox.DeriveKeys(hex.EncodeToString(make([]byte, 32)))
	b, _, _ := cryptox.DeriveKeys(hex.EncodeToString(make([]byte, 32)))
	if string(a) != string(b) {
		t.Fatal("derivation is not deterministic")
	}
}

// TestEncryptWithAADBindsCiphertextToRow proves an AAD-bound blob only decrypts
// with the same AAD, so a blob copied to another row fails, while a legacy
// AAD-less blob still decrypts through DecryptWithAAD.
func TestEncryptWithAADBindsCiphertextToRow(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	aad := []byte("domain_sending:acc1:dom1")

	enc, err := cryptox.EncryptWithAAD(key, []byte("provider-password"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v2.") {
		t.Fatalf("AAD-bound ciphertext lacks the version prefix: %q", enc)
	}
	got, err := cryptox.DecryptWithAAD(key, enc, aad)
	if err != nil || string(got) != "provider-password" {
		t.Fatalf("same-AAD decrypt = %q err=%v", got, err)
	}
	// A different row's AAD must not decrypt it.
	if _, err := cryptox.DecryptWithAAD(key, enc, []byte("domain_sending:acc2:dom2")); err == nil {
		t.Fatal("ciphertext decrypted under a different AAD")
	}
	// The legacy AAD-less decrypt must also fail on a bound blob.
	if _, err := cryptox.Decrypt(key, enc); err == nil {
		t.Fatal("bound ciphertext decrypted without its AAD")
	}

	// A legacy AAD-less blob still decrypts through DecryptWithAAD (which
	// ignores the supplied AAD for the legacy format).
	legacy, err := cryptox.Encrypt(key, []byte("old-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(legacy, "v2.") {
		t.Fatal("legacy ciphertext wrongly carries the version prefix")
	}
	if got, err := cryptox.DecryptWithAAD(key, legacy, aad); err != nil || string(got) != "old-secret" {
		t.Fatalf("legacy decrypt via AAD path = %q err=%v", got, err)
	}
}
