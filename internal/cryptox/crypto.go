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
	"sync/atomic"

	"golang.org/x/crypto/pbkdf2"
)

// kdfSalt is a fixed application salt for the passphrase fallback. A
// per-installation random salt would be stronger, but it must be stored
// alongside the ciphertext to be recoverable and there is no place for it in
// the envelope; PBKDF2 with a fixed salt and a high iteration count is a large
// improvement over a single unsalted SHA-256.
var kdfSalt = []byte("mammothmail/app-encryption-key/v3")
var legacyKDFSalt = []byte("github.com/dellarb/mailmoose/app-encryption-key/v2")

const kdfIterations = 210_000

// derivedKeys caches derivations keyed by the SHA-256 of the raw input (so the
// raw secret is not retained as a map key). Stretching a passphrase is
// deliberately expensive and deterministic, so repeated New() calls (notably
// across tests) do not re-pay the cost. The number of distinct keys in a
// process is tiny in production; the cap bounds growth when many distinct keys
// are seen (tests).
var derivedKeys sync.Map // map[[32]byte][]byte

// derivedKeyCacheMax bounds the cache so a caller that varies the key cannot
// grow it without limit.
const derivedKeyCacheMax = 64

var derivedKeyCount atomic.Int64

func derivedCacheKey(raw string, salt []byte) [32]byte {
	return sha256.Sum256(append(append([]byte(nil), salt...), []byte(raw)...))
}

// DeriveKey returns the AES key for the given APP_ENCRYPTION_KEY. A 32-byte
// base64/hex key is used directly; a passphrase is stretched with
// PBKDF2-HMAC-SHA256 under a fixed application salt.
func DeriveKey(raw string) ([]byte, error) {
	return deriveCached(raw, kdfSalt)
}

// DeriveLegacyKey returns the former passphrase-derived key, or nil for a direct key.
func DeriveLegacyKey(raw string) ([]byte, error) {
	if _, ok := decode32(strings.TrimSpace(raw)); ok {
		return nil, nil
	}
	return deriveCached(raw, legacyKDFSalt)
}

func deriveCached(raw string, salt []byte) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	ck := derivedCacheKey(raw, salt)
	if v, ok := derivedKeys.Load(ck); ok {
		return v.([]byte), nil
	}
	key, err := deriveKey(raw, salt)
	if err != nil {
		return nil, err
	}
	if derivedKeyCount.Load() < derivedKeyCacheMax {
		if _, loaded := derivedKeys.LoadOrStore(ck, key); !loaded {
			derivedKeyCount.Add(1)
		}
	}
	return key, nil
}

func deriveKey(raw string, salt []byte) ([]byte, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty key")
	}
	if b, ok := decode32(raw); ok {
		return b, nil
	}
	if len(raw) < 24 {
		return nil, fmt.Errorf("APP_ENCRYPTION_KEY must be at least 24 characters or a 32-byte base64/hex key")
	}
	return pbkdf2.Key([]byte(raw), salt, kdfIterations, 32, sha256.New), nil
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

// EncryptWithAAD encrypts plaintext and binds the ciphertext to aad, so a blob
// copied to a different row (a different account, inbox or connection) fails to
// decrypt under its new owner. Every stored secret carries such a binding, so
// an empty aad is refused instead of writing an unbound blob.
func EncryptWithAAD(key, plaintext, aad []byte) (string, error) {
	if len(aad) == 0 {
		return "", fmt.Errorf("cryptox: an aad binding is required")
	}
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
	return envelopeAADPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// envelopeAADPrefix marks an AAD-bound ciphertext. Pre-AAD ciphertext has no
// prefix, and is refused on read.
const envelopeAADPrefix = "v2."

// DecryptWithAAD decrypts an AAD-bound ("v2.") ciphertext. A value without the
// prefix is unsupported pre-AAD ciphertext and is refused rather than read
// unbound. Legacy secret-format migration support has been retired.
func DecryptWithAAD(key []byte, encoded string, aad []byte) ([]byte, error) {
	if !strings.HasPrefix(encoded, envelopeAADPrefix) {
		return nil, fmt.Errorf("cryptox: unsupported pre-AAD ciphertext")
	}
	if len(aad) == 0 {
		return nil, fmt.Errorf("cryptox: an aad binding is required")
	}
	return open(key, strings.TrimPrefix(encoded, envelopeAADPrefix), aad)
}

func open(key []byte, encoded string, aad []byte) ([]byte, error) {
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
