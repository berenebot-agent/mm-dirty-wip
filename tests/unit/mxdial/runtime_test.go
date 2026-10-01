package mxdial_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func TestSessionAuthResolveAndIngest(t *testing.T) {
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	backend := &scriptedBackend{}
	payload := []byte("raw MIME\r\nbody")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s", r.Proto)
		}
		f, e := mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameHello {
			t.Errorf("hello: %v", e)
			return
		}
		write := func(f mxwire.Frame) {
			if e := mxwire.WriteFrame(w, f); e != nil {
				t.Errorf("write: %v", e)
			}
		}
		ready, _ := mxwire.JSONFrame(mxwire.FrameReady, 0, 0, mxwire.Ready{Version: mxwire.V2Protocol, ReceiverID: "receiver", ConnectionID: "connection", SMTPHostname: "mx.example.com", MaxMessageBytes: 1 << 20})
		write(ready)
		w.(http.Flusher).Flush()
		f, e = mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameDomainAuth {
			t.Errorf("auth: %v", e)
			return
		}
		var auth mxwire.DomainAuth
		if mxwire.DecodeFrame(f, &auth) != nil {
			t.Error("decode auth")
		}
		channel := f.ChannelID
		challenge := mxwire.Challenge{Domain: auth.Domain, KeyID: auth.KeyID, ReceiverID: "receiver", ConnectionID: "connection", Nonce: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"}
		cf, _ := mxwire.JSONFrame(mxwire.FrameChallenge, 0, channel, challenge)
		write(cf)
		w.(http.Flusher).Flush()
		f, e = mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameChallengeResponse {
			t.Errorf("proof: %v", e)
			return
		}
		var proof mxwire.ChallengeResponse
		if mxwire.DecodeFrame(f, &proof) != nil || !mxwire.VerifyChallenge(pub, challenge, proof.Signature) {
			t.Error("challenge proof invalid")
		}
		af, _ := mxwire.JSONFrame(mxwire.FrameAuthResult, 0, channel, mxwire.AuthResult{Domain: auth.Domain, KeyID: auth.KeyID, Accepted: true, ExpiresAt: time.Now().Add(4 * time.Minute)})
		write(af)
		request, _ := mxwire.JSONFrame(mxwire.FrameResolve, 1, channel, mxwire.V2Resolve{Domain: auth.Domain, Recipient: "alice@example.com"})
		write(request)
		w.(http.Flusher).Flush()
		f, e = mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameResolveResult {
			t.Errorf("resolve result: %v", e)
			return
		}
		var resolved mxwire.ResolveResponse
		if mxwire.DecodeFrame(f, &resolved) != nil || len(resolved.Results) != 1 || !resolved.Results[0].Accept {
			t.Error("resolve failed")
		}
		meta := mxwire.IngestMetadata{Recipients: []string{"alice@example.com"}, ContentDigest: digest, Size: int64(len(payload))}
		start, _ := mxwire.JSONFrame(mxwire.FrameIngestStart, 1, 0, mxwire.V2IngestStart{Domains: []string{auth.Domain}, Metadata: meta})
		write(start)
		write(mxwire.ChunkFrame(1, 0, 0, payload))
		end, _ := mxwire.JSONFrame(mxwire.FrameIngestEnd, 1, 0, mxwire.V2IngestEnd{Size: int64(len(payload)), ContentDigest: digest})
		write(end)
		w.(http.Flusher).Flush()
		f, e = mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameIngestResult {
			t.Errorf("ingest result: %v", e)
			return
		}
		<-r.Context().Done()
	}))
	receiver.EnableHTTP2 = true
	receiver.StartTLS()
	defer receiver.Close()
	parsed, _ := url.Parse(receiver.URL)
	pool := x509.NewCertPool()
	pool.AddCert(receiver.Certificate())
	tlsConfig := &tls.Config{RootCAs: pool, ServerName: parsed.Hostname()}
	backend.setDomains(mxdial.Domain{Name: "example.com", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{receiver.URL}})
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := mxdial.New(backend, mxdial.Config{DataDir: dataDir, TLSConfig: tlsConfig, ReconcileInterval: 20 * time.Millisecond})
	go m.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		done := backend.ingests == 1 && backend.raw == string(payload)
		backend.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.resolves != 1 || backend.ingests != 1 || backend.raw != string(payload) {
		t.Fatalf("resolve=%d ingest=%d raw=%q", backend.resolves, backend.ingests, backend.raw)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		files, e := os.ReadDir(filepath.Join(dataDir, "messages", ".tmp"))
		if e == nil && len(files) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("staging file was not removed")
}
