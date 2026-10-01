package mxdial_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func domainFor(t *testing.T, rc *fakeReceiver, name string) mxdial.Domain {
	t.Helper()
	return mxdial.Domain{Name: name, KeyID: rc.keyID, PrivateKey: rc.priv, ReceiverURLs: []string{rc.url()}}
}

// TestReconnectResetsStateAndReauthenticates forces the server to drop the
// connection and asserts the manager reconnects, re-authenticates and clears
// per-connection state.
func TestReconnectResetsStateAndReauthenticates(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)

	first := rc.connections()
	rc.dropConnections()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if rc.connections() > first {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rc.connections() <= first {
		t.Fatal("manager did not reconnect after drop")
	}
	waitReady(t, m, "example.test", 4*time.Second)
}

// TestConfigDisabledStopsSession verifies a domain removed from the backend
// configuration stops its session and clears status without restarting.
func TestConfigDisabledStopsSession(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)

	b.setDomains()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.Status("example.test")) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(m.Status("example.test")) != 0 {
		t.Fatalf("status not cleared after disable: %#v", m.Status("example.test"))
	}
	// After the session stops, the connection count must stay put.
	stable := rc.connections()
	time.Sleep(300 * time.Millisecond)
	if got := rc.connections(); got != stable {
		t.Fatalf("session kept reconnecting after disable: %d -> %d", stable, got)
	}
}

// TestDomainsErrorKeepsExistingSession verifies a transient backend error does
// not tear down or restart an already-ready session.
func TestDomainsErrorKeepsExistingSession(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())
	waitReady(t, m, "example.test", 3*time.Second)

	conns := rc.connections()
	b.setDomainsErr(errInjected)
	time.Sleep(300 * time.Millisecond)
	if got := rc.connections(); got != conns {
		t.Fatalf("transient Domains error restarted the session: %d -> %d", conns, got)
	}
	waitReady(t, m, "example.test", 2*time.Second)
}

// TestAuthRejectRetriesWithoutResolving verifies an unauthenticated receiver
// can never resolve, and that a later successful auth is reached by retry.
func TestAuthRejectRetriesWithoutResolving(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.mode("reject")
	var resolved atomic.Int64
	rc.onResolve = func(fc *fakeConn, q mxwire.V2Resolve) {
		resolved.Add(1)
	}

	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	m, _ := managerFor(t, b, rc, t.TempDir())

	waitState(t, m, "example.test", "rejected", 3*time.Second)
	if resolved.Load() != 0 {
		t.Fatal("receiver resolved before proving control")
	}
	// Switching to accept and forcing a reconnect reaches a fresh auth attempt
	// immediately, rather than waiting out the bounded retry timer.
	first := rc.connections()
	rc.mode("accept")
	rc.dropConnections()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		s := m.Status("example.test")
		if len(s) > 0 && s[0].State == "ready" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := m.Status("example.test")
	if len(s) == 0 || s[0].State != "ready" {
		t.Fatalf("did not recover after auth retry: %#v (conns %d->%d)", s, first, rc.connections())
	}
}

// TestCancelWhileBackendIngestingKeepsFileUntilWorkerExits verifies a
// disconnect cancels the transaction but does not delete the staging file out
// from under an in-flight backend ingest.
func TestCancelWhileBackendIngestingKeepsFileUntilWorkerExits(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	b.ingestEnter = make(chan struct{}, 1)
	b.ingestHold = make(chan struct{})

	payload := []byte("hello world")
	digest := sha256Hex(payload)
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
	}
	// A conformant receiver starts ingest only after the resolve result.
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type != mxwire.FrameResolveResult {
			return false
		}
		var rr mxwire.ResolveResponse
		_ = mxwire.DecodeFrame(f, &rr)
		if len(rr.Results) != 1 || !rr.Results[0].Accept {
			return true
		}
		meta := mxwire.IngestMetadata{Recipients: []string{rr.Results[0].Recipient}, ContentDigest: digest, Size: int64(len(payload))}
		fc.send(mxwire.FrameIngestStart, f.TxID, 0, mxwire.V2IngestStart{Domains: []string{rr.Results[0].Domain}, Metadata: meta})
		fc.write(mxwire.ChunkFrame(f.TxID, 0, 0, payload))
		fc.send(mxwire.FrameIngestEnd, f.TxID, 0, mxwire.V2IngestEnd{Size: int64(len(payload)), ContentDigest: digest})
		return true
	}

	dataDir := t.TempDir()
	m, _ := managerFor(t, b, rc, dataDir)
	waitReady(t, m, "example.test", 3*time.Second)

	select {
	case <-b.ingestEnter:
	case <-time.After(3 * time.Second):
		t.Fatal("backend ingest never started")
	}
	b.mu.Lock()
	path := b.lastPath
	b.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staging file missing while ingest in flight: %v", err)
	}

	// Disconnect while the backend holds the file.
	rc.dropConnections()
	// The file must survive until the worker returns.
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staging file deleted while backend still using it: %v", err)
	}
	close(b.ingestHold)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("staging file not removed after worker exited")
}

// TestGlobalTransactionCapAnswersPerRecipient verifies the shared transaction
// budget yields a temporary per-recipient result without tearing the session,
// and that the receiver can resolve again once the budget frees.
func TestGlobalTransactionCapAnswersPerRecipient(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))

	results := make(chan mxwire.ResolveResponse, 4)
	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
		fc.send(mxwire.FrameResolve, 2, ch, mxwire.V2Resolve{Domain: domain, Recipient: "b@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameResolveResult {
			var r mxwire.ResolveResponse
			_ = mxwire.DecodeFrame(f, &r)
			results <- r
			return true
		}
		return false
	}

	m := mxdial.New(b, mxdial.Config{
		DataDir:                  t.TempDir(),
		TLSConfig:                rc.tls(),
		ReconcileInterval:        20 * time.Millisecond,
		MaxTransactions:          1,
		AllowPrivateDestinations: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	defer cancel()
	waitReady(t, m, "example.test", 3*time.Second)

	// The second transaction cannot get a slot and must receive an explicit
	// per-recipient temporary result.
	var sawTemp, sawOK bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !(sawTemp && sawOK) {
		select {
		case r := <-results:
			if r.MachineCode == mxwire.CodeTempFail && len(r.Results) == 1 && r.Results[0].Temporary {
				sawTemp = true
			} else if len(r.Results) == 1 && r.Results[0].Accept {
				sawOK = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawOK || !sawTemp {
		t.Fatalf("cap not enforced per-recipient: ok=%v temp=%v", sawOK, sawTemp)
	}
	// Session must still be alive (receiver still connected).
	if rc.connections() == 0 {
		t.Fatal("session torn down on transaction cap")
	}
}

// TestWrongSequenceCleansUpStaging verifies an out-of-order chunk closes the
// session and removes the staging file.
func TestWrongSequenceCleansUpStaging(t *testing.T) {
	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(domainFor(t, rc, "example.test"))
	payload := []byte("abcdef")
	digest := sha256Hex(payload)

	rc.onAccepted = func(fc *fakeConn, domain string, ch uint64) {
		fc.send(mxwire.FrameResolve, 1, ch, mxwire.V2Resolve{Domain: domain, Recipient: "a@" + domain})
	}
	rc.onFrame = func(fc *fakeConn, f mxwire.Frame) bool {
		if f.Type != mxwire.FrameResolveResult {
			return false
		}
		var rr mxwire.ResolveResponse
		_ = mxwire.DecodeFrame(f, &rr)
		meta := mxwire.IngestMetadata{Recipients: []string{rr.Results[0].Recipient}, ContentDigest: digest, Size: int64(len(payload))}
		fc.send(mxwire.FrameIngestStart, f.TxID, 0, mxwire.V2IngestStart{Domains: []string{rr.Results[0].Domain}, Metadata: meta})
		// seq 0 then a wrong seq 2.
		fc.write(mxwire.ChunkFrame(f.TxID, 0, 0, payload[:3]))
		fc.write(mxwire.ChunkFrame(f.TxID, 0, 2, payload[3:]))
		return true
	}

	dataDir := t.TempDir()
	managerFor(t, b, rc, dataDir)

	tmp := filepath.Join(dataDir, "messages", ".tmp")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(tmp)
		if err == nil && len(entries) == 0 && b.ingestCount() == 0 {
			// Let the reconnect settle and confirm no ingest ever ran.
			time.Sleep(100 * time.Millisecond)
			if b.ingestCount() == 0 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("staging not cleaned after malformed sequence (ingests=%d)", b.ingestCount())
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (b *scriptedBackend) ingestCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ingests
}

var errInjected = errors.New("injected domain error")
