package mxdial

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// Domain is one configured receiving domain the core is willing to speak for.
// The receiver proves control of the CORE signing key, not of the receiver
// domain: the dialer signs a challenge with Domain.PrivateKey and the receiver
// verifies it against the MM1 TXT record published for the core domain.
type Domain struct {
	AccountID    string
	ID           string
	Name         string
	KeyID        string
	PrivateKey   ed25519.PrivateKey
	ReceiverURLs []string
}

// Status is the observable per-domain state for one receiver URL.
type Status struct {
	ReceiverURL  string
	State        string
	Reason       string
	SMTPHostname string
	ExpiresAt    time.Time
}

// Backend is the shared application surface the dialer consumes. Resolve and
// Ingest are only ever called after the receiver has proven control of the
// exact core signing key via a DNS-anchored Ed25519 challenge.
type Backend interface {
	Domains(context.Context) ([]Domain, error)
	Resolve(context.Context, string, []string) (mxwire.ResolveResponse, error)
	Ingest(context.Context, []string, mxwire.IngestMetadata, string) (mxwire.IngestResponse, error)
}

// Config bounds the manager. Zero values select the documented defaults.
type Config struct {
	DataDir            string
	MaxMessageBytes    int64
	MaxTransactions    int
	TLSConfig          *tls.Config
	ReconcileInterval  time.Duration
	TransactionTimeout time.Duration
	// AuthRetryInterval overrides the base rejected-authentication cooldown so
	// deterministic tests need not wait the production window.
	AuthRetryInterval time.Duration
}

const (
	defaultMaxTransactions = 32
	maxRecipientsPerTx     = mxwire.MaxResolveRecipients
	maxDomainsPerIngest    = mxwire.MaxResolveRecipients
	ingestBackendTimeout   = 2 * time.Minute
	resolveBackendTimeout  = 30 * time.Second
	domainsBackendTimeout  = 5 * time.Second
	authRetryMin           = 5 * time.Second
	authRetryMax           = 30 * time.Second
	readyTimeout           = 5 * time.Second
	writeTimeout           = 10 * time.Second
	sessionIdleTimeout     = 90 * time.Second
	keepaliveInterval      = 30 * time.Second
	maxBackoff             = 60 * time.Second
	minStableLifetime      = 30 * time.Second
	frameQueueDepth        = 64
)

// Manager dials every configured receiver URL with a dedicated HTTP/2 session,
// reconciles the configured domain set, and exposes per-domain status. It never
// listens: the shared application's inbound HTTP listener is owned elsewhere.
type Manager struct {
	backend Backend
	cfg     Config
	wake    chan struct{}

	// mu guards only the desired sessions map and the per-receiver statuses. It
	// is never held across a session call, a network operation, or a backend
	// call.
	mu       sync.Mutex
	runCtx   context.Context
	started  bool
	sessions map[string]*session
	status   map[string]Status // key: domain + "\x00" + receiver URL
	stopping bool
	ownWG    sync.WaitGroup

	// slots is the single global transaction cap shared across every
	// connection.
	slots chan struct{}
}

func New(backend Backend, cfg Config) *Manager {
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = mxwire.DefaultMaxBodyBytes
	}
	if cfg.MaxTransactions <= 0 {
		cfg.MaxTransactions = defaultMaxTransactions
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 2 * time.Second
	}
	if cfg.TransactionTimeout <= 0 {
		cfg.TransactionTimeout = 3 * time.Minute
	}
	if cfg.AuthRetryInterval <= 0 {
		cfg.AuthRetryInterval = authRetryMin
	}
	if cfg.TLSConfig != nil {
		// Never permit a caller to disable certificate verification on the
		// production dialer; a custom root pool is the supported override.
		cfg.TLSConfig = cfg.TLSConfig.Clone()
		cfg.TLSConfig.InsecureSkipVerify = false
	}
	return &Manager{
		backend:  backend,
		cfg:      cfg,
		wake:     make(chan struct{}, 1),
		sessions: map[string]*session{},
		status:   map[string]Status{},
		runCtx:   context.Background(),
		slots:    make(chan struct{}, cfg.MaxTransactions),
	}
}

// Wake requests an immediate reconcile pass. It never blocks.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Status returns the current per-receiver status for a canonical domain.
func (m *Manager) Status(domain string) []Status {
	d, err := mxwire.CanonicalDomain(domain)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	var out []Status
	for k, v := range m.status {
		if strings.HasPrefix(k, d+"\x00") {
			out = append(out, v)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ReceiverURL < out[j].ReceiverURL })
	return out
}

// Run reconciles until ctx is canceled, then closes every session and waits for
// all of them to finish. It must be called at most once.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.runCtx = runCtx
	m.mu.Unlock()
	defer cancel()

	t := time.NewTicker(m.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		m.reconcile(runCtx)
		select {
		case <-runCtx.Done():
			m.stopAll()
			return
		case <-m.wake:
		case <-t.C:
		}
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	m.stopping = true
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	m.ownWG.Wait()
}

// reconcile computes the desired URL->domain set and starts, updates or stops
// sessions. It holds m.mu only for map bookkeeping and never calls into a
// session while holding it.
func (m *Manager) reconcile(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, domainsBackendTimeout)
	domains, err := m.backend.Domains(ctx)
	cancel()
	if err != nil {
		return
	}
	desired := map[string]map[string]Domain{}
	for _, d := range domains {
		name, e := mxwire.CanonicalDomain(d.Name)
		if e != nil || len(d.PrivateKey) != ed25519.PrivateKeySize || d.KeyID == "" {
			continue
		}
		d.Name = name
		for _, raw := range d.ReceiverURLs {
			u, e := mxwire.ReceiverURL(raw)
			if e != nil {
				continue
			}
			if desired[u] == nil {
				desired[u] = map[string]Domain{}
			}
			desired[u][name] = d
		}
	}

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	var toClose []*session
	for u, s := range m.sessions {
		if _, ok := desired[u]; !ok {
			delete(m.sessions, u)
			toClose = append(toClose, s)
		}
	}
	var toStart []*session
	type upd struct {
		s  *session
		ds map[string]Domain
	}
	var toUpdate []upd
	for u, ds := range desired {
		if s := m.sessions[u]; s == nil {
			s := m.newSession(u, ds)
			m.sessions[u] = s
			m.ownWG.Add(1)
			toStart = append(toStart, s)
		} else {
			toUpdate = append(toUpdate, upd{s, ds})
		}
	}
	// Drop any status whose session is no longer tracked, so a vanished
	// receiver cannot leave stale rows behind.
	for k := range m.status {
		if url, ok := statusURL(k); !ok || m.sessions[url] == nil {
			delete(m.status, k)
		}
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s.close()
	}
	for _, s := range toStart {
		go s.run()
	}
	for _, u := range toUpdate {
		u.s.update(u.ds)
	}
}

func statusKey(domain, url string) string { return domain + "\x00" + url }

func statusURL(key string) (string, bool) {
	i := strings.IndexByte(key, 0)
	if i < 0 {
		return "", false
	}
	return key[i+1:], true
}

func (m *Manager) setStatus(url, domain, state, reason, host string, expires time.Time) {
	m.mu.Lock()
	if !m.stopping {
		if _, ok := m.sessions[url]; ok {
			m.status[statusKey(domain, url)] = Status{ReceiverURL: url, State: state, Reason: reason, SMTPHostname: host, ExpiresAt: expires}
		}
	}
	m.mu.Unlock()
}

func (m *Manager) clearStatus(url, domain string) {
	m.mu.Lock()
	delete(m.status, statusKey(domain, url))
	m.mu.Unlock()
}

func keyFingerprint(d Domain) string {
	h := sha256.New()
	h.Write([]byte(d.KeyID))
	h.Write([]byte{0})
	h.Write(d.PrivateKey)
	h.Write([]byte{0})
	for _, u := range d.ReceiverURLs {
		h.Write([]byte(u))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sameDomains(a, b map[string]Domain) bool {
	if len(a) != len(b) {
		return false
	}
	for name, da := range a {
		db, ok := b[name]
		if !ok || keyFingerprint(da) != keyFingerprint(db) {
			return false
		}
	}
	return true
}

// authState is the per-domain authentication state machine, replacing the
// former collection of independent booleans.
type authState uint8

const (
	authPending  authState = iota // DomainAuth sent, awaiting a challenge within deadline
	authActive                    // proof signed and accepted; authorized until expires
	authRejected                  // rejected; retryAt gates the next attempt
	authReplaced                  // granted elsewhere; parked, no new resolve
	authRevoked                   // authority withdrawn
)

// auth is one domain's authentication binding, owned exclusively by the
// connection loop goroutine.
type auth struct {
	domain     Domain
	channel    uint64
	generation string
	state      authState
	expires    time.Time // active authorization expiry
	deadline   time.Time // pending proof window
	retryAt    time.Time
	// proof* bind the challenge we actually signed: an AuthResult is only
	// trusted when the nonce and session identity match the local challenge.
	proofNonce string
}

// txState is the transaction state machine, owned by the loop.
type txState uint8

const (
	txResolving  txState = iota // accepting sequential recipient resolves; no ingest yet
	txStaging                   // chunks accumulating into the staging file
	txCommitting                // backend ingest in flight
	txCancelled
)

// tx is one receiver transaction, owned exclusively by the connection loop.
type tx struct {
	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time

	domains    map[string]uint64 // domain -> pinned channel
	recipients map[string]map[string]bool
	accepted   map[string]bool

	path string
	file *os.File
	hash interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	size int64
	seq  uint32
	meta mxwire.IngestMetadata
	busy bool
	// resolveBusy forbids a second outstanding resolve for this transaction:
	// SMTP recipients are resolved one at a time per transaction.
	resolveBusy bool
	state       txState
}

// session owns one receiver URL. All auth and transaction maps are touched only
// by the run goroutine; no lock protects them.
type session struct {
	m   *Manager
	url string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	updateCh chan map[string]Domain
	// wantMu guards the latest configuration delivered by reconcile.
	wantMu  sync.Mutex
	desired map[string]Domain

	// lifeMu guards the physical connection handles so close can tear them down
	// without racing connect.
	lifeMu        sync.Mutex
	writer        *io.PipeWriter
	requestCancel context.CancelFunc

	// outMu serializes frame writes on the current physical connection.
	outMu sync.Mutex

	// loop-owned state, reset at the start of every physical connection.
	domains map[string]Domain
	auths   map[string]*auth
	byChan  map[uint64]*auth
	nextCh  uint64
	ready   mxwire.Ready
	txs     map[uint64]*tx
	sawAuth bool // an auth reached active during this connection

	// jobs carries backend worker completions to the loop, including cancellation.
	jobs          chan jobResult
	connectionCtx context.Context
	// connWG tracks resolve/ingest workers for the current connection.
	connWG sync.WaitGroup
}

func (m *Manager) newSession(u string, initial map[string]Domain) *session {
	ctx, cancel := context.WithCancel(m.runCtx)
	return &session{
		m:        m,
		url:      u,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		updateCh: make(chan map[string]Domain, 1),
		desired:  initial,
	}
}

// update queues a desired-configuration change onto the session loop. It never
// blocks and never touches the writer.
func (s *session) update(ds map[string]Domain) {
	s.wantMu.Lock()
	changed := !sameDomains(s.desired, ds)
	if changed {
		s.desired = ds
	}
	s.wantMu.Unlock()
	if !changed {
		return
	}
	select {
	case s.updateCh <- ds:
	default:
	}
}

func (s *session) close() {
	s.cancel()
	s.lifeMu.Lock()
	if s.requestCancel != nil {
		s.requestCancel()
	}
	if s.writer != nil {
		_ = s.writer.CloseWithError(io.ErrClosedPipe)
	}
	s.lifeMu.Unlock()
	<-s.done
}

func (m *Manager) closeSession(s *session) { m.ownWG.Done() }

func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(time.Now().UnixNano()%int64(half+1))
}

// run owns the session lifetime: it reconnects until the session is closed.
func (s *session) run() {
	defer s.m.closeSession(s)
	defer close(s.done)

	backoff := time.Second
	for s.ctx.Err() == nil {
		started := time.Now()
		s.sawAuth = false
		err := s.connect()
		live := s.trackedDomains()
		s.resetConnection()
		if s.ctx.Err() != nil {
			return
		}
		// Only a session that stayed authorized for a meaningful period resets
		// the backoff; a mere Ready handshake is not enough, or a flapping
		// receiver would storm.
		if s.sawAuth && time.Since(started) >= minStableLifetime {
			backoff = time.Second
		}
		reason := "disconnected"
		if errors.Is(err, errHTTP1) {
			reason = "http2_required"
		} else if errors.Is(err, errIdle) {
			reason = "idle_timeout"
		}
		for _, d := range live {
			s.m.setStatus(s.url, d, "disconnected", reason, "", time.Time{})
		}
		wait := jitter(backoff)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		case <-s.updateCh:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (s *session) trackedDomains() []string {
	out := make([]string, 0, len(s.domains))
	for d := range s.domains {
		out = append(out, d)
	}
	return out
}

var (
	errHTTP1 = errors.New("HTTP/2 required")
	errIdle  = errors.New("session idle timeout")
)

// connect performs one logical session. The connection loop it runs owns all
// auth and transaction state for the connection's lifetime; backend workers
// report back through s.jobs.
func (s *session) connect() error {
	pr, pw := io.Pipe()
	s.lifeMu.Lock()
	if s.ctx.Err() != nil {
		s.lifeMu.Unlock()
		_ = pw.CloseWithError(io.ErrClosedPipe)
		_ = pr.CloseWithError(io.ErrClosedPipe)
		return s.ctx.Err()
	}
	s.writer = pw
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.m.cfg.TLSConfig != nil {
		tlsCfg = s.m.cfg.TLSConfig.Clone()
	}
	tr := &http.Transport{
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: readyTimeout,
	}
	reqCtx, cancel := context.WithCancel(s.ctx)
	s.connectionCtx = reqCtx
	s.requestCancel = cancel
	s.lifeMu.Unlock()

	// Tear down every physical handle exactly once on return.
	defer func() {
		s.lifeMu.Lock()
		s.requestCancel = nil
		s.writer = nil
		s.lifeMu.Unlock()
		cancel()
		_ = pw.CloseWithError(io.ErrClosedPipe)
		_ = pr.CloseWithError(io.ErrClosedPipe)
		tr.CloseIdleConnections()
	}()

	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.url+mxwire.SessionPath, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	type doResult struct {
		resp *http.Response
		err  error
	}
	done := make(chan doResult, 1)
	go func() {
		resp, e := client.Do(req)
		done <- doResult{resp, e}
	}()

	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "gatehouse"})
	if err = s.write(hello); err != nil {
		return err
	}

	var resp *http.Response
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-reqCtx.Done():
		return reqCtx.Err()
	case <-time.After(readyTimeout):
		return errors.New("handshake timeout")
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		resp = r.resp
	}
	if resp.ProtoMajor != 2 {
		return errHTTP1
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("session rejected")
	}
	defer resp.Body.Close()

	// Fresh per-connection state, owned solely by this loop.
	s.wantMu.Lock()
	s.domains = map[string]Domain{}
	for name, d := range s.desired {
		s.domains[name] = d
	}
	s.wantMu.Unlock()
	s.auths = map[string]*auth{}
	s.byChan = map[uint64]*auth{}
	s.nextCh = 0
	s.txs = map[uint64]*tx{}
	s.jobs = make(chan jobResult, s.m.cfg.MaxTransactions)

	ready, err := s.readReady(resp.Body)
	if err != nil {
		return err
	}
	s.ready = ready
	if err = s.authenticateAll(); err != nil {
		return err
	}

	// Reader goroutine: bounded frame queue plus a terminal error result. It
	// never touches loop-owned state.
	type frameResult struct {
		frame mxwire.Frame
		err   error
	}
	frames := make(chan frameResult, frameQueueDepth)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			f, e := mxwire.ReadFrame(resp.Body)
			select {
			case frames <- frameResult{f, e}:
			case <-reqCtx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = resp.Body.Close()
		<-readerDone
	}()

	ping := time.NewTicker(keepaliveInterval)
	defer ping.Stop()
	authTick := time.NewTicker(time.Second)
	defer authTick.Stop()
	idle := time.NewTimer(sessionIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-reqCtx.Done():
			return reqCtx.Err()
		case <-authTick.C:
			now := time.Now()
			for id, t := range s.txs {
				if !now.Before(t.start.Add(s.m.cfg.TransactionTimeout)) {
					s.cancelTx(id)
				}
			}
			if err = s.authenticateAll(); err != nil {
				return err
			}
		case <-ping.C:
			if err = s.writeJSON(mxwire.FramePing, 0, 0, struct{}{}); err != nil {
				return err
			}
		case <-idle.C:
			return errIdle
		case <-s.updateCh:
			// A configuration change while connected: fold it in, unregister
			// changed/removed domains, and re-auth. Does not tear the
			// connection.
			s.applyConfig()
			if err = s.authenticateAll(); err != nil {
				return err
			}
		case r := <-s.jobs:
			s.completeJob(r)
		case r := <-frames:
			if r.err != nil {
				return r.err
			}
			resetTimer(idle, sessionIdleTimeout)
			if err = s.handleFrame(r.frame); err != nil {
				return err
			}
		}
	}
}

func (s *session) readReady(body io.Reader) (mxwire.Ready, error) {
	type result struct {
		f   mxwire.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() { f, e := mxwire.ReadFrame(body); ch <- result{f, e} }()
	select {
	case <-s.ctx.Done():
		return mxwire.Ready{}, s.ctx.Err()
	case <-time.After(readyTimeout):
		return mxwire.Ready{}, errors.New("ready timeout")
	case r := <-ch:
		if r.err != nil {
			return mxwire.Ready{}, r.err
		}
		if r.f.Type != mxwire.FrameReady || r.f.TxID != 0 || r.f.ChannelID != 0 {
			return mxwire.Ready{}, errors.New("invalid ready")
		}
		var ready mxwire.Ready
		if mxwire.DecodeFrame(r.f, &ready) != nil || ready.Version != mxwire.V2Protocol || ready.ReceiverID == "" || ready.ConnectionID == "" || ready.MaxMessageBytes <= 0 {
			return mxwire.Ready{}, errors.New("invalid ready")
		}
		if len(ready.ReceiverID) > 128 || len(ready.ConnectionID) > 128 {
			return mxwire.Ready{}, errors.New("invalid ready identity")
		}
		return ready, nil
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// resetConnection runs after connect returns: cancel every transaction, wait
// for backend workers to finish (so no file is deleted while in use), then
// discard connection state.
func (s *session) resetConnection() {
	for _, t := range s.txs {
		t.cancel()
	}
	s.connWG.Wait()
	for _, t := range s.txs {
		t.cleanup(s)
	}
	s.domains = map[string]Domain{}
	s.auths = map[string]*auth{}
	s.byChan = map[uint64]*auth{}
	s.txs = map[uint64]*tx{}
}

func (s *session) handleFrame(f mxwire.Frame) error {
	switch f.Type {
	case mxwire.FrameChallenge:
		return s.challenge(f)
	case mxwire.FrameAuthResult:
		return s.authResult(f)
	case mxwire.FrameDomainRevoked:
		return s.notice(f, true)
	case mxwire.FrameDomainUnregister:
		return s.notice(f, false)
	case mxwire.FramePing:
		if f.TxID != 0 || f.ChannelID != 0 {
			return errors.New("invalid ping")
		}
		return s.writeJSON(mxwire.FramePong, 0, 0, struct{}{})
	case mxwire.FramePong:
		return nil
	case mxwire.FrameResolve:
		return s.resolve(f)
	case mxwire.FrameIngestStart, mxwire.FrameIngestChunk, mxwire.FrameIngestEnd, mxwire.FrameCancel:
		return s.ingestFrame(f)
	default:
		return errors.New("unexpected frame")
	}
}

// applyConfig folds the latest desired configuration into the loop's domain and
// auth maps. A changed or removed domain is unregistered and its locally-bound
// transactions are aborted; nothing is inherited from the previous setting.
func (s *session) applyConfig() {
	s.wantMu.Lock()
	ds := s.desired
	s.wantMu.Unlock()
	if ds == nil {
		ds = map[string]Domain{}
	}
	var removed []string
	for name, prev := range s.domains {
		next, ok := ds[name]
		if ok && keyFingerprint(prev) == keyFingerprint(next) {
			continue
		}
		removed = append(removed, name)
	}
	s.domains = map[string]Domain{}
	for name, d := range ds {
		s.domains[name] = d
	}
	for _, name := range removed {
		if ad := s.auths[name]; ad != nil {
			_ = s.writeJSON(mxwire.FrameDomainUnregister, 0, ad.channel, mxwire.DomainNotice{Domain: name, Reason: "configuration_changed"})
		}
		s.m.clearStatus(s.url, name)
		s.retire(name)
		s.cancelDomain(name)
	}
}

// authenticateAll ensures every configured domain has an in-flight or live
// auth, re-issuing for unauthenticated, expired, rejected-after-retry, or
// superseded domains.
func (s *session) authenticateAll() error {
	now := time.Now()
	var pending []*auth
	for name, d := range s.domains {
		if ad := s.auths[name]; ad != nil && ad.generation == keyFingerprint(d) {
			skip := false
			switch ad.state {
			case authActive:
				skip = now.Before(ad.expires)
			case authPending:
				skip = now.Before(ad.deadline)
			case authRejected:
				skip = now.Before(ad.retryAt)
			case authReplaced:
				// Granted to another connection: park the pin, never reclaim
				// via a fresh auth. Only a config change resets it.
				skip = true
			}
			if skip {
				continue
			}
		}
		s.retire(name)
		s.nextCh++
		ad := &auth{
			domain:     d,
			channel:    s.nextCh,
			generation: keyFingerprint(d),
			state:      authPending,
			deadline:   now.Add(10 * time.Second),
		}
		s.auths[name] = ad
		s.byChan[ad.channel] = ad
		pending = append(pending, ad)
	}
	for _, ad := range pending {
		if err := s.writeJSON(mxwire.FrameDomainAuth, 0, ad.channel, mxwire.DomainAuth{Domain: ad.domain.Name, KeyID: ad.domain.KeyID}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) retire(name string) {
	if ad := s.auths[name]; ad != nil {
		delete(s.byChan, ad.channel)
	}
	delete(s.auths, name)
}

// challenge answers a challenge. A revalidation reuses the same channel and
// records only a fresh proof deadline: it never overwrites a live
// authorization's expiry, so a renewal cannot shorten it.
func (s *session) challenge(f mxwire.Frame) error {
	if f.TxID != 0 {
		return errors.New("invalid challenge tx")
	}
	var c mxwire.Challenge
	if mxwire.DecodeFrame(f, &c) != nil {
		return errors.New("invalid challenge")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		// Stale frame for a retired channel: ignore rather than tear down.
		return nil
	}
	if c.Domain != ad.domain.Name || c.KeyID != ad.domain.KeyID || c.ReceiverID != s.ready.ReceiverID || c.ConnectionID != s.ready.ConnectionID {
		return errors.New("unknown challenge channel")
	}
	sig, err := mxwire.SignChallenge(ad.domain.PrivateKey, c)
	if err != nil {
		return err
	}
	// Record the exact challenge we signed; only a matching AuthResult issued
	// after local signing may activate the binding.
	ad.proofNonce = c.Nonce
	ad.deadline = time.Now().Add(10 * time.Second)
	return s.writeJSON(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
}

func (s *session) authResult(f mxwire.Frame) error {
	if f.TxID != 0 {
		return errors.New("invalid auth result tx")
	}
	var a mxwire.AuthResult
	if mxwire.DecodeFrame(f, &a) != nil {
		return errors.New("invalid auth result")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		return nil
	}
	if a.Domain != ad.domain.Name || a.KeyID != ad.domain.KeyID {
		return errors.New("invalid auth identity")
	}
	now := time.Now()
	if !a.Accepted {
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = now.Add(s.retryDelay(a.Reason))
		s.cancelDomain(a.Domain)
		s.m.setStatus(s.url, a.Domain, "rejected", authReason(a.Reason), s.ready.SMTPHostname, ad.expires)
		return nil
	}
	// An accepted result is trusted only after we locally signed the matching
	// challenge for this exact identity and connection, and while the issued
	// expiry is sane.
	if ad.proofNonce == "" || !now.Before(ad.deadline) || !a.ExpiresAt.After(now) || a.ExpiresAt.After(now.Add(mxwire.AuthLifetime)) {
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = now.Add(s.m.cfg.AuthRetryInterval)
		s.m.setStatus(s.url, a.Domain, "rejected", "authentication_failed", s.ready.SMTPHostname, time.Time{})
		return nil
	}
	// A renewal must not shorten a still-valid authorization window.
	if ad.state == authActive && now.Before(ad.expires) && a.ExpiresAt.Before(ad.expires) {
		ad.proofNonce = ""
		s.m.setStatus(s.url, a.Domain, "ready", "", s.ready.SMTPHostname, ad.expires)
		return nil
	}
	ad.state = authActive
	ad.proofNonce = ""
	ad.expires = a.ExpiresAt
	s.sawAuth = true
	s.m.setStatus(s.url, a.Domain, "ready", "", s.ready.SMTPHostname, a.ExpiresAt)
	return nil
}

func (s *session) retryDelay(reason string) time.Duration {
	base := s.m.cfg.AuthRetryInterval
	if reason == "dns_unavailable" || reason == "key_unavailable" || reason == "domain_limit" {
		if authRetryMax > base {
			return authRetryMax
		}
	}
	return base
}

func authReason(reason string) string {
	switch reason {
	case "dns_unavailable", "key_unavailable":
		return "key_unavailable"
	case "domain_limit":
		return "domain_limit"
	default:
		return "authentication_failed"
	}
}

func (s *session) notice(f mxwire.Frame, revoked bool) error {
	if f.TxID != 0 {
		return errors.New("invalid notice tx")
	}
	var n mxwire.DomainNotice
	if mxwire.DecodeFrame(f, &n) != nil {
		return errors.New("invalid domain notice")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		return nil
	}
	if n.Domain != ad.domain.Name {
		return errors.New("invalid notice channel")
	}
	reason := n.Reason
	switch {
	case n.Reason == "replaced":
		// The receiver holds a newer binding from another connection. Park the
		// pin: no new resolve and no re-auth reclaim, but already-pinned
		// transactions may still ingest DATA until the old authorization
		// expires.
		ad.state = authReplaced
		ad.proofNonce = ""
	case revoked || n.Reason == "revoked" || n.Reason == "auth_expired":
		ad.state = authRevoked
		delete(s.byChan, f.ChannelID)
		reason = "revoked"
	default:
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = time.Now().Add(authRetryMax)
		reason = "domain_unavailable"
	}
	if ad.state != authReplaced {
		s.cancelDomain(n.Domain)
	}
	s.m.setStatus(s.url, n.Domain, "unavailable", reason, "", ad.expires)
	return nil
}

// resolve handles one recipient lookup. At most one resolve may be outstanding
// per transaction; different transactions may resolve concurrently.
func (s *session) resolve(f mxwire.Frame) error {
	if f.TxID == 0 || f.ChannelID == 0 {
		return errors.New("invalid resolve ids")
	}
	var q mxwire.V2Resolve
	if mxwire.DecodeFrame(f, &q) != nil {
		return errors.New("invalid resolve")
	}
	domain, err := mxwire.CanonicalDomain(q.Domain)
	if err != nil || domain != q.Domain {
		return errors.New("invalid resolve domain")
	}
	recipient := strings.ToLower(strings.TrimSpace(q.Recipient))
	parts := strings.Split(recipient, "@")
	if len(parts) != 2 || parts[1] != domain || len(parts[0]) == 0 {
		return errors.New("invalid recipient")
	}
	ad := s.byChan[f.ChannelID]
	if !s.authorized(ad, domain, f.ChannelID) {
		return s.writeJSON(mxwire.FrameResolveResult, f.TxID, f.ChannelID, mxwire.ResolveResponse{MachineCode: mxwire.CodeUnauthorized})
	}
	t, err := s.getTx(f.TxID)
	if err != nil || t.state != txResolving || t.resolveBusy || len(t.accepted) >= maxRecipientsPerTx {
		return s.writeJSON(mxwire.FrameResolveResult, f.TxID, f.ChannelID, tempFailResolve(domain, recipient))
	}
	t.resolveBusy = true
	jobs, connectionCtx := s.jobs, s.connectionCtx
	txctx := t.ctx
	s.connWG.Add(1)
	go func() {
		defer s.connWG.Done()
		ctx, cancel := context.WithTimeout(txctx, resolveBackendTimeout)
		defer cancel()
		res, err := s.m.backend.Resolve(ctx, domain, []string{recipient})
		if err != nil {
			res = mxwire.ResolveResponse{MachineCode: mxwire.CodeTempFail}
		}
		res = normalizeResolve(res, domain, recipient)
		select {
		case jobs <- jobResult{kind: jobResolve, txid: f.TxID, channel: f.ChannelID, domain: domain, recipient: recipient, resolve: res}:
		case <-connectionCtx.Done():
		}
	}()
	return nil
}

// authorized reports whether a channel carries live authority for the domain.
// Replaced bindings are not authorized for new work.
func (s *session) authorized(ad *auth, domain string, channel uint64) bool {
	if ad == nil || ad.state != authActive {
		return false
	}
	return s.auths[domain] == ad && ad.channel == channel && time.Now().Before(ad.expires)
}

// pinnedAuthorized reports whether a transaction may still ingest against a
// domain. A replaced binding that is still authorized and unchanged may finish
// DATA for an already-pinned transaction.
func (s *session) pinnedAuthorized(domain string, pinned uint64) bool {
	cur, ok := s.auths[domain]
	if !ok || cur.channel != pinned {
		return false
	}
	return (cur.state == authActive || cur.state == authReplaced) && time.Now().Before(cur.expires)
}

var errTxLimit = errors.New("transaction limit")

// getTx returns the transaction for id, creating it when necessary. Creation
// consumes one global slot; at the cap it returns errTxLimit and callers answer
// per-recipient instead of closing the connection.
func (s *session) getTx(id uint64) (*tx, error) {
	if t := s.txs[id]; t != nil {
		return t, nil
	}
	select {
	case s.m.slots <- struct{}{}:
	default:
		return nil, errTxLimit
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.m.cfg.TransactionTimeout)
	t := &tx{
		ctx:        ctx,
		cancel:     cancel,
		start:      time.Now(),
		domains:    map[string]uint64{},
		recipients: map[string]map[string]bool{},
		accepted:   map[string]bool{},
		state:      txResolving,
	}
	s.txs[id] = t
	return t, nil
}

// cleanup closes the staging file, removes it, and frees the global slot. It is
// called only after any in-flight backend ingest has finished, so the file is
// never deleted while a worker holds it.
func (t *tx) cleanup(s *session) {
	t.cancel()
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
	if t.path != "" {
		_ = os.Remove(t.path)
		t.path = ""
	}
	select {
	case <-s.m.slots:
	default:
	}
}

// cancelTx cancels a transaction. If a backend ingest is in flight the
// transaction stays in the map until its completion runs cleanup, so the file
// is not deleted under the worker.
func (s *session) cancelTx(id uint64) {
	t := s.txs[id]
	if t == nil {
		return
	}
	t.state = txCancelled
	if t.busy || t.resolveBusy {
		t.cancel()
		return
	}
	delete(s.txs, id)
	t.cleanup(s)
}

func (s *session) ingestFrame(f mxwire.Frame) error {
	switch f.Type {
	case mxwire.FrameCancel:
		if f.ChannelID != 0 {
			return errors.New("invalid cancel channel")
		}
		s.cancelTx(f.TxID)
		return nil
	case mxwire.FrameIngestStart:
		return s.ingestStart(f)
	case mxwire.FrameIngestChunk:
		return s.ingestChunk(f)
	case mxwire.FrameIngestEnd:
		return s.ingestEnd(f)
	}
	return errors.New("unexpected transaction frame")
}

func (s *session) ingestStart(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest start channel")
	}
	var start mxwire.V2IngestStart
	if mxwire.DecodeFrame(f, &start) != nil {
		return errors.New("invalid ingest start")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	if t.ctx.Err() != nil {
		return errors.New("transaction expired")
	}
	// Ingest begins only in the resolving phase, after every resolve answered.
	if t.state != txResolving || t.resolveBusy {
		return errors.New("invalid ingest start state")
	}
	if len(start.Domains) == 0 || len(start.Domains) > maxDomainsPerIngest || len(start.Metadata.Recipients) == 0 || len(start.Metadata.Recipients) > maxRecipientsPerTx {
		return errors.New("invalid ingest bounds")
	}
	if start.Metadata.Size <= 0 || start.Metadata.Size > s.m.cfg.MaxMessageBytes {
		return errors.New("invalid ingest size")
	}
	if _, err := hex.DecodeString(start.Metadata.ContentDigest); err != nil || len(start.Metadata.ContentDigest) != 64 {
		return errors.New("invalid ingest digest")
	}

	seen := map[string]bool{}
	for _, raw := range start.Domains {
		d, e := mxwire.CanonicalDomain(raw)
		pinned := t.domains[d]
		if e != nil || d != raw || seen[d] || pinned == 0 || len(t.recipients[d]) == 0 || !s.pinnedAuthorized(d, pinned) {
			return errors.New("unauthorized ingest domain")
		}
		seen[d] = true
	}
	if len(seen) != len(t.domains) {
		return errors.New("ingest domain mismatch")
	}

	recips := map[string]bool{}
	for _, r := range start.Metadata.Recipients {
		r = strings.ToLower(strings.TrimSpace(r))
		parts := strings.Split(r, "@")
		if len(parts) != 2 || !seen[parts[1]] || !t.recipients[parts[1]][r] || recips[r] {
			return errors.New("unresolved ingest recipient")
		}
		recips[r] = true
	}
	if len(recips) != len(t.accepted) {
		return fmt.Errorf("ingest recipient set mismatch: got %d, expected %d", len(recips), len(t.accepted))
	}
	for r := range t.accepted {
		if !recips[r] {
			return errors.New("incomplete ingest recipient set")
		}
	}
	if enc, err := encodedMetadataLen(start.Metadata); err != nil || enc > mxwire.MaxMetadataBytes {
		return errors.New("invalid ingest metadata")
	}

	root := filepath.Join(s.m.cfg.DataDir, "messages", ".tmp")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(root, "mxdial-*")
	if err != nil {
		return err
	}
	_ = file.Chmod(0600)
	t.meta = start.Metadata
	t.file = file
	t.path = file.Name()
	t.hash = sha256.New()
	t.state = txStaging
	return nil
}

func encodedMetadataLen(m mxwire.IngestMetadata) (int, error) {
	// JSONFrame requires a non-zero transaction id; a dummy id is used because
	// only the encoded payload length is measured.
	f, err := mxwire.JSONFrame(mxwire.FrameIngestStart, 1, 0, mxwire.V2IngestStart{Domains: []string{"x.test"}, Metadata: m})
	if err != nil {
		return 0, err
	}
	return len(f.Payload), nil
}

func (s *session) ingestChunk(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest chunk channel")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	if t.ctx.Err() != nil || t.state != txStaging || t.file == nil {
		return errors.New("invalid ingest chunk state")
	}
	seq, data, err := mxwire.DecodeChunk(f)
	if err != nil || seq != t.seq || t.size+int64(len(data)) > s.m.cfg.MaxMessageBytes {
		return errors.New("invalid ingest chunk")
	}
	if _, err = t.file.Write(data); err != nil {
		return err
	}
	_, _ = t.hash.Write(data)
	t.size += int64(len(data))
	t.seq++
	return nil
}

func (s *session) ingestEnd(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest end channel")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	var end mxwire.V2IngestEnd
	if mxwire.DecodeFrame(f, &end) != nil {
		return errors.New("invalid ingest end")
	}
	if t.ctx.Err() != nil || t.state != txStaging || t.file == nil {
		return errors.New("invalid ingest end state")
	}
	// Re-check authority and local configuration at the end of the stream: a
	// domain may have been revoked or reconfigured while chunks were arriving.
	for d, pinned := range t.domains {
		if !s.pinnedAuthorized(d, pinned) {
			return errors.New("ingest authority lapsed")
		}
		if _, ok := s.domains[d]; !ok {
			return errors.New("ingest configuration changed")
		}
	}
	if end.Size != t.size || end.Size != t.meta.Size || end.ContentDigest != hex.EncodeToString(t.hash.Sum(nil)) || end.ContentDigest != t.meta.ContentDigest {
		return errors.New("ingest integrity mismatch")
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	_ = t.file.Close()
	t.file = nil
	t.state = txCommitting
	t.busy = true

	path, meta := t.path, t.meta
	jobs, connectionCtx := s.jobs, s.connectionCtx
	txctx := t.ctx
	domains := make([]string, 0, len(t.domains))
	for d := range t.domains {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	s.connWG.Add(1)
	go func() {
		defer s.connWG.Done()
		ctx, cancel := context.WithTimeout(txctx, ingestBackendTimeout)
		defer cancel()
		response, err := s.m.backend.Ingest(ctx, domains, meta, path)
		if err != nil {
			response = mxwire.IngestResponse{MachineCode: mxwire.CodeTempFail}
		}
		response = normalizeIngest(response, meta.Recipients)
		select {
		case jobs <- jobResult{kind: jobIngest, txid: f.TxID, ingest: response}:
		case <-connectionCtx.Done():
		}
	}()
	return nil
}

// jobKind distinguishes backend completions.
type jobKind uint8

const (
	jobResolve jobKind = iota
	jobIngest
)

// jobResult is an immutable backend completion delivered to the loop.
type jobResult struct {
	kind      jobKind
	txid      uint64
	channel   uint64
	domain    string
	recipient string
	resolve   mxwire.ResolveResponse
	ingest    mxwire.IngestResponse
}

// completeJob applies a worker result on the loop goroutine. Pins are updated
// before any result is emitted, so a subsequent ingest can never observe a
// half-applied resolve, and resolveBusy is cleared before the result is sent.
func (s *session) completeJob(r jobResult) {
	t := s.txs[r.txid]
	if t == nil {
		return
	}
	if t.state == txCancelled || t.ctx.Err() != nil {
		delete(s.txs, r.txid)
		t.cleanup(s)
		return
	}
	switch r.kind {
	case jobResolve:
		t.resolveBusy = false
		if t.ctx.Err() != nil {
			return
		}
		if !s.authorized(s.byChan[r.channel], r.domain, r.channel) {
			r.resolve = tempFailResolve(r.domain, r.recipient)
		}
		accepted := false
		for _, rr := range r.resolve.Results {
			if strings.EqualFold(rr.Recipient, r.recipient) && rr.Accept {
				accepted = true
				break
			}
		}
		switch {
		case accepted && len(t.accepted) >= maxRecipientsPerTx:
			// At the recipient cap an otherwise-acceptable recipient is a
			// temporary failure for this transaction.
			r.resolve = tempFailResolve(r.domain, r.recipient)
		case accepted:
			if t.recipients[r.domain] == nil {
				t.recipients[r.domain] = map[string]bool{}
			}
			t.recipients[r.domain][r.recipient] = true
			t.accepted[r.recipient] = true
			if t.domains[r.domain] == 0 {
				t.domains[r.domain] = r.channel
			}
		}
		// A core decision of "unknown/denied recipient" (accept=false) is a
		// permanent result and is preserved verbatim; only a genuinely missing
		// or errored backend result was already normalized to a temporary
		// failure.
		_ = s.writeJSON(mxwire.FrameResolveResult, r.txid, r.channel, r.resolve)
	case jobIngest:
		t.busy = false
		canceled := t.ctx.Err() != nil
		delete(s.txs, r.txid)
		// Emit before cleanup cancels the transaction context. Results are only
		// sent after the backend has returned and its durable commit completed.
		if !canceled {
			_ = s.writeJSON(mxwire.FrameIngestResult, r.txid, 0, r.ingest)
		}
		t.cleanup(s)
	}
}

func tempFailResolve(domain, recipient string) mxwire.ResolveResponse {
	return mxwire.ResolveResponse{
		MachineCode: mxwire.CodeTempFail,
		Results:     []mxwire.ResolveRecipient{{Recipient: recipient, Domain: domain, Temporary: true, Code: string(mxwire.CodeTempFail)}},
	}
}

// normalizeResolve guarantees exactly one result for the requested recipient,
// scoped to this domain, so a misbehaving backend cannot smuggle extra
// recipients into a transaction. A missing result is a temporary failure; a
// core decision of "unknown recipient" (accept=false) is preserved as
// permanent and never upgraded to a temporary failure.
func normalizeResolve(res mxwire.ResolveResponse, domain, recipient string) mxwire.ResolveResponse {
	var out mxwire.ResolveResponse
	out.Version = mxwire.V2Protocol
	out.MachineCode = res.MachineCode
	for _, r := range res.Results {
		if !strings.EqualFold(strings.TrimSpace(r.Recipient), recipient) {
			continue
		}
		r.Recipient, r.Domain = recipient, domain
		out.Results = []mxwire.ResolveRecipient{r}
		return out
	}
	return tempFailResolve(domain, recipient)
}

// normalizeIngest ensures every accepted recipient has exactly one result and
// no spurious recipients leak through. The backend's per-recipient machine
// codes are preserved; only a genuinely missing result is filled with a
// temporary failure. No authorization-OK result is ever invented.
func normalizeIngest(res mxwire.IngestResponse, recipients []string) mxwire.IngestResponse {
	want := make([]string, 0, len(recipients))
	seen := map[string]bool{}
	for _, r := range recipients {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		want = append(want, r)
	}
	sort.Strings(want)
	byRecipient := map[string]mxwire.RecipientIngestResult{}
	for _, r := range res.PerRecipient {
		k := strings.ToLower(strings.TrimSpace(r.Recipient))
		if _, ok := byRecipient[k]; !ok {
			byRecipient[k] = r
		}
	}
	out := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: res.MachineCode, MessageID: res.MessageID}
	for _, r := range want {
		rr, ok := byRecipient[r]
		if !ok {
			rr = mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeTempFail, Reason: "no_result"}
		}
		rr.Recipient = r
		out.PerRecipient = append(out.PerRecipient, rr)
	}
	if out.MachineCode == "" {
		out.MachineCode = mxwire.CodeOK
	}
	return out
}

// cancelDomain cancels every transaction pinned to a domain.
func (s *session) cancelDomain(domain string) {
	for id, t := range s.txs {
		_, ok := t.domains[domain]
		_, accepted := t.recipients[domain]
		if ok || accepted {
			s.cancelTx(id)
		}
	}
}

// write serializes all frame writes for the current physical connection and
// captures the writer generation, so a write queued across a reconnect cannot
// touch the next connection.
func (s *session) write(f mxwire.Frame) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.lifeMu.Lock()
	w, ctxErr := s.writer, s.ctx.Err()
	s.lifeMu.Unlock()
	if ctxErr != nil {
		return ctxErr
	}
	if w == nil {
		return io.ErrClosedPipe
	}
	stop := time.AfterFunc(writeTimeout, func() { _ = w.CloseWithError(context.DeadlineExceeded) })
	defer stop.Stop()
	return mxwire.WriteFrame(w, f)
}

func (s *session) writeJSON(t mxwire.FrameType, tx, ch uint64, v any) error {
	f, err := mxwire.JSONFrame(t, tx, ch, v)
	if err != nil {
		return err
	}
	return s.write(f)
}
