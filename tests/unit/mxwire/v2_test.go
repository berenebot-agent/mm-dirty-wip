package mxwire_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

func TestV2FrameFixtureAndStrictBounds(t *testing.T) {
	f := mxwire.Frame{Type: mxwire.FrameResolve, TxID: 7, ChannelID: 9, Payload: []byte("{}")}
	var buf bytes.Buffer
	if err := mxwire.WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}
	want := "020b000000000002000000000000000700000000000000097b7d"
	if hex.EncodeToString(buf.Bytes()) != want {
		t.Fatalf("frame = %x", buf.Bytes())
	}
	got, err := mxwire.ReadFrame(bytes.NewReader(buf.Bytes()))
	if err != nil || got.TxID != 7 || got.ChannelID != 9 || !bytes.Equal(got.Payload, f.Payload) {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	for _, index := range []int{0, 1, 2, 4} {
		bad := bytes.Clone(buf.Bytes())
		bad[index] = 255
		if _, err := mxwire.ReadFrame(bytes.NewReader(bad)); err == nil {
			t.Fatalf("accepted invalid header at %d", index)
		}
	}
	if err := mxwire.DecodeFrame(mxwire.Frame{Payload: []byte(`{"domain":"example.com","extra":true}`)}, &mxwire.DomainAuth{}); err == nil {
		t.Fatal("accepted unknown JSON field")
	}
	if err := mxwire.DecodeFrame(mxwire.Frame{Payload: []byte(`{} {}`)}, &mxwire.DomainAuth{}); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

// TestDomainAuthRegistrationMetadata pins the optional Antler MX registration
// metadata wire shape: absent for custom cores, present (and decoded) for a
// named-service core, and still strict about unknown fields.
func TestDomainAuthRegistrationMetadata(t *testing.T) {
	// A plain custom-core DomainAuth decodes with empty metadata and omits the
	// fields when re-encoded, so custom receivers never see them.
	plain, err := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "example.com", KeyID: "key-1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(plain.Payload) != `{"domain":"example.com","key_id":"key-1"}` {
		t.Fatalf("plain DomainAuth payload = %s", plain.Payload)
	}
	var decoded mxwire.DomainAuth
	if err := mxwire.DecodeFrame(plain, &decoded); err != nil || decoded.ContactEmail != "" || decoded.SetupID != "" {
		t.Fatalf("plain decode: %+v %v", decoded, err)
	}

	// An Antler core carries both fields.
	full, err := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{
		Domain: "example.com", KeyID: "key-1", ContactEmail: "ops@example.com", SetupID: "setup_ab12",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mxwire.DecodeFrame(full, &decoded); err != nil || decoded.ContactEmail != "ops@example.com" || decoded.SetupID != "setup_ab12" {
		t.Fatalf("metadata decode: %+v %v", decoded, err)
	}
	// Unknown fields remain fatal; the metadata contract is not an excuse for
	// loose decoding.
	if err := mxwire.DecodeFrame(mxwire.Frame{Payload: []byte(`{"domain":"example.com","key_id":"k","surprise":1}`)}, &mxwire.DomainAuth{}); err == nil {
		t.Fatal("accepted unknown JSON field alongside metadata")
	}
}

func TestContactEmailValidator(t *testing.T) {
	for _, ok := range []string{"ops@example.com", "a.b+tag@sub.example.co.uk", "x@example-domain.com"} {
		if !mxwire.ValidContactEmail(ok) {
			t.Fatalf("valid contact %q rejected", ok)
		}
	}
	for _, bad := range []string{"", "no-at", "@example.com", "a@", "a@b", "a@example.com ", " a@example.com", "a@example.com\n", "a b@example.com", "a@exa mple.com"} {
		if mxwire.ValidContactEmail(bad) {
			t.Fatalf("invalid contact %q accepted", bad)
		}
	}
	for _, ok := range []string{"setup_ab12", "Antler-1", "abcd"} {
		if !mxwire.ValidSetupID(ok) {
			t.Fatalf("valid setup id %q rejected", ok)
		}
	}
	for _, bad := range []string{"", "has space", "slash/", "tab\t"} {
		if mxwire.ValidSetupID(bad) {
			t.Fatalf("invalid setup id %q accepted", bad)
		}
	}
}

func TestV2ProofAndDNSSelection(t *testing.T) {
	seed, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	challenge := mxwire.Challenge{Domain: "example.com", KeyID: "key-1", ReceiverID: "rx", ConnectionID: "conn", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	transcript, err := mxwire.AuthTranscript(challenge)
	if err != nil || hex.EncodeToString(transcript) != "0083bf1af22cc8ccb013a30b015dc160b23796b1857e8116b349d3e7fbc99ddb" {
		t.Fatalf("transcript fixture: %x %v", transcript, err)
	}
	sig, err := mxwire.SignChallenge(private, challenge)
	if err != nil || !mxwire.VerifyChallenge(public, challenge, sig) {
		t.Fatalf("proof rejected: %v", err)
	}
	for _, mutate := range []func(*mxwire.Challenge){
		func(c *mxwire.Challenge) { c.Domain = "child.example.com" },
		func(c *mxwire.Challenge) { c.KeyID = "key-2" },
		func(c *mxwire.Challenge) { c.ReceiverID = "other" },
		func(c *mxwire.Challenge) { c.ConnectionID = "other" },
		func(c *mxwire.Challenge) { c.Nonce = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) },
	} {
		changed := challenge
		mutate(&changed)
		if mxwire.VerifyChallenge(public, changed, sig) {
			t.Fatal("proof accepted changed transcript")
		}
	}
	txt := mxwire.DomainTXT(challenge.KeyID, public)
	got, err := mxwire.ParseDomainTXT([]string{txt}, challenge.KeyID)
	if err != nil || !bytes.Equal(got, public) {
		t.Fatalf("TXT rejected: %v", err)
	}
	for _, records := range [][]string{nil, {txt, txt}, {txt + "; p=bad"}, {"v=MM1; k=ed25519; id=key-1; p=bad"}} {
		if _, err := mxwire.ParseDomainTXT(records, challenge.KeyID); err == nil {
			t.Fatalf("accepted invalid TXT %v", records)
		}
	}
}

func TestV2ExactDomainAndURL(t *testing.T) {
	if domain, err := mxwire.CanonicalDomain("EXAMPLE.COM."); err != nil || domain != "example.com" {
		t.Fatalf("canonicalization %q %v", domain, err)
	}
	for _, domain := range []string{"*.example.com", "example.com..", "127.0.0.1", " example.com", "é.example", "-bad.example", "bad..example", "bad\n.example"} {
		if _, err := mxwire.CanonicalDomain(domain); err == nil {
			t.Fatalf("accepted domain %q", domain)
		}
	}
	for _, raw := range []string{"http://mx.example", "https://user:secret@mx.example", "https://mx.example/path", "https://mx.example?q=x"} {
		if _, err := mxwire.ReceiverURL(raw); err == nil {
			t.Fatalf("accepted URL %q", raw)
		}
	}
}

// TestV2ReadyAdvertisedLimits pins the coordinated wire upgrade: Ready
// advertises optional limits, and Hello stays strictly versioned. A Ready with
// none of the optional fields must still decode.
func TestV2ReadyAdvertisedLimits(t *testing.T) {
	hello, err := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "gatehouse"})
	if err != nil {
		t.Fatal(err)
	}
	var gotHello mxwire.Hello
	if err := mxwire.DecodeFrame(hello, &gotHello); err != nil || gotHello.Version != mxwire.V2Protocol {
		t.Fatalf("hello rejected: %#v %v", gotHello, err)
	}

	ready, err := mxwire.JSONFrame(mxwire.FrameReady, 0, 0, mxwire.Ready{
		Version: mxwire.V2Protocol, ReceiverID: "r", ConnectionID: "c", SMTPHostname: "mx.test",
		MaxMessageBytes: 1 << 20, MaxDomains: 8, MaxAuthInflight: 4, RevalidateSeconds: 240,
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotReady mxwire.Ready
	if err := mxwire.DecodeFrame(ready, &gotReady); err != nil {
		t.Fatal(err)
	}
	if gotReady.MaxDomains != 8 || gotReady.MaxAuthInflight != 4 || gotReady.RevalidateSeconds != 240 {
		t.Fatalf("ready limits not decoded: %#v", gotReady)
	}
	// An older Ready with none of the optional fields must still decode.
	legacy, _ := mxwire.JSONFrame(mxwire.FrameReady, 0, 0, mxwire.Ready{Version: mxwire.V2Protocol, ReceiverID: "r", ConnectionID: "c", SMTPHostname: "mx.test", MaxMessageBytes: 1 << 20})
	var gotLegacy mxwire.Ready
	if err := mxwire.DecodeFrame(legacy, &gotLegacy); err != nil || gotLegacy.MaxDomains != 0 {
		t.Fatalf("legacy ready rejected: %#v %v", gotLegacy, err)
	}
	if mxwire.MaxAdvertisedDomains != 128 {
		t.Fatalf("fallback domain cap drift: %d", mxwire.MaxAdvertisedDomains)
	}
}
