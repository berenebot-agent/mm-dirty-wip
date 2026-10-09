package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/pbkdf2"
)

// kdfSalt is a fixed application salt for the passphrase fallback. A
// per-installation random salt would be stronger, but it must be stored
// alongside the ciphertext to be recoverable and there is no place for it in
// the pre-existing envelope; PBKDF2 with a fixed salt and a high iteration
// count is still a large, backward-compatible improvement over a single
// unsalted SHA-256.
var kdfSalt = []byte("github.com/dellarb/mailmoose/app-encryption-key/v2")

const kdfIterations = 210_000

// DeriveKey returns the key for the given APP_ENCRYPTION_KEY. A 32-byte
// base64/hex key is used directly; a passphrase is stretched with
// PBKDF2-HMAC-SHA256.
func DeriveKey(raw string) ([]byte, error) {
	primary, _, err := DeriveKeys(raw)
	return primary, err
}

// derivedKeys caches derivations by raw input. Stretching a passphrase is
// deliberately expensive, and the result is deterministic, so repeated New()
// calls (notably across tests) do not re-pay the cost.
var derivedKeys sync.Map // map[string]derived

type derived struct {
	primary    []byte
	candidates [][]byte
	err        error
}

// DeriveKeys returns the primary encryption key and every key that may still
// decrypt existing ciphertext, newest first. The second key is the legacy
// single-SHA-256 derivation of a passphrase, retained so data encrypted before
// the PBKDF2 change keeps decrypting; a 32-byte key has no legacy variant.
func DeriveKeys(raw string) (primary []byte, candidates [][]byte, err error) {
	raw = strings.TrimSpace(raw)
	if v, ok := derivedKeys.Load(raw); ok {
		d := v.(derived)
		return d.primary, d.candidates, d.err
	}
	primary, candidates, err = deriveKeys(raw)
	derivedKeys.Store(raw, derived{primary, candidates, err})
	return primary, candidates, err
}

func deriveKeys(raw string) (primary []byte, candidates [][]byte, err error) {
	if raw == "" {
		return nil, nil, fmt.Errorf("empty key")
	}
	if b, ok := decode32(raw); ok {
		return b, [][]byte{b}, nil
	}
	if len(raw) < 24 {
		return nil, nil, fmt.Errorf("APP_ENCRYPTION_KEY must be at least 24 characters or a 32-byte base64/hex key")
	}
	primary = pbkdf2.Key([]byte(raw), kdfSalt, kdfIterations, 32, sha256.New)
	h := sha256.Sum256([]byte(raw))
	legacy := make([]byte, len(h))
	copy(legacy, h[:])
	return primary, [][]byte{primary, legacy}, nil
}

// decode32 decodes a 32-byte key from base64 (raw or std) or hex.
func decode32(raw string) ([]byte, bool) {
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	return nil, false
}

// DecryptFirst tries each key in order and returns the first successful
// plaintext, so a rotated or upgraded derivation still reads old ciphertext.
func DecryptFirst(keys [][]byte, encoded string) ([]byte, error) {
	return DecryptFirstAAD(keys, encoded, nil)
}

// DecryptFirstAAD is DecryptFirst with an AAD binding. An AAD-bound ("v2.")
// blob requires the same aad; a legacy blob ignores it, so a migrated call site
// reads both old and new rows.
func DecryptFirstAAD(keys [][]byte, encoded string, aad []byte) ([]byte, error) {
	var last error
	for _, k := range keys {
		b, err := DecryptWithAAD(k, encoded, aad)
		if err == nil {
			return b, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("no decryption key available")
	}
	return nil, last
}

func Encrypt(key, plaintext []byte) (string, error) {
	return EncryptWithAAD(key, plaintext, nil)
}

// EncryptWithAAD encrypts plaintext and binds the ciphertext to aad, so a blob
// copied to a different row (a different account/domain/kind) fails to decrypt
// under its new owner. The envelope is versioned: a "v2." prefix marks an
// AAD-bound blob, while an unprefixed value is the legacy AAD-less format.
// Passing a nil/empty aad produces the legacy format, preserving compatibility
// for callers not yet migrated.
func EncryptWithAAD(key, plaintext, aad []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	out := append(nonce, ct...)
	enc := base64.RawURLEncoding.EncodeToString(out)
	if len(aad) == 0 {
		return enc, nil
	}
	return envelopeAADPrefix + enc, nil
}

// envelopeAADPrefix marks an AAD-bound ciphertext. The legacy format has no
// prefix, so DecryptWithAAD can tell them apart.
const envelopeAADPrefix = "v2."

// Decrypt decrypts a legacy (AAD-less) ciphertext.
func Decrypt(key []byte, encoded string) ([]byte, error) {
	return DecryptWithAAD(key, encoded, nil)
}

// DecryptWithAAD decrypts a ciphertext, using aad when the blob is AAD-bound (a
// "v2." prefix) and ignoring the supplied aad for a legacy blob. This lets a
// migrated call site read both old and new rows.
func DecryptWithAAD(key []byte, encoded string, aad []byte) ([]byte, error) {
	bound := strings.HasPrefix(encoded, envelopeAADPrefix)
	if bound {
		encoded = strings.TrimPrefix(encoded, envelopeAADPrefix)
	} else {
		aad = nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], aad)
}
