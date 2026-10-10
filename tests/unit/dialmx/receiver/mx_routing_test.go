package receiver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// authOnce drives a full DomainAuth / Challenge / ChallengeResponse exchange on a
// fresh channel and returns the receiver's AuthResult. The TXT proof is always
// valid; mxFn supplies the gate-2 MX answer.
func authOnce(t *testing.T, s *rawSession, priv ed25519.PrivateKey, ch uint64, domain string) mxwire.AuthResult {
	t.Helper()
	auth, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, ch, mxwire.DomainAuth{Domain: domain, KeyID: "key1"})
	s.write(auth)
	f := s.read()
	switch f.Type {
	case mxwire.FrameChallenge:
	case mxwire.FrameAuthResult:
		var a mxwire.AuthResult
		if mxwire.DecodeFrame(f, &a) != nil {
			t.Fatalf("bad immediate auth result")
		}
		return a
	default:
		t.Fatalf("expected Challenge or AuthResult, got frame %d", f.Type)
	}
	var c mxwire.Challenge
	if mxwire.DecodeFrame(f, &c) != nil {
		t.Fatal("bad challenge")
	}
	sig, err := mxwire.SignChallenge(priv, c)
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := mxwire.JSONFrame(mxwire.FrameChallengeResponse, 0, ch, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
	s.write(proof)
	var a mxwire.AuthResult
	if mxwire.DecodeFrame(s.expect(t, mxwire.FrameAuthResult), &a) != nil {
		t.Fatal("bad auth result")
	}
	return a
}

// TestGate2NotMXRejectsInitialAuth proves the routing gate: even with a valid
// TXT authority proof, a receiver whose own SMTP hostname is not in the domain's
// MX answers rejected/not_mx rather than installing a binding.
func TestGate2NotMXRejectsInitialAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	txt := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	mx := func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: "someone-else.test."}}, nil
	}
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: txt, LookupMX: mx})
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	s := newRawSession(t, hc, srv.URL)
	defer s.close()

	a := authOnce(t, s, priv, 1, "example.test")
	if a.Accepted || a.Reason != "not_mx" {
		t.Fatalf("gate-2 = %+v, want rejected not_mx", a)
	}
}

// TestGate2MXPresentAccepts proves the positive path: a receiver that is named in
// the domain's MX completes the binding as before.
func TestGate2MXPresentAccepts(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	txt := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	mx := func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: "MX.TEST."}}, nil
	}
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: txt, LookupMX: mx})
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	s := newRawSession(t, hc, srv.URL)
	defer s.close()

	a := authOnce(t, s, priv, 1, "example.test")
	if !a.Accepted {
		t.Fatalf("gate-2 acceptance = %+v, want accepted", a)
	}
}

// TestGate2ResolverErrorIsDeferrable proves a transient MX resolver failure is
// NOT the not_mx verdict: the receiver reports the retryable dns_unavailable and
// reserves not_mx for a successful lookup that omits it.
func TestGate2ResolverErrorIsDeferrable(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	txt := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	mx := func(context.Context, string) ([]*net.MX, error) { return nil, errors.New("resolver down") }
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: txt, LookupMX: mx})
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	s := newRawSession(t, hc, srv.URL)
	defer s.close()

	a := authOnce(t, s, priv, 1, "example.test")
	if a.Accepted || a.Reason != "dns_unavailable" {
		t.Fatalf("resolver error = %+v, want rejected dns_unavailable", a)
	}
}
