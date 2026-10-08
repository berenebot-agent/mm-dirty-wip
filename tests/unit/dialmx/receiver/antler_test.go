package receiver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestAntlerRegistrationMetadataLogged proves the optional Antler MX contact
// email and setup id supplied on DomainAuth are carried through the domain
// proof and message handoff metadata, and that the proof itself still succeeds
// from DNS authority alone (the metadata is never authority).
func TestAntlerRegistrationMetadataLogged(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	sink := &jsonSink{}
	_, srv, log := newLoggedServerLogger(t, sink, receiver.Config{LookupTXT: dns})

	be := &backend{domains: []mxdial.Domain{{
		Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL},
		ContactEmail: "ops@example.test", SetupID: "setup_abc123",
	}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: rootTLS(t, srv), ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")
	send(t, startEdgeWithLogger(t, 2, srv.URL, log), []string{"alice@example.test"})

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		for _, rec := range allRecords(recs, "dialmx domain auth") {
			if rec["phase"] == "registration" && rec["contact_email"] == "ops@example.test" {
				return true
			}
		}
		return false
	})
	for _, rec := range allRecords(sink.records(t), "dialmx domain auth") {
		if rec["phase"] == "registration" {
			if rec["contact_email"] != "ops@example.test" || rec["setup_id"] != "setup_abc123" {
				t.Fatalf("registration metadata missing: %v", rec)
			}
		}
	}
	waitForRecords(t, sink, func(recs []map[string]any) bool {
		for _, rec := range allRecords(recs, "dialmx handoff result") {
			if rec["contact_email"] == "ops@example.test" && rec["setup_id"] == "setup_abc123" {
				return true
			}
		}
		return false
	})
}

// TestAntlerMalformedMetadataDroppedWithoutRejectingProof proves a malformed
// contact email or setup id never becomes authority: it is dropped and the
// DNS-anchored proof still authenticates the domain.
func TestAntlerMalformedMetadataDroppedWithoutRejectingProof(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: rootTLS(t, srv), ForceAttemptHTTP2: true}}
	s := newRawSession(t, client, srv.URL)
	defer s.close()
	auth, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{
		Domain: "example.test", KeyID: "key1", ContactEmail: "not-an-email", SetupID: "bad id!",
	})
	s.write(auth)

	// The receiver answers the challenge/proof exchange and ultimately accepts
	// the domain despite the dropped metadata.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f := s.read()
		switch f.Type {
		case mxwire.FrameChallenge:
			var c mxwire.Challenge
			if mxwire.DecodeFrame(f, &c) != nil {
				t.Fatal("bad challenge frame")
			}
			sig, err := mxwire.SignChallenge(priv, c)
			if err != nil {
				t.Fatal(err)
			}
			resp, _ := mxwire.JSONFrame(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
			s.write(resp)
		case mxwire.FrameAuthResult:
			var a mxwire.AuthResult
			if mxwire.DecodeFrame(f, &a) != nil {
				t.Fatal("bad auth result")
			}
			if a.Accepted {
				return
			}
		case mxwire.FramePing:
			pong, _ := mxwire.JSONFrame(mxwire.FramePong, 0, 0, struct{}{})
			s.write(pong)
		}
		if time.Now().After(deadline) {
			t.Fatal("proof never accepted")
		}
	}
}

// TestBrowserRootRedirect proves GET / serves an HTML browser a 302 to the
// configured landing page, while a non-HTML client gets a plain 404 and the
// configured redirect never affects the session or health routes.
func TestBrowserRootRedirect(t *testing.T) {
	r := newReceiver(t, receiver.Config{BrowserRedirectURL: "https://github.com/dellarb/mailmoose"})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://github.com/dellarb/mailmoose" {
		t.Fatalf("browser root = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-HTML root = %d, want 404", resp.StatusCode)
	}

	resp, err = client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
}

// TestBrowserRootWithoutConfigDoesNotRedirect pins the default: no redirect
// configured means the root is an ordinary 404 for every client.
func TestBrowserRootWithoutConfigDoesNotRedirect(t *testing.T) {
	r := newReceiver(t, receiver.Config{})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured root = %d, want 404", resp.StatusCode)
	}
}
