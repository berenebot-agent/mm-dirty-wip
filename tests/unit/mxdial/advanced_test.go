package mxdial_test

import (
	"encoding/base64"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestReplacedPinnedTransactionStillIngests verifies the "last valid rule":
// after a receiver grants a domain to another connection, a transaction that
// was already pinned to the old binding may still deliver its DATA durably
// until the old authorization expires.
func TestReplacedPinnedTransactionStillIngests(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))

	payload := []byte("pinned body")
	digest := sha256Hex(payload)
	ingested := make(chan string, 1)

	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 7, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		switch f.Type {
		case mxwire.FrameResolveResult:
			var rr mxwire.ResolveResponse
			_ = mxwire.DecodeFrame(f, &rr)
			if len(rr.Results) != 1 || !rr.Results[0].Accept {
				return true
			}
			// The receiver has granted the domain elsewhere but still expects
			// the already-pinned transaction to finish.
			fc.send(mxwire.FrameDomainRevoked, 0, f.ChannelID, mxwire.DomainNotice{Domain: "example.test", Reason: "replaced"})
			meta := mxwire.IngestMetadata{Recipients: []string{rr.Results[0].Recipient}, ContentDigest: digest, Size: int64(len(payload))}
			fc.send(mxwire.FrameIngestStart, f.TxID, 0, mxwire.V2IngestStart{Domains: []string{"example.test"}, Metadata: meta})
			fc.write(mxwire.ChunkFrame(f.TxID, 0, 0, payload))
			fc.send(mxwire.FrameIngestEnd, f.TxID, 0, mxwire.V2IngestEnd{Size: int64(len(payload)), ContentDigest: digest})
			return true
		case mxwire.FrameIngestResult:
			ingested <- ""
			return true
		}
		return false
	}

	managerFor(t, b, rc, t.TempDir())
	select {
	case <-ingested:
	case <-time.After(4 * time.Second):
		t.Fatal("pinned transaction did not ingest after replacement")
	}
	b.mu.Lock()
	raw := b.raw
	b.mu.Unlock()
	if raw != string(payload) {
		t.Fatalf("durable ingest missing payload: %q", raw)
	}
}

// TestUnknownRecipientStaysPermanent verifies a core decision of unknown
// recipient (accept=false) is returned as permanent and is never upgraded to a
// temporary failure by normalization.
func TestUnknownRecipientStaysPermanent(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	b.resolveFn = func(domain string, recipients []string) mxwire.ResolveResponse {
		return mxwire.ResolveResponse{MachineCode: mxwire.CodeUnknownRecipient, Results: []mxwire.ResolveRecipient{{
			Recipient: recipients[0], Domain: domain, Accept: false, Code: string(mxwire.CodeUnknownRecipient),
		}}}
	}

	result := make(chan mxwire.ResolveResponse, 1)
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "ghost@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameResolveResult {
			var r mxwire.ResolveResponse
			_ = mxwire.DecodeFrame(f, &r)
			result <- r
			return true
		}
		return false
	}

	managerFor(t, b, rc, t.TempDir())
	select {
	case r := <-result:
		if len(r.Results) != 1 || r.Results[0].Accept || r.Results[0].Temporary {
			t.Fatalf("unknown recipient was not permanent: %#v", r)
		}
		if r.Results[0].Code != string(mxwire.CodeUnknownRecipient) {
			t.Fatalf("unknown recipient code lost: %#v", r.Results[0])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no resolve result")
	}
}

// TestBackendPanicIsContained verifies a panic in the resolve backend does not
// crash the core: the client receives a temporary failure and the manager keeps
// running.
func TestBackendPanicIsContained(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	b.resolveFn = func(domain string, recipients []string) mxwire.ResolveResponse {
		panic("boom")
	}

	result := make(chan mxwire.ResolveResponse, 1)
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "x@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameResolveResult {
			var r mxwire.ResolveResponse
			_ = mxwire.DecodeFrame(f, &r)
			result <- r
			return true
		}
		return false
	}

	managerFor(t, b, rc, t.TempDir())
	select {
	case r := <-result:
		if len(r.Results) != 1 || !r.Results[0].Temporary {
			t.Fatalf("panic was not reported as temporary: %#v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no resolve result after backend panic")
	}
}

// TestBackoffNotBypassedByReconcile verifies a stable configuration does not
// wake the session every reconcile tick, so reconnect backoff is honoured.
func TestBackoffNotBypassedByReconcile(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.mode("silent") // never answers, so the handshake eventually times out
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())
	_ = m

	// Reconcile runs every 20ms. Without change-detection the session would
	// reconnect ~50 times/second; with backoff it must be far fewer.
	time.Sleep(1 * time.Second)
	conns := rc.connections()
	if conns > 6 {
		t.Fatalf("reconcile bypassed backoff: %d connections in ~1s", conns)
	}
}

// TestReplacedNoticeParksChannel verifies a "replaced" notice parks the local
// pin (no re-auth takeover loop) for an unchanged configuration.
func TestReplacedNoticeParksChannel(t *testing.T) {
	rc := newFakeReceiver(t)
	var revokeOnce atomic.Bool
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		if revokeOnce.CompareAndSwap(false, true) {
			fc.send(mxwire.FrameDomainRevoked, 0, ch, mxwire.DomainNotice{Domain: domain, Reason: "replaced"})
		}
	}
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())

	waitState(t, m, "example.test", "unavailable", 3*time.Second)
	s := m.Status("example.test")
	if len(s) == 0 || s[0].Reason != "replaced" {
		t.Fatalf("unexpected replaced status: %#v", s)
	}
	// The parked pin must not trigger an immediate re-auth.
	stable := rc.connections()
	time.Sleep(500 * time.Millisecond)
	if got := rc.connections(); got != stable {
		t.Fatalf("replaced pin re-authenticated: %d -> %d", stable, got)
	}
}

// TestMissingBackendResultGetsScopedTemporaryResult verifies the client
// synthesizes an explicit temporary result when the backend omits one.
func TestMissingBackendResultGetsScopedTemporaryResult(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	b.ingestFn = func(domains []string, meta mxwire.IngestMetadata, path string) (mxwire.IngestResponse, error) {
		// Deliberately return no per-recipient results.
		return mxwire.IngestResponse{MachineCode: mxwire.CodeOK}, nil
	}

	payload := []byte("body")
	digest := sha256Hex(payload)
	result := make(chan mxwire.IngestResponse, 1)

	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		switch f.Type {
		case mxwire.FrameResolveResult:
			meta := mxwire.IngestMetadata{Recipients: []string{"a@example.test"}, ContentDigest: digest, Size: int64(len(payload))}
			fc.send(mxwire.FrameIngestStart, f.TxID, 0, mxwire.V2IngestStart{Domains: []string{"example.test"}, Metadata: meta})
			fc.write(mxwire.ChunkFrame(f.TxID, 0, 0, payload))
			fc.send(mxwire.FrameIngestEnd, f.TxID, 0, mxwire.V2IngestEnd{Size: int64(len(payload)), ContentDigest: digest})
			return true
		case mxwire.FrameIngestResult:
			var r mxwire.IngestResponse
			_ = mxwire.DecodeFrame(f, &r)
			select {
			case result <- r:
			default:
			}
			return true
		}
		return false
	}

	managerFor(t, b, rc, t.TempDir())
	select {
	case r := <-result:
		if len(r.PerRecipient) != 1 || r.PerRecipient[0].Recipient != "a@example.test" || r.PerRecipient[0].MachineCode != mxwire.CodeTempFail {
			t.Fatalf("unexpected normalized result: %#v", r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no ingest result")
	}
}

// TestAuthRejectRetriesSameConnection verifies a rejected authentication is
// retried on the same connection after the bounded retry window, without
// tearing the connection down.
func TestAuthRejectRetriesSameConnection(t *testing.T) {
	rc := newFakeReceiver(t)
	var attempts atomic.Int64
	rc.onAuth = func(fc *fakeConn, ch uint64, a mxwire.DomainAuth) {
		n := attempts.Add(1)
		if n == 1 {
			// Simulate a key/proof rejection: the retry cooldown is bounded
			// (tests wire a short AuthRetryInterval).
			fc.send(mxwire.FrameAuthResult, 0, ch, mxwire.AuthResult{Domain: a.Domain, KeyID: a.KeyID, Accepted: false, Reason: "proof_invalid"})
			return
		}
		nonce := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		c := mxwire.Challenge{Domain: a.Domain, KeyID: a.KeyID, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), Nonce: nonce}
		fc.pending[ch] = c
		fc.send(mxwire.FrameChallenge, 0, ch, c)
	}
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())

	waitState(t, m, "example.test", "rejected", 3*time.Second)
	conns := rc.connections()
	waitReady(t, m, "example.test", 9*time.Second)
	if rc.connections() != conns {
		t.Fatalf("retry opened a new connection: %d -> %d", conns, rc.connections())
	}
	if attempts.Load() < 2 {
		t.Fatalf("did not retry authentication: %d", attempts.Load())
	}
}

// TestConfigKeyChangeReauths verifies a changed domain key re-authenticates
// and unregisters the replaced channel rather than reusing stale authority.
func TestConfigKeyChangeReauths(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))

	rc.mu.Lock()
	rc.keyID = "key1"
	rc.mu.Unlock()
	var keyIDs []string
	var keyMu sync.Mutex
	rc.onAuth = func(fc *fakeConn, ch uint64, a mxwire.DomainAuth) {
		keyMu.Lock()
		keyIDs = append(keyIDs, a.KeyID)
		keyMu.Unlock()
		// Standard handshake regardless of key id.
		nonce := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		c := mxwire.Challenge{Domain: a.Domain, KeyID: a.KeyID, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), Nonce: nonce}
		fc.pending[ch] = c
		fc.send(mxwire.FrameChallenge, 0, ch, c)
	}

	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)

	// Rotate the key id for the same receiver URL.
	b.setDomains(mxdial.Domain{Name: "example.test", KeyID: "key2", PrivateKey: rc.priv, ReceiverURLs: []string{rc.url()}})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		keyMu.Lock()
		seen := false
		for _, k := range keyIDs {
			if k == "key2" {
				seen = true
			}
		}
		keyMu.Unlock()
		if seen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	keyMu.Lock()
	defer keyMu.Unlock()
	found := false
	for _, k := range keyIDs {
		if k == "key2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("did not re-auth with rotated key: %v", keyIDs)
	}
}

// TestDistinctTransactionsRunInParallel verifies two different transaction ids
// on the same connection can be in flight together.
func TestDistinctTransactionsRunInParallel(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))

	var inFlight, maxInFlight atomic.Int64
	b.resolveFn = func(d string, rs []string) mxwire.ResolveResponse {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
		inFlight.Add(-1)
		out := mxwire.ResolveResponse{MachineCode: mxwire.CodeOK}
		for _, r := range rs {
			out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: r, Domain: d, Accept: true})
		}
		return out
	}

	var started atomic.Int64
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		// The client answers each resolve with a ResolveResult on the request
		// stream; count those.
		if f.Type == mxwire.FrameResolveResult {
			started.Add(1)
		}
		return false
	}
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
		fc.send(mxwire.FrameResolve, 2, ch, mxwire.V2Resolve{Domain: domain, Recipient: "b@" + domain})
	}

	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if started.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() < 2 {
		t.Fatalf("distinct transactions did not both start: %d", started.Load())
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("resolves did not overlap: max in flight = %d", maxInFlight.Load())
	}
}
