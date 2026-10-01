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

func TestGroupedTwoDomainsAndCancelWhileBusy(t *testing.T) {
	_, keyA, _ := ed25519.GenerateKey(rand.Reader)
	_, keyB, _ := ed25519.GenerateKey(rand.Reader)
	dataDir := t.TempDir()
	b := &scriptedBackend{}
	b.setDomains(
		mxdial.Domain{Name: "a.test", KeyID: "ka", PrivateKey: keyA},
		mxdial.Domain{Name: "b.test", KeyID: "kb", PrivateKey: keyB},
	)
	b.ingestEnter = make(chan struct{}, 1)
	b.ingestHold = make(chan struct{})
	receiver := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, e := mxwire.ReadFrame(r.Body)
		if e != nil || f.Type != mxwire.FrameHello {
			return
		}
		write := func(fr mxwire.Frame) { _ = mxwire.WriteFrame(w, fr) }
		ready, _ := mxwire.JSONFrame(mxwire.FrameReady, 0, 0, mxwire.Ready{Version: mxwire.V2Protocol, ReceiverID: "r", ConnectionID: "c", SMTPHostname: "mx.test", MaxMessageBytes: 1 << 20})
		write(ready)
		w.(http.Flusher).Flush()
		channels := map[string]uint64{}
		authed := make(chan string, 2)
		resolved := 0
		ingestSent := false
		payload := []byte("grouped body")
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		for {
			fr, e := mxwire.ReadFrame(r.Body)
			if e != nil {
				return
			}
			switch fr.Type {
			case mxwire.FrameDomainAuth:
				var a mxwire.DomainAuth
				_ = mxwire.DecodeFrame(fr, &a)
				channels[a.Domain] = fr.ChannelID
				ch := mxwire.Challenge{Domain: a.Domain, KeyID: a.KeyID, ReceiverID: "r", ConnectionID: "c", Nonce: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY"}
				cf, _ := mxwire.JSONFrame(mxwire.FrameChallenge, 0, fr.ChannelID, ch)
				write(cf)
				w.(http.Flusher).Flush()
			case mxwire.FrameChallengeResponse:
				var p mxwire.ChallengeResponse
				_ = mxwire.DecodeFrame(fr, &p)
				ar, _ := mxwire.JSONFrame(mxwire.FrameAuthResult, 0, fr.ChannelID, mxwire.AuthResult{Domain: p.Domain, KeyID: p.KeyID, Accepted: true, ExpiresAt: time.Now().Add(4 * time.Minute)})
				write(ar)
				w.(http.Flusher).Flush()
				authed <- p.Domain
				// RCPTs within one transaction are resolved sequentially: send
				// the first resolve only once both domains are authenticated.
				if len(authed) >= 2 && resolved == 0 {
					rq, _ := mxwire.JSONFrame(mxwire.FrameResolve, 1, channels["a.test"], mxwire.V2Resolve{Domain: "a.test", Recipient: "user@a.test"})
					write(rq)
					w.(http.Flusher).Flush()
				}
			case mxwire.FrameResolveResult:
				resolved++
				// After the first result, issue the next recipient's resolve;
				// SMTP resolves recipients one at a time per transaction.
				if resolved == 1 {
					rq, _ := mxwire.JSONFrame(mxwire.FrameResolve, 1, channels["b.test"], mxwire.V2Resolve{Domain: "b.test", Recipient: "user@b.test"})
					write(rq)
					w.(http.Flusher).Flush()
					continue
				}
				// A conformant receiver waits for every resolve result before
				// starting the grouped ingest; model that here.
				if resolved >= 2 && !ingestSent {
					ingestSent = true
					meta := mxwire.IngestMetadata{Recipients: []string{"user@a.test", "user@b.test"}, ContentDigest: digest, Size: int64(len(payload))}
					st, _ := mxwire.JSONFrame(mxwire.FrameIngestStart, 1, 0, mxwire.V2IngestStart{Domains: []string{"a.test", "b.test"}, Metadata: meta})
					write(st)
					write(mxwire.ChunkFrame(1, 0, 0, payload))
					en, _ := mxwire.JSONFrame(mxwire.FrameIngestEnd, 1, 0, mxwire.V2IngestEnd{Size: int64(len(payload)), ContentDigest: digest})
					write(en)
					w.(http.Flusher).Flush()
				}
			}
		}
	}))
	receiver.EnableHTTP2 = true
	receiver.StartTLS()
	defer receiver.Close()
	parsed, _ := url.Parse(receiver.URL)
	pool := x509.NewCertPool()
	pool.AddCert(receiver.Certificate())
	b.setDomains(
		mxdial.Domain{Name: "a.test", KeyID: "ka", PrivateKey: keyA, ReceiverURLs: []string{receiver.URL}},
		mxdial.Domain{Name: "b.test", KeyID: "kb", PrivateKey: keyB, ReceiverURLs: []string{receiver.URL}},
	)
	m := mxdial.New(b, mxdial.Config{DataDir: dataDir, TLSConfig: &tls.Config{RootCAs: pool, ServerName: parsed.Hostname()}, ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sa, sb := m.Status("a.test"), m.Status("b.test")
		if len(sa) > 0 && sa[0].State == "ready" && len(sb) > 0 && sb[0].State == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.NewTimer(3 * time.Second)
	select {
	case <-b.ingestEnter:
	case <-started.C:
		t.Fatal("ingest never started")
	}
	started.Stop()
	close(b.ingestHold)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		files, e := os.ReadDir(filepath.Join(dataDir, "messages", ".tmp"))
		if e == nil && len(files) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("staging file retained")
}
