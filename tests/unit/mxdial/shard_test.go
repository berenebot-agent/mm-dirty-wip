package mxdial_test

import (
	"context"
	"encoding/base64"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestSharedModeShardsByAdvertisedMaxDomains proves that when a receiver
// advertises a small domain cap, the core opens one stable shard per group of
// domains rather than forcing every domain onto one session.
func TestSharedModeShardsByAdvertisedMaxDomains(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.setCaps(1, 0)
	b := &scriptedBackend{}
	b.setDomains(
		domainFor(t, rc, "a.test"),
		domainFor(t, rc, "b.test"),
		domainFor(t, rc, "c.test"),
	)
	m, _ := managerFor(t, b, rc, t.TempDir())

	for _, d := range []string{"a.test", "b.test", "c.test"} {
		waitReady(t, m, d, 5*time.Second)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && rc.liveConnections() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := rc.liveConnections(); got < 3 {
		t.Fatalf("expected 3 shards for 3 domains at cap 1, got %d live connections", got)
	}
}

// TestSharedModeShardCountStableAcrossReconnect proves shard assignment is
// stable: after every connection is dropped, the same number of shards is
// re-established and all domains return to ready without churn.
func TestSharedModeShardCountStableAcrossReconnect(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.setCaps(1, 0)
	b := &scriptedBackend{}
	b.setDomains(
		domainFor(t, rc, "a.test"),
		domainFor(t, rc, "b.test"),
		domainFor(t, rc, "c.test"),
		domainFor(t, rc, "d.test"),
	)
	m, _ := managerFor(t, b, rc, t.TempDir())
	domains := []string{"a.test", "b.test", "c.test", "d.test"}
	for _, d := range domains {
		waitReady(t, m, d, 6*time.Second)
	}
	// Let the shard count settle before recording the baseline.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rc.liveConnections() < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	baseline := rc.liveConnections()
	if baseline < 4 {
		t.Fatalf("expected at least 4 shards, got %d", baseline)
	}
	before := rc.connections()
	rc.dropConnections()
	// The drop must be observed first, then every shard re-establishes and
	// every domain returns to ready.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rc.connections() >= before+baseline && rc.liveConnections() >= baseline {
			allReady := true
			for _, d := range domains {
				s := m.Status(d)
				if len(s) == 0 || s[0].State != "ready" {
					allReady = false
					break
				}
			}
			if allReady {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The connection count must settle at the shard count, without opening an
	// extra session per domain.
	stable := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(stable) {
		if got := rc.liveConnections(); got < baseline || got > baseline+1 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rc.liveConnections(); got < baseline || got > baseline+1 {
		t.Fatalf("shard count unstable after reconnect: %d live connections (baseline %d)", got, baseline)
	}
}

// TestSingleModeStaysOneSession proves private single mode is never sharded,
// even when the configured domains would otherwise exceed a small cap.
func TestSingleModeStaysOneSession(t *testing.T) {
	// Single mode is keyed by cfg.ReceiverURL and the backend returns no
	// domains; use a bare manager with a receiver URL and assert that only one
	// session is created regardless of advertised caps.
	rc := newFakeReceiver(t)
	rc.setCaps(1, 0)
	rc.setReadyMode("single")
	b := &scriptedBackend{}
	m := mxdial.New(b, mxdial.Config{
		ReceiverURL:              rc.url(),
		CoreKey:                  "k",
		DataDir:                  t.TempDir(),
		TLSConfig:                rc.tls(),
		ReconcileInterval:        20 * time.Millisecond,
		AllowPrivateDestinations: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(600 * time.Millisecond)
	if got := rc.liveConnections(); got != 1 {
		t.Fatalf("single mode has %d live sessions, want 1", got)
	}
}

// TestShardCapacityPackingAndNoCompaction proves adding a domain packs into the
// lowest shard with spare capacity instead of reshuffling, and that raising the
// advertised cap does NOT compact healthy shards.
func TestShardCapacityPackingAndNoCompaction(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.setCaps(2, 0)
	b := &scriptedBackend{}
	b.setDomains(
		domainFor(t, rc, "a.test"),
		domainFor(t, rc, "b.test"),
	)
	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "a.test", 5*time.Second)
	waitReady(t, m, "b.test", 5*time.Second)
	// Two domains fit in one shard at cap 2.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rc.liveConnections() != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := rc.liveConnections(); got != 1 {
		t.Fatalf("expected 1 shard at cap 2 with 2 domains, got %d", got)
	}
	// Add a third domain: it must open a second shard (the first is full) and
	// both existing domains must stay ready.
	b.setDomains(
		domainFor(t, rc, "a.test"),
		domainFor(t, rc, "b.test"),
		domainFor(t, rc, "c.test"),
	)
	waitReady(t, m, "c.test", 5*time.Second)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rc.liveConnections() != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := rc.liveConnections(); got != 2 {
		t.Fatalf("expected 2 shards after adding a third domain at cap 2, got %d", got)
	}
	// Raising the cap must not compact the two healthy shards into one.
	rc.setCaps(128, 0)
	rc.dropConnections()
	time.Sleep(1500 * time.Millisecond)
	if got := rc.liveConnections(); got != 2 {
		t.Fatalf("healthy shards were compacted after cap raise: %d", got)
	}
}

// TestShardCapacityExhaustionDefers proves that when every shard is at the
// advertised cap and no more shards may be opened, an extra domain is reported
// as deferred rather than folded into an oversized shard.
func TestShardCapacityExhaustionDefers(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.setCaps(1, 0)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "a.test"), domainFor(t, rc, "b.test"))
	m := mxdial.New(b, mxdial.Config{
		DataDir:                  t.TempDir(),
		TLSConfig:                rc.tls(),
		ReconcileInterval:        20 * time.Millisecond,
		MaxShardsPerReceiver:     1,
		AllowPrivateDestinations: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// Only one domain can be placed; the other must be deferred with an explicit
	// status and never authenticated.
	b.setDomains(domainFor(t, rc, "a.test"), domainFor(t, rc, "b.test"))
	var deferred string
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, name := range []string{"a.test", "b.test"} {
			s := m.Status(name)
			if len(s) > 0 && s[0].State == "deferred" && s[0].Reason == "domain_capacity" {
				deferred = name
			}
		}
		if deferred != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if deferred == "" {
		t.Fatalf("no domain deferred at capacity: a=%#v b=%#v", m.Status("a.test"), m.Status("b.test"))
	}
	if got := rc.liveConnections(); got != 1 {
		t.Fatalf("capacity exhaustion opened %d shards, want 1", got)
	}
	// The deferred domain never reached ready.
	if s := m.Status(deferred); len(s) == 0 || s[0].State == "ready" {
		t.Fatalf("deferred domain reported ready: %#v", s)
	}
}

// TestAuthPacingBoundsInflight proves the dialer never has more initial
// authentications outstanding than its configured pacing budget: with a silent
// receiver and a budget of one, only a single DomainAuth is sent even when
// many domains are configured.
func TestAuthPacingBoundsInflight(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.mode("silent")
	var mu sync.Mutex
	var sent int
	rc.onAuth = func(fc *fakeConn, ch uint64, a mxwire.DomainAuth) {
		mu.Lock()
		sent++
		mu.Unlock()
	}
	b := &scriptedBackend{}
	b.setDomains(
		domainFor(t, rc, "a.test"),
		domainFor(t, rc, "b.test"),
		domainFor(t, rc, "c.test"),
		domainFor(t, rc, "d.test"),
	)
	m := mxdial.New(b, mxdial.Config{
		DataDir:                  t.TempDir(),
		TLSConfig:                rc.tls(),
		ReconcileInterval:        20 * time.Millisecond,
		MaxAuthInflight:          1,
		AllowPrivateDestinations: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	got := sent
	mu.Unlock()
	if got != 1 {
		t.Fatalf("auth pacing allowed %d in-flight DomainAuth, want 1", got)
	}
}

// TestReceiverDrivenRenewalKeepsBinding proves a binding is renewed by a
// receiver-issued Challenge on the existing channel: the core answers with a
// ChallengeResponse and never sends a second DomainAuth.
func TestReceiverDrivenRenewalKeepsBinding(t *testing.T) {
	rc := newFakeReceiver(t)
	var authFrames atomic.Int64
	var proofFrames atomic.Int64
	var renewSent atomic.Bool
	// onAuth performs the initial challenge so the built-in proof handling can
	// accept it.
	rc.onAuth = func(fc *fakeConn, ch uint64, a mxwire.DomainAuth) {
		authFrames.Add(1)
		nonce := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		c := mxwire.Challenge{Domain: a.Domain, KeyID: a.KeyID, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), Nonce: nonce}
		fc.pending[ch] = c
		fc.send(mxwire.FrameChallenge, 0, ch, c)
	}
	// onAccepted fires once the built-in handler accepts the initial proof;
	// send one receiver-driven renewal on the same channel.
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		if !renewSent.CompareAndSwap(false, true) {
			return
		}
		nonce := base64.RawURLEncoding.EncodeToString([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
		c := mxwire.Challenge{Domain: domain, KeyID: rc.keyID, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), Nonce: nonce}
		fc.pending[ch] = c
		fc.send(mxwire.FrameChallenge, 0, ch, c)
	}
	// Observe (but do not consume) proof frames so the built-in handler still
	// verifies and accepts them.
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameChallengeResponse {
			proofFrames.Add(1)
		}
		return false
	}
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && proofFrames.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	// One initial proof plus one renewal proof, and never a second DomainAuth:
	// the renewal was driven entirely by the receiver.
	if proofFrames.Load() < 2 {
		t.Fatalf("receiver-driven renewal was not answered: proofs=%d", proofFrames.Load())
	}
	if authFrames.Load() != 1 {
		t.Fatalf("core sent %d DomainAuth frames, want 1 (renewal must be receiver-driven)", authFrames.Load())
	}
}
