package dkimsign_test

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/dkimsign"
	"github.com/emersion/go-msgauth/dkim"
)

const sampleMessage = "From: Alice <alice@example.com>\r\n" +
	"To: bob@example.net\r\n" +
	"Subject: hello\r\n" +
	"Date: Mon, 12 Oct 2026 10:00:00 +0000\r\n" +
	"Message-ID: <abc@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"body text\r\n"

// verifyWith runs the real verifier against a public key looked up through the
// DNS TXT record value we would hand an operator. It is the whole point of the
// signature: if this does not pass, the record an operator publishes is wrong.
func verifyWith(t *testing.T, signed []byte, domain, selector string, publicPEM string) []*dkim.Verification {
	t.Helper()
	b64, err := dkimsign.PublicKeyBase64(publicPEM)
	if err != nil {
		t.Fatalf("PublicKeyBase64: %v", err)
	}
	txt := dkimsign.TXTRecordValue(b64)
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			if want := dkimsign.RecordName(selector, domain); name != want {
				t.Errorf("verifier looked up %q, want %q", name, want)
			}
			return []string{txt}, nil
		},
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	return verifs
}

// TestGeneratedKeySignsAndVerifies is the load-bearing test for this package:
// a key Mammoth Mail generates must produce a signature that verifies against the
// public key Mammoth Mail hands the operator. If these two ever diverge, mail is
// silently sent unsigned-but-looking-signed.
func TestGeneratedKeySignsAndVerifies(t *testing.T) {
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	key, err := dkimsign.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	signed, err := dkimsign.Sign([]byte(sampleMessage), "example.com", "mammothmail", key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	verifs := verifyWith(t, signed, "example.com", "mammothmail", pubPEM)
	if len(verifs) != 1 {
		t.Fatalf("got %d signatures, want exactly 1", len(verifs))
	}
	if verifs[0].Err != nil {
		t.Fatalf("signature did not verify: %v", verifs[0].Err)
	}
	if verifs[0].Domain != "example.com" {
		t.Errorf("verified domain = %q, want example.com", verifs[0].Domain)
	}
}

// TestSignPrependsSignatureHeader pins the single-signature property the retry
// path depends on: signing once adds exactly one DKIM-Signature header.
func TestSignPrependsSignatureHeader(t *testing.T) {
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	key, err := dkimsign.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	signed, err := dkimsign.Sign([]byte(sampleMessage), "example.com", "mammothmail", key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := strings.Count(string(signed), "DKIM-Signature:"); n != 1 {
		t.Fatalf("got %d DKIM-Signature headers, want 1", n)
	}
	// The original message must survive verbatim after the added header.
	if !bytes.HasSuffix(signed, []byte(sampleMessage)) {
		t.Error("signed message does not end with the original message bytes")
	}
	// No expiry tag: a delayed retry must still verify.
	head := string(signed[:strings.Index(string(signed), "\r\n\r\n")])
	if strings.Contains(head, " x=") {
		t.Error("signature carries an expiry (x=) tag; a delayed retry would fail")
	}
}

func TestSignRejectsEmptyInputs(t *testing.T) {
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	key, err := dkimsign.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	for name, tc := range map[string]struct{ domain, selector string }{
		"no domain":   {"", "mammothmail"},
		"no selector": {"example.com", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := dkimsign.Sign([]byte(sampleMessage), tc.domain, tc.selector, key); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := dkimsign.Sign([]byte(sampleMessage), "example.com", "mammothmail", nil); err == nil {
		t.Fatal("expected an error for a nil key")
	}
}

func TestParsePrivateKeyAcceptsPKCS1AndPKCS8(t *testing.T) {
	// PKCS#1 is the format `openssl genrsa` writes, which an operator pasting a
	// pre-existing key is most likely to supply.
	pkcs1 := testPKCS1KeyPEM(t)
	for name, pemText := range map[string]string{
		"pkcs8 (generated form)": mustGenerate(t),
		"pkcs1 (openssl genrsa)": pkcs1,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := dkimsign.ParsePrivateKey(pemText); err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
		})
	}
}

// TestParsePrivateKeyRejectsNonRSA proves an ed25519 key is refused rather than
// accepted and used to emit a signature some receivers cannot verify.
func TestParsePrivateKeyRejectsNonRSA(t *testing.T) {
	ed25519PEM := "-----BEGIN PRIVATE KEY-----\n" +
		"MC4CAQAwBQYDK2VwBCIEINQ1T4l5h1lA2m0m9Y5aV3kL9oO0n1k2m3p4q5r6s7t8\n" +
		"-----END PRIVATE KEY-----\n"
	if _, err := dkimsign.ParsePrivateKey(ed25519PEM); err == nil {
		t.Fatal("expected ed25519 key to be rejected")
	}
	for name, bad := range map[string]string{
		"empty":      "",
		"not pem":    "hello",
		"garbage":    "-----BEGIN PRIVATE KEY-----\nnot base64!\n-----END PRIVATE KEY-----\n",
		"public key": testPublicOnlyPEM(t),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := dkimsign.ParsePrivateKey(bad); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestPublicKeyPEMIsDerivedFromPrivateKey pins the invariant that the displayed
// public key always matches the stored private key, so a mismatched pair can
// never be presented to an operator as a DNS record.
func TestPublicKeyPEMIsDerivedFromPrivateKey(t *testing.T) {
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	key, err := dkimsign.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	derived, err := dkimsign.PublicKeyPEM(key)
	if err != nil {
		t.Fatalf("PublicKeyPEM: %v", err)
	}
	if strings.TrimSpace(derived) != strings.TrimSpace(pubPEM) {
		t.Error("derived public key differs from the generated public key")
	}
}

func TestRecordNameAndTXTValue(t *testing.T) {
	if got, want := dkimsign.RecordName("mammothmail", "example.com"), "mammothmail._domainkey.example.com"; got != want {
		t.Errorf("RecordName = %q, want %q", got, want)
	}
	got := dkimsign.TXTRecordValue("QUJDMTIz")
	if want := "v=DKIM1; k=rsa; p=QUJDMTIz"; got != want {
		t.Errorf("TXTRecordValue = %q, want %q", got, want)
	}
	// The p= value must itself be valid base64 so a receiver can decode it.
	if _, err := base64.StdEncoding.DecodeString("QUJDMTIz"); err != nil {
		t.Fatalf("test fixture is not base64: %v", err)
	}
}

// TestParseTXTRecordValue proves the published-record check reads the p= value a
// verifier derives, tolerating tag reordering and the whitespace a DNS provider
// may insert inside a long base64 value, and reports absence honestly.
func TestParseTXTRecordValue(t *testing.T) {
	const key = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A"
	for _, tc := range []struct {
		name    string
		records []string
		want    string
		ok      bool
	}{
		{"the canonical record", []string{"v=DKIM1; k=rsa; p=" + key}, key, true},
		{"tags may be reordered", []string{"k=rsa; p=" + key + "; v=DKIM1"}, key, true},
		{"whitespace inside the key collapses", []string{"v=DKIM1; k=rsa; p=" + key[:8] + " \n " + key[8:]}, key, true},
		{"another record in the set is skipped", []string{"v=spf1 -all", "p=" + key}, key, true},
		{"no p= tag yields not-ok", []string{"v=DKIM1; k=rsa"}, "", false},
		{"an empty p= yields not-ok", []string{"v=DKIM1; p="}, "", false},
		{"no records yields not-ok", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dkimsign.ParseTXTRecordValue(tc.records)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseTXTRecordValue(%q) = (%q, %v), want (%q, %v)", tc.records, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func mustGenerate(t *testing.T) string {
	t.Helper()
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	return privPEM
}

// testPKCS1KeyPEM re-encodes a generated key in PKCS#1 form, mirroring what
// `openssl genrsa` writes for an operator pasting their own key.
func testPKCS1KeyPEM(t *testing.T) string {
	t.Helper()
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		t.Fatal("no PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("generated key is %T, want *rsa.PrivateKey", parsed)
	}
	der := x509.MarshalPKCS1PrivateKey(rsaKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func testPublicOnlyPEM(t *testing.T) string {
	t.Helper()
	_, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatalf("GenerateRSA2048: %v", err)
	}
	return pubPEM
}

// TestTXTRecordSplitsLongValues pins the DNS limit that a real RSA-2048 key
// hits: a TXT record is a sequence of at most 255-byte character-strings, so a
// longer value must be offered in the split form some providers require.
func TestTXTRecordSplitsLongValues(t *testing.T) {
	// A short value is one string and needs no split.
	short := dkimsign.TXTRecordValue("QUJDMTIz")
	if got := dkimsign.TXTRecordStrings(short); len(got) != 1 || got[0] != short {
		t.Errorf("short value split unexpectedly: %q", got)
	}
	if got := dkimsign.TXTRecordQuoted(short); got != `"`+short+`"` {
		t.Errorf("short value quoted = %q", got)
	}

	// A real generated key produces a value longer than one string.
	_, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	b64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	value := dkimsign.TXTRecordValue(b64)
	if len(value) <= 255 {
		t.Fatalf("an RSA-2048 record value is %d bytes; the test expects it to exceed one string", len(value))
	}
	parts := dkimsign.TXTRecordStrings(value)
	if len(parts) < 2 {
		t.Fatalf("long value produced %d parts, want at least 2", len(parts))
	}
	for i, p := range parts {
		if len(p) > 255 {
			t.Errorf("part %d is %d bytes, over the 255-byte DNS limit", i, len(p))
		}
	}
	// The split must rejoin to exactly the original value: a receiver
	// concatenates the strings, so any change would break verification.
	if got := strings.Join(parts, ""); got != value {
		t.Error("the split parts do not rejoin to the original value")
	}
	// Boundaries: exactly 255 stays whole; 256 splits 255+1.
	if got := dkimsign.TXTRecordStrings(strings.Repeat("a", 255)); len(got) != 1 {
		t.Errorf("255 bytes produced %d parts, want 1", len(got))
	}
	if got := dkimsign.TXTRecordStrings(strings.Repeat("a", 256)); len(got) != 2 || len(got[0]) != 255 || len(got[1]) != 1 {
		t.Errorf("256 bytes split as %v, want 255+1", got)
	}
}

// TestTXTRecordQuotedEscapesNothing proves the quoted form is a faithful
// wrapping: the value itself is base64 plus fixed ASCII tags, so no part needs
// escaping and a receiver sees exactly the original bytes.
func TestTXTRecordQuotedPreservesValue(t *testing.T) {
	_, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	b64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	value := dkimsign.TXTRecordValue(b64)
	quoted := dkimsign.TXTRecordQuoted(value)
	// Strip the wrapping quotes and separating spaces, then compare.
	stripped := strings.ReplaceAll(quoted, `" "`, "")
	stripped = strings.TrimPrefix(stripped, `"`)
	stripped = strings.TrimSuffix(stripped, `"`)
	if stripped != value {
		t.Error("the quoted form does not round-trip to the original value")
	}
	if !strings.HasPrefix(quoted, `"`) || !strings.HasSuffix(quoted, `"`) {
		t.Errorf("quoted form is not quoted: %q", quoted)
	}
}
