package receiver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
	"github.com/emersion/go-smtp"
)

// backend is a fake core: it implements the dialer's Backend surface so tests
// can drive real mxdial Managers against a real receiver without a database.
type backend struct {
	mu         sync.Mutex
	domains    []mxdial.Domain
	stored     []string
	raw        string
	reject     map[string]bool
	resolveErr error
}

func (b *backend) Domains(context.Context) ([]mxdial.Domain, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]mxdial.Domain(nil), b.domains...), nil
}

func (b *backend) Resolve(_ context.Context, d string, rs []string) (mxwire.ResolveResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.resolveErr != nil {
		return mxwire.ResolveResponse{}, b.resolveErr
	}
	x := mxwire.ResolveResponse{MachineCode: mxwire.CodeOK}
	for _, a := range rs {
		if b.reject[strings.ToLower(a)] {
			x.Results = append(x.Results, mxwire.ResolveRecipient{Recipient: a, Domain: d})
			continue
		}
		x.Results = append(x.Results, mxwire.ResolveRecipient{Recipient: a, Domain: d, Accept: true})
	}
	return x, nil
}

func (b *backend) Ingest(_ context.Context, _ []string, m mxwire.IngestMetadata, path string) (mxwire.IngestResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, e := os.ReadFile(path)
	if e != nil {
		return mxwire.IngestResponse{}, e
	}
	b.raw = string(raw)
	x := mxwire.IngestResponse{MachineCode: mxwire.CodeOK, MessageID: "msg-" + m.ContentDigest}
	for _, a := range m.Recipients {
		b.stored = append(b.stored, a)
		x.PerRecipient = append(x.PerRecipient, mxwire.RecipientIngestResult{Recipient: a, MachineCode: mxwire.CodeOK, Disposition: mxwire.DispositionStored, MessageID: "msg-" + m.ContentDigest})
	}
	return x, nil
}

// rootTLS builds a client TLS config that trusts exactly the httptest server's
// certificate, with no insecure skip.
func rootTLS(t *testing.T, srv *httptest.Server) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// newReceiver builds a receiver with the test SMTP edge bounds.
func newReceiver(t *testing.T, cfg receiver.Config) *receiver.Receiver {
	t.Helper()
	if cfg.Mode == "" {
		cfg.Mode = "shared"
	}
	if cfg.SMTP.Hostname == "" {
		cfg.SMTP.Hostname = "mx.test"
	}
	if cfg.SMTP.MaxMessageBytes == 0 {
		cfg.SMTP.MaxMessageBytes = 1 << 20
	}
	if cfg.SMTP.MaxStagingBytes == 0 {
		cfg.SMTP.MaxStagingBytes = 2 << 20
	}
	if cfg.SMTP.MaxRecipients == 0 {
		cfg.SMTP.MaxRecipients = 10
	}
	if cfg.SMTP.MaxConnections == 0 {
		cfg.SMTP.MaxConnections = 16
	}
	if cfg.SMTP.DataTimeout == 0 {
		cfg.SMTP.DataTimeout = 5 * time.Second
	}
	if cfg.SMTP.DNSTimeout == 0 {
		cfg.SMTP.DNSTimeout = 2 * time.Second
	}
	return receiver.New(cfg, nil)
}

// newReceiverServer starts the receiver's HTTP/2 session handler under httptest
// and returns the receiver plus the client TLS config that trusts it.
func newReceiverServer(t *testing.T, cfg receiver.Config) (*receiver.Receiver, *httptest.Server, *tls.Config) {
	t.Helper()
	r := newReceiver(t, cfg)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	registerReceiver(srv.URL, r)
	return r, srv, rootTLS(t, srv)
}

func ready(t *testing.T, m *mxdial.Manager, d string) {
	t.Helper()
	limit := time.Now().Add(4 * time.Second)
	for time.Now().Before(limit) {
		s := m.Status(d)
		if len(s) > 0 && s[0].State == "ready" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s not ready: %#v", d, m.Status(d))
}

func send(t *testing.T, addr string, recipients []string) {
	t.Helper()
	c, e := smtp.Dial(addr)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Hello("sender.test"); e != nil {
		t.Fatal(e)
	}
	if e = c.Mail("sender@outside.test", nil); e != nil {
		t.Fatal(e)
	}
	for _, r := range recipients {
		if e = c.Rcpt(r, nil); e != nil {
			t.Fatal(e)
		}
	}
	w, e := c.Data()
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.WriteString(w, "From: sender@outside.test\r\nSubject: test\r\n\r\nbody")
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
}

// TestTLSHTTP2SMTPDelivery is the end-to-end smoke test: one manager, one
// domain, real SMTP edge, real HTTP/2 session, faked core persistence.
func TestTLSHTTP2SMTPDelivery(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})
	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	manager := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	go manager.Run(ctx)
	defer cancel()
	ready(t, manager, "example.test")
	edge := startEdge(t, 2, srv.URL, nil)
	send(t, edge, []string{"alice@example.test"})
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.stored) != 1 || be.raw == "" {
		t.Fatalf("no persisted delivery: %#v", be.stored)
	}
}

// TestTwoManagersOneReceiverFansOut proves genuine fan-out: two mxdial managers
// (two domain authorities) both authenticate on the same receiver object, and a
// single SMTP transaction fans out across both physical connections.
func TestTwoManagersOneReceiverFansOut(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	r, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})

	mk := func(domain string) (*backend, *mxdial.Manager) {
		be := &backend{domains: []mxdial.Domain{{Name: domain, KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
		m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
		return be, m
	}
	beA, mA := mk("example.test")
	beB, mB := mk("other.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mA.Run(ctx)
	go mB.Run(ctx)
	ready(t, mA, "example.test")
	ready(t, mB, "other.test")

	edge := startEdge(t, 3, srv.URL, nil)
	send(t, edge, []string{"alice@example.test", "bob@other.test"})
	beA.mu.Lock()
	defer beA.mu.Unlock()
	beB.mu.Lock()
	defer beB.mu.Unlock()
	if len(beA.stored) != 1 || len(beB.stored) != 1 {
		t.Fatalf("expected one delivery per connection: A=%#v B=%#v", beA.stored, beB.stored)
	}
	if got := r.Stats()["active_connections"]; got < 2 {
		t.Fatalf("expected two physical receiver sessions, got %d", got)
	}
}

// TestOneCoreTwoDomainsOnePhysicalConnection covers a single mxdial session
// authenticating two domains for the same receiver and delivering both in one
// SMTP transaction -- on one physical HTTP/2 connection.
func TestOneCoreTwoDomainsOnePhysicalConnection(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	r, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})
	be := &backend{domains: []mxdial.Domain{
		{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}},
		{Name: "other.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}},
	}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")
	ready(t, m, "other.test")
	edge := startEdge(t, 2, srv.URL, nil)
	send(t, edge, []string{"alice@example.test", "bob@other.test"})
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.stored) != 2 {
		t.Fatalf("expected both domains delivered by one core, got %#v", be.stored)
	}
	if got := r.Stats()["active_connections"]; got != 1 {
		t.Fatalf("expected one physical receiver session, got %d", got)
	}
}

// TestUnknownRecipientBackendReplyPreserved proves the receiver preserves a
// dialer's per-recipient "not accepted" resolve decision rather than guessing a
// temporary failure: the SMTP edge surfaces it as a permanent 5.1.1 unknown
// recipient. It drives the receiver with a fake in-process dialer over the real
// wire protocol, so it does not depend on the mxdial runtime.
func TestUnknownRecipientBackendReplyPreserved(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})
	d := newFakeDialer(t, srv.URL, client, priv, "example.test")
	defer d.close()
	d.waitAuth(t)

	edge := startEdge(t, 2, srv.URL, nil)
	c, e := smtp.Dial(edge)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Hello("sender.test"); e != nil {
		t.Fatal(e)
	}
	if e = c.Mail("sender@outside.test", nil); e != nil {
		t.Fatal(e)
	}
	e = c.Rcpt("ghost@example.test", nil)
	var se *smtp.SMTPError
	if !errors.As(e, &se) || se.Code != 550 || se.EnhancedCode != (smtp.EnhancedCode{5, 1, 1}) {
		t.Fatalf("expected 550 5.1.1 for unknown recipient, got %v", e)
	}
}

// TestFreshReauthThenTXTRemovalRenewsAndRevokes proves maintenance re-auth
// succeeds against the live key, then that removing the TXT record causes the
// binding to be revoked promptly under a fast tick.
func TestFreshReauthThenTXTRemovalRenewsAndRevokes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var mu sync.Mutex
	records := []string{mxwire.DomainTXT("key1", pub)}
	dns := func(context.Context, string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), records...), nil
	}
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns, RevalidateInterval: 50 * time.Millisecond})
	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")

	time.Sleep(300 * time.Millisecond)
	if s := m.Status("example.test"); len(s) == 0 || s[0].State != "ready" {
		t.Fatalf("renewal lost authority: %#v", s)
	}

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	mu.Lock()
	records = []string{mxwire.DomainTXT("key1", other)}
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := m.Status("example.test")
		unavailable := len(status) == 0 || status[0].State != "ready"
		if unavailable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("changed key did not remove authority: %#v", status)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestReplacementPinnedDataStillDelivers runs the real Delivery path: the old
// authority resolves a recipient and pins it, a second core then authenticates
// and replaces the registry binding, and the old connection still finishes DATA
// on its already-pinned (now replaced, unexpired) binding. Replacement must not
// behave like revocation.
func TestReplacementPinnedDataStillDelivers(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})

	beOld := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	beNew := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	mOld := mxdial.New(beOld, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
	mNew := mxdial.New(beNew, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mOld.Run(ctx)
	ready(t, mOld, "example.test")

	edge := startEdge(t, 2, srv.URL, nil)
	c, e := smtp.Dial(edge)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Hello("sender.test"); e != nil {
		t.Fatal(e)
	}
	if e = c.Mail("sender@outside.test", nil); e != nil {
		t.Fatal(e)
	}
	// Resolve while the old connection still owns the registry: this pins the
	// recipient to the old connection's binding.
	if e = c.Rcpt("alice@example.test", nil); e != nil {
		t.Fatal(e)
	}

	// A second core now authenticates the same domain, replacing the registry
	// binding while the first transaction is still open.
	go mNew.Run(ctx)
	ready(t, mNew, "example.test")

	// The old session should observe "replaced" (not a hard revocation).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := mOld.Status("example.test")
		if len(s) > 0 && s[0].State == "unavailable" && strings.Contains(s[0].Reason, "replaced") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The pinned DATA must still complete on the old connection.
	w, e := c.Data()
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.WriteString(w, "From: sender@outside.test\r\nSubject: test\r\n\r\nbody")
	if e = w.Close(); e != nil {
		t.Fatalf("pinned DATA rejected after replacement: %v", e)
	}
	beOld.mu.Lock()
	beNew.mu.Lock()
	defer beOld.mu.Unlock()
	defer beNew.mu.Unlock()
	if len(beOld.stored) != 1 || len(beNew.stored) != 0 {
		t.Fatalf("expected old pinned binding to deliver: old=%#v new=%#v", beOld.stored, beNew.stored)
	}
}

// TestDNSFailureCooldownBoundsAuthGrowth proves a source that triggers DNS
// failures is cooled down: after the first burst, further auth attempts from the
// same source do no additional DNS work until the cooldown lapses.
func TestDNSFailureCooldownBoundsAuthGrowth(t *testing.T) {
	var mu sync.Mutex
	lookups := 0
	dns := func(context.Context, string) ([]string, error) {
		mu.Lock()
		lookups++
		mu.Unlock()
		return nil, errors.New("dns unavailable")
	}
	_, srv, client := newReceiverServer(t, receiver.Config{LookupTXT: dns})
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}

	burst := func(n int) []*rawSession {
		sessions := make([]*rawSession, 0, n)
		for i := 0; i < n; i++ {
			s := newRawSession(t, httpClient, srv.URL)
			sessions = append(sessions, s)
			auth, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "burst.test", KeyID: "key1"})
			s.write(auth)
		}
		return sessions
	}

	first := burst(10)
	time.Sleep(400 * time.Millisecond)
	mu.Lock()
	afterFirst := lookups
	mu.Unlock()
	for _, s := range first {
		s.close()
	}
	if afterFirst == 0 {
		t.Fatal("expected at least one DNS lookup in the first burst")
	}

	// A second burst inside the cooldown window must add no DNS work.
	second := burst(10)
	time.Sleep(400 * time.Millisecond)
	for _, s := range second {
		s.close()
	}
	mu.Lock()
	afterSecond := lookups
	mu.Unlock()
	if afterSecond != afterFirst {
		t.Fatalf("cooldown did not suppress further DNS work: first=%d second=%d", afterFirst, afterSecond)
	}
}

// fakeDialer is a minimal in-process dialer: it authenticates one domain over
// the real HTTP/2 session protocol, then answers resolve and ingest frames.
// It lets tests exercise the receiver + SMTP edge without the mxdial runtime.
type fakeDialer struct {
	t      *testing.T
	cancel context.CancelFunc
	pw     *io.PipeWriter
	resp   *http.Response
	priv   ed25519.PrivateKey
	domain string
	ready  chan struct{}
	// reject maps recipient -> not accepted.
	reject map[string]bool
	// transient maps recipient -> answered with a transient machine code at
	// ingest time.
	transient map[string]bool
	// dropIngestResult makes the dialer swallow the ingest result, simulating a
	// core that committed but never answered.
	dropIngestResult bool
	// ingestRecipients is the last IngestStart recipient set, used to answer
	// IngestEnd with a per-recipient result.
	mu               sync.Mutex
	ingestRecipients []string
}

func newFakeDialer(t *testing.T, base string, client *tls.Config, priv ed25519.PrivateKey, domain string) *fakeDialer {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: client, ForceAttemptHTTP2: true}}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, base+mxwire.SessionPath, pr)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := httpClient.Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "fake"})
	if e := mxwire.WriteFrame(pw, hello); e != nil {
		t.Fatal(e)
	}
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake dialer handshake timeout")
	}
	if resp == nil || resp.ProtoMajor != 2 {
		t.Fatalf("fake dialer no HTTP/2 session: %#v", resp)
	}
	d := &fakeDialer{t: t, cancel: cancel, pw: pw, resp: resp, priv: priv, domain: domain, ready: make(chan struct{}), reject: map[string]bool{"ghost@example.test": true}}
	go d.run()
	return d
}

func (d *fakeDialer) run() {
	// First frame is the receiver Ready.
	if f, e := mxwire.ReadFrame(d.resp.Body); e != nil || f.Type != mxwire.FrameReady {
		d.t.Errorf("fake dialer expected ready, got %v %v", f.Type, e)
		return
	}
	auth, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: d.domain, KeyID: "key1"})
	if e := mxwire.WriteFrame(d.pw, auth); e != nil {
		return
	}
	for {
		f, e := mxwire.ReadFrame(d.resp.Body)
		if e != nil {
			return
		}
		switch f.Type {
		case mxwire.FrameChallenge:
			var c mxwire.Challenge
			if mxwire.DecodeFrame(f, &c) != nil {
				return
			}
			sig, se := mxwire.SignChallenge(d.priv, c)
			if se != nil {
				return
			}
			proof, _ := mxwire.JSONFrame(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
			_ = mxwire.WriteFrame(d.pw, proof)
		case mxwire.FrameAuthResult:
			var a mxwire.AuthResult
			if mxwire.DecodeFrame(f, &a) == nil && a.Accepted {
				close(d.ready)
			}
		case mxwire.FrameResolve:
			var q mxwire.V2Resolve
			if mxwire.DecodeFrame(f, &q) != nil {
				return
			}
			resp := mxwire.ResolveResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK}
			if d.reject[strings.ToLower(q.Recipient)] {
				resp.Results = []mxwire.ResolveRecipient{{Recipient: q.Recipient, Domain: q.Domain}}
			} else {
				resp.Results = []mxwire.ResolveRecipient{{Recipient: q.Recipient, Domain: q.Domain, Accept: true}}
			}
			out, _ := mxwire.JSONFrame(mxwire.FrameResolveResult, f.TxID, f.ChannelID, resp)
			_ = mxwire.WriteFrame(d.pw, out)
		case mxwire.FrameIngestStart:
			var st mxwire.V2IngestStart
			if mxwire.DecodeFrame(f, &st) == nil {
				d.mu.Lock()
				d.ingestRecipients = append([]string(nil), st.Metadata.Recipients...)
				d.mu.Unlock()
			}
		case mxwire.FrameIngestChunk:
			// Keep draining until end; no reply yet.
		case mxwire.FrameIngestEnd:
			if d.dropIngestResult {
				continue
			}
			d.mu.Lock()
			recips := append([]string(nil), d.ingestRecipients...)
			d.mu.Unlock()
			resp := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeOK}
			for i, r := range recips {
				rr := mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeOK, Disposition: mxwire.DispositionStored, MessageID: fmt.Sprintf("msg-%d", i)}
				if d.transient[strings.ToLower(r)] {
					rr.MachineCode = mxwire.CodeTempFail
					rr.Disposition = ""
					rr.MessageID = ""
				}
				resp.PerRecipient = append(resp.PerRecipient, rr)
			}
			out, _ := mxwire.JSONFrame(mxwire.FrameIngestResult, f.TxID, 0, resp)
			_ = mxwire.WriteFrame(d.pw, out)
		case mxwire.FramePing:
			pong, _ := mxwire.JSONFrame(mxwire.FramePong, 0, 0, struct{}{})
			_ = mxwire.WriteFrame(d.pw, pong)
		}
	}
}

func (d *fakeDialer) waitAuth(t *testing.T) {
	t.Helper()
	select {
	case <-d.ready:
	case <-time.After(4 * time.Second):
		t.Fatal("fake dialer did not authenticate")
	}
}

func (d *fakeDialer) close() {
	d.cancel()
	_ = d.pw.CloseWithError(io.ErrClosedPipe)
	if d.resp != nil {
		_ = d.resp.Body.Close()
	}
}

// rawSession drives one receiver HTTP/2 session at the wire level.
type rawSession struct {
	t      *testing.T
	client *http.Client
	url    string
	cancel context.CancelFunc
	pr     *io.PipeReader
	pw     *io.PipeWriter
	resp   *http.Response
	mu     sync.Mutex
	ch     uint64
}

func newRawSession(t *testing.T, client *http.Client, base string) *rawSession {
	t.Helper()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, base+mxwire.SessionPath, pr)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	done := make(chan *http.Response, 1)
	go func() {
		resp, _ := client.Do(req)
		done <- resp
	}()
	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "raw"})
	if e := mxwire.WriteFrame(pw, hello); e != nil {
		t.Fatal(e)
	}
	var resp *http.Response
	select {
	case resp = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session handshake timeout")
	}
	if resp == nil || resp.ProtoMajor != 2 {
		t.Fatalf("no HTTP/2 session: %#v", resp)
	}
	s := &rawSession{t: t, client: client, url: base, cancel: cancel, pr: pr, pw: pw, resp: resp}
	s.expect(t, mxwire.FrameReady)
	return s
}

func (s *rawSession) write(t mxwire.Frame) {
	if e := mxwire.WriteFrame(s.pw, t); e != nil {
		s.t.Fatal(e)
	}
}

func (s *rawSession) read() mxwire.Frame {
	s.t.Helper()
	f, e := mxwire.ReadFrame(s.resp.Body)
	if e != nil {
		s.t.Fatalf("read frame: %v", e)
	}
	return f
}

func (s *rawSession) expect(t *testing.T, kind mxwire.FrameType) mxwire.Frame {
	t.Helper()
	f := s.read()
	if f.Type != kind {
		t.Fatalf("expected frame %d, got %d", kind, f.Type)
	}
	return f
}

func (s *rawSession) close() {
	s.cancel()
	_ = s.pw.CloseWithError(io.ErrClosedPipe)
	if s.resp != nil {
		_ = s.resp.Body.Close()
	}
}

// startEdge serves the real mxagent SMTP edge for the receiver registered for
// baseURL. The receiver object is resolved from the receiverRegistry so the
// edge's Delivery factory shares the exact receiver under test.
func startEdge(t *testing.T, maxConns int, baseURL string, _ *tls.Config) string {
	t.Helper()
	return startEdgeWithLogger(t, maxConns, baseURL, nil)
}

// startEdgeWithLogger serves the real mxagent SMTP edge sharing the given
// logger, so tests can assert the shared-edge records carry the receiver
// envelope.
func startEdgeWithLogger(t *testing.T, maxConns int, baseURL string, logger *slog.Logger) string {
	t.Helper()
	r := lookupReceiver(t, baseURL)
	edge := mxagent.NewServerWithHandoff(mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 2 << 20, MaxRecipients: 10, MaxConnections: maxConns, DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second}, logger, func() mxagent.Delivery {
		return r.NewDelivery()
	})
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = edge.ListenAndServe(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

var (
	registryMu       sync.Mutex
	receiverRegistry = map[string]*receiver.Receiver{}
)

func registerReceiver(baseURL string, r *receiver.Receiver) {
	registryMu.Lock()
	receiverRegistry[baseURL] = r
	registryMu.Unlock()
}

func lookupReceiver(t *testing.T, baseURL string) *receiver.Receiver {
	t.Helper()
	registryMu.Lock()
	defer registryMu.Unlock()
	r, ok := receiverRegistry[baseURL]
	if !ok {
		t.Fatalf("no receiver registered for %s", baseURL)
	}
	return r
}
