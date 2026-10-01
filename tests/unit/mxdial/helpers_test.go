package mxdial_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// fakeReceiver is a scriptable stand-in for the Dial MX receiver. It speaks the
// real wire protocol and lets tests drive protocol paths that are awkward to
// provoke with the full receiver implementation.
type fakeReceiver struct {
	mu sync.Mutex

	pub        ed25519.PublicKey
	priv       ed25519.PrivateKey
	keyID      string
	receiverID string
	hostname   string

	// authMode selects how a DomainAuth is answered:
	//   "accept"    - full challenge/proof exchange
	//   "reject"    - immediate AuthResult accepted=false (e.g. missing TXT)
	//   "silent"    - no answer at all
	authMode string

	// onAuth, when set, fully handles a DomainAuth (including the challenge and
	// later proof verification via fc.pending).
	onAuth func(fc *fakeConn, ch uint64, a mxwire.DomainAuth)

	// onAccepted runs once a domain's proof is verified and accepted.
	onAccepted func(fc *fakeConn, domain string, ch uint64)

	// onResolve answers a resolve request. When nil, accepts automatically.
	onResolve func(fc *fakeConn, q mxwire.V2Resolve)
	// onIngest is invoked once the full chunked body is received.
	onIngest func(fc *fakeConn, start mxwire.V2IngestStart, chunks [][]byte, end mxwire.V2IngestEnd)

	// onFrame may consume a frame before the built-in handling.
	onFrame func(fc *fakeConn, f mxwire.Frame) bool

	server *httptest.Server
	tlsCfg *tls.Config

	conns int
}

type fakeConn struct {
	rc      *fakeReceiver
	w       http.ResponseWriter
	flusher http.Flusher
	chans   map[string]uint64
	pending map[uint64]mxwire.Challenge
}

func (fc *fakeConn) write(f mxwire.Frame) {
	if e := mxwire.WriteFrame(fc.w, f); e != nil {
		return
	}
	fc.flusher.Flush()
}

func (fc *fakeConn) send(t mxwire.FrameType, tx, ch uint64, v any) {
	f, _ := mxwire.JSONFrame(t, tx, ch, v)
	fc.write(f)
}

func newFakeReceiver(t *testing.T) *fakeReceiver {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rc := &fakeReceiver{pub: pub, priv: priv, keyID: "key1", receiverID: "receiver-1", hostname: "mx.test", authMode: "accept"}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(rc.serve))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	rc.server = srv

	parsed, _ := url.Parse(srv.URL)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	rc.tlsCfg = &tls.Config{RootCAs: pool, ServerName: parsed.Hostname()}
	return rc
}

func (rc *fakeReceiver) url() string { return rc.server.URL }

func (rc *fakeReceiver) connections() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.conns
}

func (rc *fakeReceiver) mode(m string) {
	rc.mu.Lock()
	rc.authMode = m
	rc.mu.Unlock()
}

// dropConnections closes the server side of every live session, forcing the
// client to reconnect.
func (rc *fakeReceiver) dropConnections() {
	rc.server.CloseClientConnections()
}

func (rc *fakeReceiver) serve(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 2 {
		http.Error(w, "http2 required", 426)
		return
	}
	rc.mu.Lock()
	rc.conns++
	mode := rc.authMode
	rc.mu.Unlock()

	f, err := mxwire.ReadFrame(r.Body)
	if err != nil || f.Type != mxwire.FrameHello {
		return
	}
	fc := &fakeConn{rc: rc, w: w, flusher: w.(http.Flusher), chans: map[string]uint64{}, pending: map[uint64]mxwire.Challenge{}}
	fc.send(mxwire.FrameReady, 0, 0, mxwire.Ready{Version: mxwire.V2Protocol, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), SMTPHostname: rc.hostname, MaxMessageBytes: 1 << 20})

	for {
		f, err := mxwire.ReadFrame(r.Body)
		if err != nil {
			return
		}
		if rc.onFrame != nil && rc.onFrame(fc, f) {
			continue
		}
		switch f.Type {
		case mxwire.FrameDomainAuth:
			var a mxwire.DomainAuth
			if mxwire.DecodeFrame(f, &a) != nil {
				return
			}
			fc.chans[a.Domain] = f.ChannelID
			if rc.onAuth != nil {
				rc.onAuth(fc, f.ChannelID, a)
				continue
			}
			switch mode {
			case "silent":
			case "reject":
				fc.send(mxwire.FrameAuthResult, 0, f.ChannelID, mxwire.AuthResult{Domain: a.Domain, KeyID: a.KeyID, Accepted: false, Reason: "dns_unavailable"})
			default:
				nonce := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
				c := mxwire.Challenge{Domain: a.Domain, KeyID: a.KeyID, ReceiverID: rc.receiverID, ConnectionID: rc.connID(), Nonce: nonce}
				fc.pending[f.ChannelID] = c
				fc.send(mxwire.FrameChallenge, 0, f.ChannelID, c)
			}
		case mxwire.FrameChallengeResponse:
			var proof mxwire.ChallengeResponse
			if mxwire.DecodeFrame(f, &proof) != nil {
				return
			}
			c, ok := fc.pending[f.ChannelID]
			if !ok {
				fc.send(mxwire.FrameAuthResult, 0, f.ChannelID, mxwire.AuthResult{Domain: proof.Domain, KeyID: proof.KeyID, Accepted: false, Reason: "challenge_expired"})
				continue
			}
			delete(fc.pending, f.ChannelID)
			if c.Domain != proof.Domain || c.KeyID != proof.KeyID || c.Nonce != proof.Nonce || !mxwire.VerifyChallenge(rc.pub, c, proof.Signature) {
				fc.send(mxwire.FrameAuthResult, 0, f.ChannelID, mxwire.AuthResult{Domain: proof.Domain, KeyID: proof.KeyID, Accepted: false, Reason: "proof_invalid"})
				continue
			}
			fc.send(mxwire.FrameAuthResult, 0, f.ChannelID, mxwire.AuthResult{Domain: proof.Domain, KeyID: proof.KeyID, Accepted: true, ExpiresAt: time.Now().Add(4 * time.Minute)})
			if rc.onAccepted != nil {
				rc.onAccepted(fc, proof.Domain, f.ChannelID)
			}
		case mxwire.FrameResolve:
			var q mxwire.V2Resolve
			if mxwire.DecodeFrame(f, &q) != nil {
				return
			}
			if rc.onResolve != nil {
				rc.onResolve(fc, q)
			} else {
				fc.send(mxwire.FrameResolveResult, f.TxID, f.ChannelID, mxwire.ResolveResponse{
					MachineCode: mxwire.CodeOK,
					Results:     []mxwire.ResolveRecipient{{Recipient: q.Recipient, Domain: q.Domain, Accept: true}},
				})
			}
		case mxwire.FrameIngestStart:
			rc.readIngest(r, fc, f)
		case mxwire.FramePing:
			fc.send(mxwire.FramePong, 0, 0, struct{}{})
		}
	}
}

func (rc *fakeReceiver) connID() string { return "conn" }

func (rc *fakeReceiver) readIngest(r *http.Request, fc *fakeConn, f mxwire.Frame) {
	var start mxwire.V2IngestStart
	if mxwire.DecodeFrame(f, &start) != nil {
		return
	}
	var chunks [][]byte
	var end mxwire.V2IngestEnd
	for {
		nf, e := mxwire.ReadFrame(r.Body)
		if e != nil {
			return
		}
		switch nf.Type {
		case mxwire.FrameIngestChunk:
			_, data, e := mxwire.DecodeChunk(nf)
			if e != nil {
				return
			}
			chunks = append(chunks, data)
		case mxwire.FrameIngestEnd:
			_ = mxwire.DecodeFrame(nf, &end)
			if rc.onIngest != nil {
				rc.onIngest(fc, start, chunks, end)
			}
			return
		case mxwire.FrameCancel:
			return
		default:
			return
		}
	}
}

func (rc *fakeReceiver) tls() *tls.Config { return rc.tlsCfg }

// managerFor builds a manager wired to rc with a fast reconcile interval and
// stops it when the test ends.
func managerFor(t *testing.T, backend mxdial.Backend, rc *fakeReceiver, dataDir string) (*mxdial.Manager, context.CancelFunc) {
	t.Helper()
	m := mxdial.New(backend, mxdial.Config{
		DataDir:           dataDir,
		TLSConfig:         rc.tls(),
		ReconcileInterval: 20 * time.Millisecond,
		// Deterministic, fast rejection cooldown so same-connection retry tests
		// need not wait the production 5s window.
		AuthRetryInterval: 200 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})
	return m, cancel
}

func waitReady(t *testing.T, m *mxdial.Manager, domain string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s := m.Status(domain)
		if len(s) > 0 && s[0].State == "ready" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("domain %s not ready: %#v", domain, m.Status(domain))
}

func waitState(t *testing.T, m *mxdial.Manager, domain, state string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s := m.Status(domain)
		if len(s) > 0 && s[0].State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("domain %s not %s: %#v", domain, state, m.Status(domain))
}

// scriptedBackend is a controllable Backend implementation for the dialer.
type scriptedBackend struct {
	mu         sync.Mutex
	domains    []mxdial.Domain
	domainsErr error

	resolves int
	ingests  int
	raw      string
	lastMeta mxwire.IngestMetadata
	lastPath string

	resolveDelay time.Duration
	ingestDelay  time.Duration
	ingestEnter  chan struct{}
	ingestHold   chan struct{}
	ingestErr    error
	resolveFn    func(domain string, recipients []string) mxwire.ResolveResponse
	ingestFn     func(domains []string, meta mxwire.IngestMetadata, path string) (mxwire.IngestResponse, error)
}

func (b *scriptedBackend) setDomains(ds ...mxdial.Domain) {
	b.mu.Lock()
	b.domains = ds
	b.mu.Unlock()
}

func (b *scriptedBackend) Domains(_ context.Context) ([]mxdial.Domain, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.domainsErr != nil {
		return nil, b.domainsErr
	}
	return append([]mxdial.Domain(nil), b.domains...), nil
}

func (b *scriptedBackend) setDomainsErr(err error) {
	b.mu.Lock()
	b.domainsErr = err
	b.mu.Unlock()
}

func (b *scriptedBackend) Resolve(_ context.Context, d string, rs []string) (mxwire.ResolveResponse, error) {
	b.mu.Lock()
	b.resolves++
	delay := b.resolveDelay
	fn := b.resolveFn
	b.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if fn != nil {
		return fn(d, rs), nil
	}
	out := mxwire.ResolveResponse{MachineCode: mxwire.CodeOK}
	for _, r := range rs {
		out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: r, Domain: d, Accept: true})
	}
	return out, nil
}

func (b *scriptedBackend) Ingest(_ context.Context, domains []string, meta mxwire.IngestMetadata, path string) (mxwire.IngestResponse, error) {
	b.mu.Lock()
	b.ingests++
	b.lastMeta = meta
	b.lastPath = path
	enter := b.ingestEnter
	hold := b.ingestHold
	delay := b.ingestDelay
	fn := b.ingestFn
	b.mu.Unlock()

	if enter != nil {
		select {
		case enter <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		<-hold
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if fn != nil {
		return fn(domains, meta, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return mxwire.IngestResponse{MachineCode: mxwire.CodeTempFail}, err
	}
	b.mu.Lock()
	b.raw = string(data)
	b.mu.Unlock()
	out := mxwire.IngestResponse{MachineCode: mxwire.CodeOK}
	for _, r := range meta.Recipients {
		out.PerRecipient = append(out.PerRecipient, mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeOK, Disposition: mxwire.DispositionStored})
	}
	return out, nil
}
