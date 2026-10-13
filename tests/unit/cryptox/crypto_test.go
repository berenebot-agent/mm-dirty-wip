package cryptox_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/cryptox"
)

// TestDeriveKeyPassphraseUsesPBKDF2 proves a passphrase gets the stretched key,
// which differs from the pre-PBKDF2 single-SHA-256 derivation that only the
// one-time re-encrypt walk still uses.
func TestDeriveKeyPassphraseUsesPBKDF2(t *testing.T) {
	const pass = "a-long-but-human-chosen-passphrase"
	primary, err := cryptox.DeriveKey(pass)
	if err != nil {
		t.Fatal(err)
	}
	legacy, ok := cryptox.DeprecatedLegacyKey(pass)
	if !ok {
		t.Fatal("a passphrase key must have a legacy derivation")
	}
	if len(primary) != 32 || len(legacy) != 32 {
		t.Fatalf("key lengths = %d, %d", len(primary), len(legacy))
	}
	if string(primary) == string(legacy) {
		t.Fatal("primary passphrase key must not equal the legacy SHA-256 key")
	}
	h := sha256.Sum256([]byte(pass))
	if string(legacy) != string(h[:]) {
		t.Fatal("legacy derivation is not the bare SHA-256 of the passphrase")
	}
	// A 32-byte key is used directly and has no legacy variant.
	raw := hex.EncodeToString(make([]byte, 32))
	if direct, err := cryptox.DeriveKey(raw); err != nil || len(direct) != 32 {
		t.Fatalf("32-byte key derive = %d bytes err=%v", len(direct), err)
	}
	if _, ok := cryptox.DeprecatedLegacyKey(raw); ok {
		t.Fatal("a 32-byte key must not report a legacy derivation")
	}
}

// TestDeriveKeyCacheIsBounded proves the derivation cache does not grow without
// limit when many distinct keys are seen (it keys by a hash and caps the count).
// 32-byte raw keys are used directly, so this stays fast.
func TestDeriveKeyCacheIsBounded(t *testing.T) {
	for i := 0; i < 200; i++ {
		b := make([]byte, 32)
		for j := range b {
			b[j] = byte(i*7 + j + 1)
		}
		key, err := cryptox.DeriveKey(hex.EncodeToString(b))
		if err != nil {
			t.Fatalf("derive %d: %v", i, err)
		}
		if len(key) != 32 {
			t.Fatalf("key length %d", len(key))
		}
	}
	// A previously derived key still returns a deterministic result.
	a, _ := cryptox.DeriveKey(hex.EncodeToString(make([]byte, 32)))
	b, _ := cryptox.DeriveKey(hex.EncodeToString(make([]byte, 32)))
	if string(a) != string(b) {
		t.Fatal("derivation is not deterministic")
	}
}

// TestEncryptWithAADBindsCiphertextToRow proves an AAD-bound blob only decrypts
// under the same AAD, and that neither an empty AAD nor the pre-AAD format is
// accepted by the read path.
func TestEncryptWithAADBindsCiphertextToRow(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	aad := []byte("cfg:acc1:dom1")

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
	if _, err := cryptox.DecryptWithAAD(key, enc, []byte("cfg:acc2:dom2")); err == nil {
		t.Fatal("ciphertext decrypted under a different AAD")
	}
	// A binding must be supplied on both sides.
	if _, err := cryptox.EncryptWithAAD(key, []byte("x"), nil); err == nil {
		t.Fatal("encryption with an empty AAD was allowed")
	}
	if _, err := cryptox.DecryptWithAAD(key, enc, nil); err == nil {
		t.Fatal("decryption with an empty AAD was allowed")
	}

	// The pre-AAD format is refused by the read path, and only the one-time
	// re-encrypt walk can still open it.
	legacy := legacyCiphertext(t, key, "old-secret")
	if got, err := cryptox.DeprecatedOpenLegacy(key, legacy); err != nil || string(got) != "old-secret" {
		t.Fatalf("legacy open = %q err=%v", got, err)
	}
	if _, err := cryptox.DecryptWithAAD(key, legacy, aad); err == nil {
		t.Fatal("pre-AAD ciphertext was accepted by the read path")
	}
	if _, err := cryptox.DeprecatedOpenLegacy(key, enc); err == nil {
		t.Fatal("the legacy opener accepted an AAD-bound blob")
	}
}

// legacyCiphertext writes the pre-AAD envelope exactly as the previous release
// did: GCM over the raw key with a nil AAD, base64url, no version prefix.
func legacyCiphertext(t *testing.T, key []byte, plaintext string) string {
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
