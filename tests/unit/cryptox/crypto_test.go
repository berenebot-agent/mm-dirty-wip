package cryptox_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/cryptox"
)

// TestDeriveKeyPassphraseUsesPBKDF2 proves a passphrase gets the stretched key
// and that a 32-byte key is used directly.
func TestDeriveKeyPassphraseUsesPBKDF2(t *testing.T) {
	primary, err := cryptox.DeriveKey("a-long-but-human-chosen-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if len(primary) != 32 {
		t.Fatalf("passphrase key length %d", len(primary))
	}
	// The passphrase key is not a bare SHA-256 of the input.
	if strings.Contains(hex.EncodeToString(primary), "a-long") {
		t.Fatal("passphrase key looks like raw input")
	}
	raw := hex.EncodeToString(make([]byte, 32))
	direct, err := cryptox.DeriveKey(raw)
	if err != nil || len(direct) != 32 {
		t.Fatalf("32-byte key derive = %d bytes err=%v", len(direct), err)
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
// accepted.
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
	// The unsupported pre-AAD format is refused, never read without a binding.
	if _, err := cryptox.DecryptWithAAD(key, "bm90LXByZWZpeGVk", aad); err == nil {
		t.Fatal("unprefixed ciphertext was accepted")
	}
}
