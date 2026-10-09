package receiver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/tests/support/smoke"
)

// TestReadyAdvertisesSessionLimits proves the receiver publishes its domain,
// auth-inflight and renewal limits in Ready so a dialer can shard and pace
// itself.
func TestReadyAdvertisesSessionLimits(t *testing.T) {
	pub, _, err0 := ed25519.GenerateKey(rand.Reader)
	if err0 != nil {
		t.Fatal(err0)
	}
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{
		LookupTXT:               dns,
		MaxDomainsPerConnection: 7,
		RevalidateInterval:      90 * time.Second,
	})

	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+mxwire.SessionPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := hc.Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "gatehouse"})
	if err := mxwire.WriteFrame(pw, hello); err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake timeout")
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol = %s", resp.Proto)
	}
	f, err := mxwire.ReadFrame(resp.Body)
	if err != nil || f.Type != mxwire.FrameReady {
		t.Fatalf("ready: %v type=%d", err, f.Type)
	}
	var ready mxwire.Ready
	if err := mxwire.DecodeFrame(f, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.MaxDomains != 7 {
		t.Fatalf("MaxDomains = %d, want 7", ready.MaxDomains)
	}
	if ready.MaxAuthInflight <= 0 {
		t.Fatalf("MaxAuthInflight not advertised: %d", ready.MaxAuthInflight)
	}
	if ready.RevalidateSeconds != 90 {
		t.Fatalf("RevalidateSeconds = %d, want 90", ready.RevalidateSeconds)
	}
	cancel()
}

// TestIdleSessionSurvivesWriteDeadline exercises real HTTP/2 stream deadlines:
// a completed Ready write must not leave the ten-second deadline armed.
func TestIdleSessionSurvivesWriteDeadline(t *testing.T) {
	smoke.Require(t)
	_, srv, tlsConfig := newReceiverServer(t, receiver.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+mxwire.SessionPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := client.Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "gatehouse"})
	if err := mxwire.WriteFrame(pw, hello); err != nil {
		t.Fatal(err)
	}
	resp := <-done
	if resp == nil {
		t.Fatal("session request failed")
	}
	defer resp.Body.Close()
	if f, err := mxwire.ReadFrame(resp.Body); err != nil || f.Type != mxwire.FrameReady {
		t.Fatalf("Ready: %v type=%d", err, f.Type)
	}
	time.Sleep(11 * time.Second)
	ping, _ := mxwire.JSONFrame(mxwire.FramePing, 0, 0, nil)
	if err := mxwire.WriteFrame(pw, ping); err != nil {
		t.Fatalf("idle session closed before ping: %v", err)
	}
	if f, err := mxwire.ReadFrame(resp.Body); err != nil || f.Type != mxwire.FramePong {
		t.Fatalf("idle session must still answer: %v type=%d", err, f.Type)
	}
}

// renewalDialer is a minimal core that authenticates many domains and answers
// every challenge, deliberately slowly, so renewal pacing can be observed.
type renewalDialer struct {
	t        *testing.T
	pw       *io.PipeWriter
	priv     ed25519.PrivateKey
	proofLag time.Duration
	writeMu  sync.Mutex

	ready     chan struct{}
	readyOnce sync.Once
	inflight  atomic.Int64
	maxSeen   atomic.Int64
	renewals  atomic.Int64
	auths     atomic.Int64
	revoked   atomic.Int64
}

// write serializes frame writes: many proofs are answered concurrently, but the
// pipe is a single ordered byte stream.
func (d *renewalDialer) write(f mxwire.Frame) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return mxwire.WriteFrame(d.pw, f)
}

func newRenewalDialer(t *testing.T, base string, client *tls.Config, priv ed25519.PrivateKey, domains []string, proofLag time.Duration) *renewalDialer {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+mxwire.SessionPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := httpClient.Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "renewal"})
	if err := mxwire.WriteFrame(pw, hello); err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal dialer handshake timeout")
	}
	if resp == nil || resp.ProtoMajor != 2 {
		t.Fatalf("no HTTP/2 session: %#v", resp)
	}
	d := &renewalDialer{t: t, pw: pw, priv: priv, proofLag: proofLag, ready: make(chan struct{})}
	t.Cleanup(cancel)
	go d.run(resp, domains)
	return d
}

func (d *renewalDialer) run(resp *http.Response, domains []string) {
	if f, e := mxwire.ReadFrame(resp.Body); e != nil || f.Type != mxwire.FrameReady {
		d.t.Errorf("expected ready: %v %v", f.Type, e)
		return
	}
	// Authenticate every domain on its own channel.
	ch := uint64(1)
	for _, domain := range domains {
		auth, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, ch, mxwire.DomainAuth{Domain: domain, KeyID: "key1"})
		if err := d.write(auth); err != nil {
			return
		}
		ch++
	}
	expected := len(domains)
	for {
		f, e := mxwire.ReadFrame(resp.Body)
		if e != nil {
			return
		}
		switch f.Type {
		case mxwire.FrameChallenge:
			var c mxwire.Challenge
			if mxwire.DecodeFrame(f, &c) != nil {
				return
			}
			// A challenge is a renewal only once at least that many domains are
			// already bound; initial proofs are not paced against this counter.
			if d.auths.Load() >= int64(expected) {
				d.renewals.Add(1)
			}
			go func(f mxwire.Frame, c mxwire.Challenge) {
				cur := d.inflight.Add(1)
				for {
					old := d.maxSeen.Load()
					if cur <= old || d.maxSeen.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(d.proofLag)
				sig, se := mxwire.SignChallenge(d.priv, c)
				if se == nil {
					proof, _ := mxwire.JSONFrame(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
					_ = d.write(proof)
				}
				d.inflight.Add(-1)
			}(f, c)
		case mxwire.FrameAuthResult:
			var a mxwire.AuthResult
			if mxwire.DecodeFrame(f, &a) == nil && a.Accepted {
				if d.auths.Add(1) == int64(expected) {
					d.readyOnce.Do(func() { close(d.ready) })
				}
			}
		case mxwire.FrameDomainRevoked:
			d.revoked.Add(1)
		}
	}
}

// TestReceiverDrivenRenewalIsPaced proves that with many domains due at once the
// receiver issues renewal challenges in bounded batches instead of blasting
// past the source's auth capacity, and never revokes a binding merely because a
// renewal could not start immediately.
func TestReceiverDrivenRenewalIsPaced(t *testing.T) {
	const domains = 25
	const renewalsInFlight = 4
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{
		LookupTXT: dns,
		// Renew quickly, pace renewals to 4 at a time, and give the source
		// generous auth limits so pacing (not the per-IP window) is what is
		// being measured.
		RevalidateInterval:  100 * time.Millisecond,
		MaxRenewalsInFlight: renewalsInFlight,
		MaxAuthConcurrent:   64,
		AuthWindowMax:       100000,
	})
	names := make([]string, 0, domains)
	for i := 0; i < domains; i++ {
		names = append(names, fmt.Sprintf("d%03d.test", i))
	}
	d := newRenewalDialer(t, srv.URL, client, priv, names, 60*time.Millisecond)
	select {
	case <-d.ready:
	case <-time.After(10 * time.Second):
		t.Fatalf("domains did not authenticate: %d/%d", d.auths.Load(), domains)
	}
	// Let any initial-auth challenges drain, then measure renewal concurrency
	// only.
	time.Sleep(300 * time.Millisecond)
	d.maxSeen.Store(0)
	d.renewals.Store(0)
	// Watch several renewal rounds.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && d.renewals.Load() < int64(domains*2) {
		time.Sleep(20 * time.Millisecond)
	}
	if d.renewals.Load() == 0 {
		t.Fatal("no receiver-driven renewals were issued")
	}
	if got := d.maxSeen.Load(); got > renewalsInFlight {
		t.Fatalf("renewal pacing exceeded: %d concurrent renewals, cap %d", got, renewalsInFlight)
	}
	// No binding may be revoked while its grant is still valid: all domains must
	// remain available and the session alive.
	if got := d.auths.Load(); got < domains {
		t.Fatalf("only %d/%d domains authenticated", got, domains)
	}
	if got := d.revoked.Load(); got != 0 {
		t.Fatalf("renewal pacing revoked %d bindings", got)
	}
}

// TestRenewalSourceLimitDoesNotRevoke proves a renewal that cannot start because
// the source is at its temporary authentication capacity is deferred, not
// treated as a failed proof: no binding is revoked before its grant expires.
func TestRenewalSourceLimitDoesNotRevoke(t *testing.T) {
	const domains = 12
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{
		LookupTXT: dns,
		// One concurrent auth job per source forces renewals to contend and be
		// deferred, while the window is large enough that it is never the cause.
		RevalidateInterval:  50 * time.Millisecond,
		MaxAuthConcurrent:   1,
		AuthWindowMax:       100000,
		MaxRenewalsInFlight: 8,
	})
	names := make([]string, 0, domains)
	for i := 0; i < domains; i++ {
		names = append(names, fmt.Sprintf("d%03d.test", i))
	}
	d := newRenewalDialer(t, srv.URL, client, priv, names, 20*time.Millisecond)
	select {
	case <-d.ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("domains did not authenticate: %d/%d", d.auths.Load(), domains)
	}
	// Let many renewal rounds queue against the single auth slot.
	time.Sleep(1500 * time.Millisecond)
	if d.renewals.Load() == 0 {
		t.Fatal("no renewals were attempted")
	}
	if got := d.revoked.Load(); got != 0 {
		t.Fatalf("bindings revoked on temporary per-domain/source limit: %d", got)
	}
}
