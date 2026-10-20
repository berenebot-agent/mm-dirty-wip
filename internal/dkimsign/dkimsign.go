// Package dkimsign signs outbound messages with a DomainKeys Identified Mail
// signature. It wraps github.com/emersion/go-msgauth/dkim (an approved
// dependency, decision D031) behind a small surface so the signing policy
// (canonicalisation, header set, key handling) lives in one place and can be
// exercised without a network or a live mail submission.
//
// Signing is only used by the Direct MX sending path. Every other outbound
// provider either signs with its own DKIM key or submits through its own
// authenticated infrastructure, so signing there would double-sign.
package dkimsign

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// DefaultSelector is the selector offered when an operator enables signing and
// does not choose one. Receivers find the public key through the s= tag in the
// DKIM-Signature header, so the selector is purely a namespace for rotations:
// for domain example.com it names the record at <selector>._domainkey.example.com.
const DefaultSelector = "mailmoose"

// rsaKeyBits is the generated key size. 2048 is the smallest size universally
// accepted by DKIM verifiers in the wild.
const rsaKeyBits = 2048

// ErrInvalidKey marks a private key that cannot be used for signing. It is a
// sentinel so a caller can report a user-safe validation error rather than a
// generic failure.
var ErrInvalidKey = errors.New("DKIM private key is not a usable RSA key")

// signedHeaders is the exact header set covered by the signature, in the order
// written. An explicit list (rather than dkim.Sign's "all headers" default)
// keeps the signature deterministic and immune to headers added to the message
// builder later. "From" must be present: dkim.Sign rejects a list without it.
var signedHeaders = []string{
	"From", "To", "Cc", "Subject", "Date", "Message-ID",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding",
	"In-Reply-To", "References",
}

// Sign returns raw with a DKIM-Signature header prepended. domain is the
// signing domain (the SDID a verifier looks the key up under), selector names
// the key record, and key is a parsed RSA private key.
//
// The signature deliberately carries no expiry (x=) tag. A message can wait a
// long time before it is delivered: the outbox holds mail when its domain has
// no sender, and a failed attempt is retried with backoff. An expiry would make
// a legitimate delayed retry fail verification, so a stale timestamp (t=) is
// preferred, which verifiers treat as informational rather than fatal.
func Sign(raw []byte, domain, selector string, key crypto.Signer) ([]byte, error) {
	domain = strings.TrimSpace(domain)
	selector = strings.TrimSpace(selector)
	if domain == "" {
		return nil, fmt.Errorf("DKIM signing domain is required")
	}
	if selector == "" {
		return nil, fmt.Errorf("DKIM selector is required")
	}
	if key == nil {
		return nil, ErrInvalidKey
	}
	var out bytes.Buffer
	// Relaxed/relaxed canonicalisation: the body is relayed verbatim over SMTP,
	// where intervening agents may normalise line endings or trailing
	// whitespace. Relaxed canonicalisation tolerates that, so a signature
	// survives a clean relay hop that simple canonicalisation would break.
	opts := &dkim.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 key,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             signedHeaders,
	}
	if err := dkim.Sign(&out, bytes.NewReader(raw), opts); err != nil {
		return nil, fmt.Errorf("sign message: %w", err)
	}
	return out.Bytes(), nil
}

// GenerateRSA2048 mints a fresh signing keypair, returning the private key and
// the matching public key as PEM blocks (PKCS#8 and SPKI respectively).
func GenerateRSA2048() (privatePEM, publicPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return "", "", err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	privatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	publicPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return privatePEM, publicPEM, nil
}

// ParsePrivateKey parses a PEM-encoded RSA private key in either PKCS#1
// ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY") form.
//
// Only RSA is accepted. The signing library also supports ed25519, but an
// operator-supplied key is restricted to RSA so the key type is uniform across
// generated and pasted keys; anything else is reported as ErrInvalidKey rather
// than being accepted and silently producing a signature some receivers cannot
// verify.
func ParsePrivateKey(privatePEM string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(privatePEM)))
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block found", ErrInvalidKey)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot parse the PEM block", ErrInvalidKey)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: only RSA keys are accepted", ErrInvalidKey)
	}
	return key, nil
}

// PublicKeyPEM returns the public half of a private key as an SPKI PEM block.
// It exists so the public key shown to an operator is always derived from the
// stored private key, never stored alongside it where the two could disagree.
func PublicKeyPEM(key crypto.Signer) (string, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

// PublicKeyBase64 returns the SPKI public key in the base64 form a DKIM p= tag
// requires.
func PublicKeyBase64(publicPEM string) (string, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(publicPEM)))
	if block == nil {
		return "", fmt.Errorf("%w: no PEM block found", ErrInvalidKey)
	}
	if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		return "", fmt.Errorf("%w: cannot parse the PEM block", ErrInvalidKey)
	}
	return base64.StdEncoding.EncodeToString(block.Bytes), nil
}

// CanonicalPEM normalises a PEM block for storage: surrounding whitespace is
// stripped and exactly one trailing newline is kept. It makes a save idempotent,
// so re-submitting a form without re-pasting the key leaves the stored value
// byte-identical rather than shifting it by a newline on every save.
func CanonicalPEM(pemText string) string {
	return strings.TrimSpace(pemText) + "\n"
}

// RecordName is the DNS name the public key must be published under:
// <selector>._domainkey.<domain>.
func RecordName(selector, domain string) string {
	return strings.TrimSpace(selector) + "._domainkey." + strings.TrimSpace(domain)
}

// TXTRecordValue is the copy-ready TXT record body for a public key. k=rsa is
// stated explicitly because a receiver must know the key type before it can
// verify anything.
func TXTRecordValue(publicKeyBase64 string) string {
	return "v=DKIM1; k=rsa; p=" + strings.TrimSpace(publicKeyBase64)
}

// txtChunkBytes is the maximum length of one DNS TXT character-string. A TXT
// record is a sequence of length-prefixed strings, each at most 255 bytes, so a
// value longer than this cannot be published as a single quoted string.
const txtChunkBytes = 255

// TXTRecordStrings splits a record value into the 255-byte character-strings a
// DNS provider that exposes them expects. An RSA-2048 public key makes the value
// longer than one string, so providers such as Route 53 require the value to be
// entered as several quoted parts; others accept the single long value and split
// it themselves. Returned as a slice so a caller can render either form.
func TXTRecordStrings(value string) []string {
	if len(value) <= txtChunkBytes {
		return []string{value}
	}
	var out []string
	for len(value) > txtChunkBytes {
		out = append(out, value[:txtChunkBytes])
		value = value[txtChunkBytes:]
	}
	if value != "" {
		out = append(out, value)
	}
	return out
}

// ParseTXTRecordValue extracts the p= public key from published DKIM TXT record
// strings, so a local published-record check compares against the same value a
// verifier derives. It accepts the `v=DKIM1; k=rsa; p=<base64>` form
// TXTRecordValue emits, tolerates tag reordering and the whitespace a DNS
// provider may insert inside a long base64 value, and returns ok=false when no
// record carries a non-empty p= value (an absent or malformed record, which the
// check reports as not-yet-published rather than a mismatch).
func ParseTXTRecordValue(records []string) (value string, ok bool) {
	for _, rec := range records {
		for _, tag := range strings.Split(rec, ";") {
			name, val, found := strings.Cut(tag, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "p") {
				continue
			}
			val = strings.Join(strings.Fields(val), "")
			if val != "" {
				return val, true
			}
		}
	}
	return "", false
}

// TXTRecordQuoted renders a record value in the multi-string form a DNS provider
// expects when the value is too long for one character-string, each part quoted.
func TXTRecordQuoted(value string) string {
	parts := TXTRecordStrings(value)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, `"`+part+`"`)
	}
	return strings.Join(quoted, " ")
}
