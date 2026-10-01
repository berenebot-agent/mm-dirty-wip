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
