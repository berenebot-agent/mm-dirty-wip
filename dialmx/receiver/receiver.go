package receiver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

const (
	challengeTTL = 10 * time.Second

	// Bounds for source-IP behaviour. The per-IP connection cap stops one
	// sender consuming every connection slot; the per-IP connection window
	// bounds how many sessions one source may open per minute so short-lived
	// cycling cannot evade the concurrent cap; the auth window bounds how many
	// challenges a source may start per minute so a burst of DomainAuth frames
	// cannot turn into unbounded DNS work. The cooldown is applied during AUTH,
	// not on connection open, so a legitimate sender that opens many short
	// connections is not punished for a single failed lookup.
	perIPConnLimit       = 16
	perIPConnWindow      = time.Minute
	perIPConnWindowMax   = 128
	perIPAuthWindow      = time.Minute
	perIPAuthWindowMax   = 256
	perIPAuthConcurrent  = 16
	ipMapCap             = 4096
	ipFailureCooldown    = 5 * time.Second
	maxChallengesPerConn = 16
	dnsJobs              = 32

	// defaultMaxTransactions is the global ceiling on pending transaction
	// groups across every connection, independent of the SMTP edge's own
	// MaxConnections. It is a safety net against unbounded group growth, not a
	// policy knob an operator normally tunes.
	defaultMaxTransactions = 128

	// authProofLifetime is the receiver's ceiling for a freshly proved binding.
	// The dialer must not cache a binding past 5 minutes (mxwire.AuthLifetime).
	authProofLifetime = mxwire.AuthLifetime

	// readDeadline bounds one idle read after the handshake.
	readDeadline = 90 * time.Second
	// writeDeadline bounds one frame write; a stuck peer cannot wedge a session.
	writeDeadline = 10 * time.Second
)

// Config is the standalone receiver configuration. The SMTP edge settings
// (hostname, message/staging/connection bounds, verification toggles, DNS
// resolver and timeouts) live in the embedded mxagent.Config so the receiver
// and the SMTP edge share one operator surface. TXT lookup is the receiver's
// own DNS call for the _mailmoose-mx proof; when nil the system resolver is
// used.
type Config struct {
	// Mode selects single-core bearer authentication or shared DNS authentication.
	Mode    string
	CoreKey string
	SMTP    mxagent.Config

	// LookupTXT resolves the _mailmoose-mx.<domain> TXT records proving domain
	// authority. It is separate from mxagent's resolver because the proof is a
	// receiver concern, not an email-verification one.
	LookupTXT func(context.Context, string) ([]string, error)

	// MaxDomainsPerConnection bounds distinct domains admitted on one session.
	MaxDomainsPerConnection int
	// MaxTransactionsPerConnection bounds concurrent transaction groups on one
	// session; MaxTransactionsPerDomain bounds them per domain across all
	// sessions. Both are enforced below the global mxagent MaxConnections.
	MaxTransactionsPerConnection int
	MaxTransactionsPerDomain     int
	// MaxTransactions bounds concurrent pending groups globally, across all
	// connections. It is deliberately separate from the SMTP edge's
	// MaxConnections so the receiver is bounded on its own terms.
	MaxTransactions int

	// AuthTimeout bounds one DNS proof, ResolveTimeout one recipient resolve and
	// IngestTimeout one staged message handoff.
	AuthTimeout, ResolveTimeout, IngestTimeout time.Duration
	// RevalidateInterval is the nominal renewal period. A binding is renewed
	// when its renewAt deadline passes, which is derived from the grant rather
	// than from a fixed per-connection tick, so a late-authenticated binding is
	// not expired early. Tests may set it below one second.
	RevalidateInterval time.Duration

	// MaxConnsPerIP bounds concurrent sessions from one source IP. Zero selects
	// perIPConnLimit.
	MaxConnsPerIP int
	// ConnWindowMax bounds how many sessions one source IP may open per minute.
	// Zero selects perIPConnWindowMax.
	ConnWindowMax int

	// MaxAuthConcurrent bounds authentication jobs running concurrently for one
	// source IP. Zero selects perIPAuthConcurrent.
	MaxAuthConcurrent int
	// AuthWindowMax bounds how many authentication jobs one source IP may start
	// per minute. Zero selects perIPAuthWindowMax.
	AuthWindowMax int

	// TrustedProxies lists the peers allowed to open a cleartext (non-TLS)
	// session in shared mode. A cleartext session is admitted only from
	// loopback or one of these prefixes; an empty list means every shared
	// session must be TLS. It lets a TLS-terminating reverse proxy front the
	// session listener without the receiver holding a certificate. In single
	// mode the bearer key is the control and cleartext is not peer-gated.
	TrustedProxies []netip.Prefix
	// MaxRenewalsInFlight bounds receiver-driven renewal challenges outstanding
	// on one session at once. Zero selects the effective auth-concurrency limit.
	// Raising it lets tests observe paced renewals without changing production
	// defaults.
	MaxRenewalsInFlight int

	// BrowserRedirectURL, when set, is where an ordinary browser visiting the
	// receiver's root is redirected (302). API and health routes are unchanged;
	// only GET / with an HTML Accept header is served a redirect.
	BrowserRedirectURL string
}

func (c *Config) maxAuthConcurrent() int {
	if c.MaxAuthConcurrent > 0 {
		return c.MaxAuthConcurrent
	}
	return perIPAuthConcurrent
}

func (c *Config) maxConnsPerIP() int {
	if c.MaxConnsPerIP > 0 {
		return c.MaxConnsPerIP
	}
	return perIPConnLimit
}

func (c *Config) connWindowMax() int {
	if c.ConnWindowMax > 0 {
		return c.ConnWindowMax
	}
	return perIPConnWindowMax
}

func (c *Config) authWindowMax() int {
	if c.AuthWindowMax > 0 {
		return c.AuthWindowMax
	}
	return perIPAuthWindowMax
}

func (c *Config) maxRenewalsInFlight() int {
	if c.MaxRenewalsInFlight > 0 {
		return c.MaxRenewalsInFlight
	}
	return c.maxAuthConcurrent()
}

func (c *Config) applyDefaults() {
	if c.Mode == "" {
		c.Mode = "single"
	}
	s := &c.SMTP
	if s.Hostname == "" {
		s.Hostname = "localhost"
	}
	if s.MaxMessageBytes < 1 {
		s.MaxMessageBytes = mxwire.DefaultMaxBodyBytes
	}
	if s.MaxStagingBytes < s.MaxMessageBytes+1 {
		s.MaxStagingBytes = s.MaxMessageBytes + 1
	}
	if s.MaxConnections < 1 {
		s.MaxConnections = 128
	}
	if s.MaxRecipients < 1 {
		s.MaxRecipients = 100
	}
	if s.ReadTimeout <= 0 {
		s.ReadTimeout = 60 * time.Second
	}
	if s.WriteTimeout <= 0 {
		s.WriteTimeout = 60 * time.Second
	}
	if s.DataTimeout <= 0 {
		s.DataTimeout = 5 * time.Minute
	}
	if s.DNSTimeout <= 0 {
		s.DNSTimeout = 10 * time.Second
	}
	if c.MaxDomainsPerConnection < 1 {
		c.MaxDomainsPerConnection = 128
	}
	if c.MaxTransactionsPerConnection < 1 {
		c.MaxTransactionsPerConnection = 16
	}
	if c.MaxTransactionsPerDomain < 1 {
		c.MaxTransactionsPerDomain = 8
	}
	if c.MaxTransactions < 1 {
		c.MaxTransactions = defaultMaxTransactions
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = 10 * time.Second
	}
	if c.ResolveTimeout <= 0 {
		c.ResolveTimeout = 10 * time.Second
	}
	if c.IngestTimeout <= 0 {
		c.IngestTimeout = 3 * time.Minute
	}
	if c.RevalidateInterval <= 0 || c.RevalidateInterval > 4*time.Minute {
		c.RevalidateInterval = 4 * time.Minute
	}
	if c.LookupTXT == nil {
		c.LookupTXT = net.DefaultResolver.LookupTXT
		if s.DNSResolver != "" {
			resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: s.DNSTimeout}).DialContext(ctx, network, s.DNSResolver)
			}}
			c.LookupTXT = resolver.LookupTXT
		}
	}
}

// bindState is the single lifecycle state of a domain binding. It replaces the
// former pair of booleans: a binding is active (the registry's authority),
// replaced (a newer binding owns the registry, but this one may still serve
// pinned DATA until it expires) or revoked (unusable).
type bindState uint8

const (
	bindActive bindState = iota
	bindReplaced
	bindRevoked
)

// Receiver is one logical MX receiver. All mutable state is guarded by mu;
// network writes never happen while mu is held.
type Receiver struct {
	single *connection
	cfg    Config
	log    *slog.Logger

	mu          sync.Mutex
	domains     map[string]*binding
	connections map[string]*connection
	sem         chan struct{}
	dnsSem      chan struct{}
	ips         map[string]*ipState

	// txGlobal and domainTx are the transaction accounting shared by every
	// connection's pending groups.
	txGlobal int
	domainTx map[string]int

	active   atomic.Int64
	stopping atomic.Bool
}

type ipState struct {
	conns int
	// connWindow/connCount bound how many sessions one source may open per
	// minute, so a source cannot cycle short-lived connections to evade the
	// concurrent cap.
	connWindow time.Time
	connCount  int
	// auths is the number of in-flight authentication jobs; authCount is the
	// number started in the current window beginning at authWindow.
	auths      int
	authCount  int
	authWindow time.Time
	cooldown   time.Time
}

// binding is one connection's authority over a domain. registry membership is
// not implied: only the active binding is in Receiver.domains. ContactEmail and
// SetupID are the optional Antler MX registration metadata supplied on
// DomainAuth; they are logged for usage accounting and are never credentials.
type binding struct {
	c             *connection
	channel       uint64
	domain, keyID string
	pub           []byte
	state         bindState
	expires       time.Time
	// renewAt is when maintenance should begin a renewal. It is derived from
	// the grant so a binding that authenticated late is not renewed (or
	// expired) on the connection's fixed tick.
	renewAt      time.Time
	contactEmail string
	setupID      string
}

// challenge is one in-flight challenge. binding is nil for an initial
// authentication and non-nil for a maintenance renewal.
type challenge struct {
	value   mxwire.Challenge
	expires time.Time
	keyID   string
	domain  string
	binding *binding
	// contactEmail/setupID are the optional registration metadata echoed from
	// the DomainAuth that started this challenge.
	contactEmail string
	setupID      string
}

type connection struct {
	id, receiver string
	// transportID is the HTTPS transport connection id assigned at accept time
	// by the transport tracker, when the listener provided one.
	transportID string
	ip          string
	ctx         context.Context
	cancel      context.CancelFunc
	writer      *lockedWriter

	domains     map[uint64]*binding
	challenges  map[uint64]challenge
	lastChannel uint64
	pending     map[uint64]*pending
	txid        atomic.Uint64
	jobs        sync.WaitGroup

	// closing is set before the writer is closed and jobs are waited for, so a
	// racing spawn is rejected instead of adding to a zero WaitGroup.
	closing bool
}

type pending struct {
	tx        uint64
	ch        uint64
	expected  mxwire.FrameType
	result    chan mxwire.Frame
	bindings  map[string]*binding
	domain    string
	domains   []string
	counted   map[string]bool
	released  bool
	cancelled bool
}

type lockedWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	ctl    *http.ResponseController
	closed bool
}

func (w *lockedWriter) write(f mxwire.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return io.ErrClosedPipe
	}
	_ = w.ctl.SetWriteDeadline(time.Now().Add(writeDeadline))
	// Bound this frame's write/flush only. Leaving an HTTP/2 stream deadline
	// armed also terminates an idle session after a successful write.
	defer w.ctl.SetWriteDeadline(time.Time{})
	if e := mxwire.WriteFrame(w.w, f); e != nil {
		return e
	}
	return w.ctl.Flush()
}

func (w *lockedWriter) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

func New(cfg Config, l *slog.Logger) *Receiver {
	if l == nil {
		l = slog.Default()
	}
	cfg.applyDefaults()
	r := &Receiver{
		cfg:         cfg,
		log:         l,
		domains:     map[string]*binding{},
		connections: map[string]*connection{},
		sem:         make(chan struct{}, cfg.SMTP.MaxConnections),
		dnsSem:      make(chan struct{}, dnsJobs),
		ips:         map[string]*ipState{},
		domainTx:    map[string]int{},
	}
	return r
}

// Handler serves the HTTP/2 session endpoint and minimal health.
func (r *Receiver) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST "+mxwire.SessionPath, r.serve)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	// Readiness reflects the receiver process, not domain registration: an MX
	// receiver with no domains registered is still ready to accept sessions.
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if r.stopping.Load() {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(200)
	})
	// A regular browser that visits the receiver's root is sent to the
	// operator's landing page. Only an HTML navigation is redirected; a client
	// that accepts anything (or has no Accept header) gets the plain 404 so
	// health probes and tooling keep their expected behaviour.
	m.HandleFunc("GET /{$}", r.browserRoot)
	return m
}

// browserRoot serves GET / for browsers: a 302 to the configured landing page,
// or 404 when no redirect is configured. The URL is validated at load time, so
// it is never an open redirect target chosen by a request.
func (r *Receiver) browserRoot(w http.ResponseWriter, req *http.Request) {
	if r.cfg.BrowserRedirectURL == "" || !wantsBrowserHTML(req) {
		http.NotFound(w, req)
		return
	}
	http.Redirect(w, req, r.cfg.BrowserRedirectURL, http.StatusFound)
}

// wantsBrowserHTML reports whether the request is a browser navigation:
// text/html is explicitly an acceptable type. It is a positive check, not a
// user-agent guess, so API clients are unaffected.
func wantsBrowserHTML(req *http.Request) bool {
	for _, part := range strings.Split(req.Header.Get("Accept"), ",") {
		media, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(media), "text/html") {
			return true
		}
	}
	return false
}

func (r *Receiver) serve(w http.ResponseWriter, q *http.Request) {
	ip, _, _ := net.SplitHostPort(q.RemoteAddr)
	peer := ip
	if r.log != nil {
		if host, _, err := net.SplitHostPort(q.RemoteAddr); err == nil {
			peer = host
		}
	}
	if q.ProtoMajor != 2 {
		r.logRejected(peer, "not_h2")
		http.Error(w, "HTTP/2 required", 426)
		return
	}
	// Shared mode requires TLS unless the immediate peer is a trusted local
	// proxy (or loopback): the receiver then holds no certificate and a
	// TLS-terminating proxy fronts the listener. The bearer key stays the
	// control in single mode, where cleartext is accepted for loopback.
	cleartext := q.TLS == nil
	cleartextTrusted := false
	if cleartext && r.cfg.Mode == "shared" {
		if !r.cleartextPeerAllowed(ip) {
			r.logRejected(peer, "cleartext_not_trusted")
			http.Error(w, "TLS HTTP/2 required", 426)
			return
		}
		cleartextTrusted = true
	}
	if r.cfg.Mode == "single" && !r.authenticateCore(q) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.stopping.Load() {
		r.logRejected(peer, "stopping")
		http.Error(w, "stopping", 503)
		return
	}
	r.mu.Lock()
	st := r.ips[ip]
	if st == nil {
		if len(r.ips) >= ipMapCap {
			for key, entry := range r.ips {
				if entry.conns == 0 && entry.auths == 0 && time.Now().After(entry.cooldown) && time.Since(entry.authWindow) >= perIPAuthWindow {
					delete(r.ips, key)
				}
			}
			if len(r.ips) >= ipMapCap {
				r.mu.Unlock()
				r.logRejected(peer, "ip_map_full")
				http.Error(w, "limited", 429)
				return
			}
		}
		st = &ipState{}
		r.ips[ip] = st
	}
	// The failure cooldown is enforced during AUTH, not on connection open, so
	// a source is not punished for opening a short connection. The concurrent
	// cap and the per-minute connection window both bound connection churn.
	now := time.Now()
	if now.Sub(st.connWindow) >= perIPConnWindow {
		st.connWindow = now
		st.connCount = 0
	}
	if st.conns >= r.cfg.maxConnsPerIP() || st.connCount >= r.cfg.connWindowMax() {
		r.mu.Unlock()
		r.logRejected(peer, "ip_limit")
		http.Error(w, "limited", 429)
		return
	}
	st.conns++
	st.connCount++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		st.conns--
		if st.conns == 0 && st.auths == 0 && time.Now().After(st.cooldown) && time.Since(st.authWindow) >= perIPAuthWindow {
			delete(r.ips, ip)
		}
		r.mu.Unlock()
	}()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	default:
		r.logRejected(peer, "connection_limit")
		http.Error(w, "limited", 503)
		return
	}
	ctl := http.NewResponseController(w)
	if ctl.EnableFullDuplex() != nil {
		r.logRejected(peer, "full_duplex_unavailable")
		http.Error(w, "full duplex unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	if ctl.Flush() != nil {
		return
	}
	ctx, cancel := context.WithCancel(q.Context())
	defer cancel()
	var rid, cid [16]byte
	if _, err := rand.Read(rid[:]); err != nil {
		return
	}
	if _, err := rand.Read(cid[:]); err != nil {
		return
	}
	c := &connection{
		id: hex.EncodeToString(cid[:]), receiver: hex.EncodeToString(rid[:]), ip: ip,
		ctx: ctx, cancel: cancel, writer: &lockedWriter{w: w, ctl: ctl},
		domains: map[uint64]*binding{}, challenges: map[uint64]challenge{}, pending: map[uint64]*pending{},
	}
	if st, ok := transportFromContext(q.Context()); ok {
		c.transportID = st.id
	}
	r.mu.Lock()
	r.connections[c.id] = c
	r.mu.Unlock()
	r.active.Add(1)
	started := time.Now()
	var tlsVersion, tlsCipher uint16
	if q.TLS != nil {
		tlsVersion, tlsCipher = q.TLS.Version, q.TLS.CipherSuite
	}
	r.logSession(c, "opened", "active_connections", r.active.Load(), "cleartext", cleartext, "cleartext_trusted", cleartextTrusted, "tls_version", tlsVersion, "tls_cipher", tlsCipher, "protocol", q.Proto, "peer_address", q.RemoteAddr)
	var closeReason string
	defer func() {
		cancel()
		// Stop accepting new jobs, close the writer so no job can write after
		// the handler returns, then wait for jobs to drain and release state.
		r.mu.Lock()
		c.closing = true
		if r.single == c {
			r.single = nil
		}
		r.mu.Unlock()
		c.writer.close()
		c.jobs.Wait()
		r.mu.Lock()
		for ch, b := range c.domains {
			b.state = bindRevoked
			if r.domains[b.domain] == b {
				delete(r.domains, b.domain)
			}
			delete(c.domains, ch)
		}
		delete(r.connections, c.id)
		pending := make([]*pending, 0, len(c.pending))
		for _, p := range c.pending {
			pending = append(pending, p)
		}
		r.mu.Unlock()
		for _, p := range pending {
			r.release(c, p)
		}
		r.active.Add(-1)
		r.logSession(c, "closed",
			"duration_ms", time.Since(started).Milliseconds(),
			"reason", closeReason,
			"active_connections", r.active.Load(),
		)
	}()
	if !r.spawn(c, func() { r.maintenance(c) }) {
		closeReason = "spawn_failed"
		return
	}
	// When the connection context is cancelled (maintenance saw an external
	// write failure, or the handler is winding down), force any blocked read to
	// return immediately instead of waiting out the 90s read deadline. The
	// handler waits for the watcher to exit before returning, so it never
	// touches the response controller after the handler is done.
	stopWatch := make(chan struct{})
	watchExited := make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-c.ctx.Done():
			_ = ctl.SetReadDeadline(time.Now())
		case <-stopWatch:
		}
	}()
	defer func() { close(stopWatch); <-watchExited }()

	// The handshake read is bounded by AuthTimeout; the session read loop then
	// uses a per-read deadline and honours context cancellation so a dropped
	// client cannot wedge a read.
	_ = ctl.SetReadDeadline(time.Now().Add(r.cfg.AuthTimeout))
	f, e := mxwire.ReadFrame(q.Body)
	if e != nil || f.Type != mxwire.FrameHello {
		closeReason = "handshake"
		return
	}
	var h mxwire.Hello
	if mxwire.DecodeFrame(f, &h) != nil || h.Version != mxwire.V2Protocol {
		closeReason = "protocol_mismatch"
		return
	}
	// core_label is the dialer's self-declared instance label. It is a display
	// string ("gatehouse" for every core) and is deliberately NOT unique:
	// correlation keys are connection_id and receiver_id, never this label.
	r.logSession(c, "hello", "version", h.Version, "core_label", h.Instance)
	r.mu.Lock()
	if r.cfg.Mode == "single" {
		r.single = c
	}
	r.mu.Unlock()
	_ = r.send(c, mxwire.FrameReady, 0, 0, mxwire.Ready{
		Mode:              r.cfg.Mode,
		Version:           mxwire.V2Protocol,
		ReceiverID:        c.receiver,
		ConnectionID:      c.id,
		SMTPHostname:      r.cfg.SMTP.Hostname,
		MaxMessageBytes:   r.cfg.SMTP.MaxMessageBytes,
		MaxDomains:        r.cfg.MaxDomainsPerConnection,
		MaxAuthInflight:   r.cfg.maxAuthConcurrent(),
		RevalidateSeconds: int(r.cfg.RevalidateInterval / time.Second),
	})
	for {
		if c.ctx.Err() != nil {
			closeReason = "canceled"
			return
		}
		_ = ctl.SetReadDeadline(time.Now().Add(readDeadline))
		f, e = mxwire.ReadFrame(q.Body)
		if e != nil {
			if closeReason == "" {
				closeReason = boundedReason(e)
			}
			return
		}
		if r.handle(c, f) != nil {
			closeReason = "protocol_error"
			return
		}
	}
}

// logSession emits a session lifecycle event (opened/hello/closed) with the
// receiver and connection identity. It is safe to log: the event carries no
// protocol bytes, only ids, bounded facts and a duration.
func (r *Receiver) logSession(c *connection, stage string, extra ...any) {
	if r.log == nil {
		return
	}
	event := ""
	switch stage {
	case "opened":
		event = eventSessionOpened
	case "hello":
		event = eventSessionHello
	case "closed":
		event = eventSessionClosed
	default:
		return
	}
	args := []any{"stage", stage}
	if c != nil {
		args = append(args, "core_connection_id", c.id, "receiver_id", c.receiver)
		if c.transportID != "" {
			args = append(args, "transport_id", c.transportID)
		}
		args = append(args, "peer", c.ip)
	}
	args = append(args, extra...)
	r.log.Info(event, args...)
}

// logProof records one step of the domain-ownership proof. phase is a bounded
// token; the record never carries a raw TXT record, signature or key material.
func (r *Receiver) logProof(c *connection, phase, domain, keyID, result, reason string, d time.Duration, extra ...any) {
	if r.log == nil {
		return
	}
	args := []any{
		"phase", phase,
		"domain", domain,
		"key_id", keyID,
		"result", result,
	}
	if c != nil {
		args = append(args, "core_connection_id", c.id, "receiver_id", c.receiver)
		if c.transportID != "" {
			args = append(args, "transport_id", c.transportID)
		}
	}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	if phase == "dns_lookup" {
		resolver := r.cfg.SMTP.DNSResolver
		if resolver == "" {
			resolver = "system"
		}
		args = append(args, "query_name", "_mailmoose-mx."+domain, "resolver", resolver)
	}
	if d > 0 {
		args = append(args, "duration_ms", d.Milliseconds())
	}
	args = append(args, extra...)
	r.log.Info(eventDomainProof, args...)
}

// logRejected records a session that was refused before it was ever admitted.
// The reason is a bounded classifier; no request content is logged. It is
// emitted before any core connection exists, so it carries no core id.
func (r *Receiver) logRejected(peer, reason string) {
	if r.log == nil {
		return
	}
	args := []any{"reason", reason}
	if peer != "" {
		args = append(args, "peer", peer)
	}
	r.log.Info(eventSessionRejected, args...)
}

func (r *Receiver) send(c *connection, t mxwire.FrameType, tx, ch uint64, v any) error {
	f, e := mxwire.JSONFrame(t, tx, ch, v)
	if e != nil {
		return e
	}
	return c.writer.write(f)
}

// spawn runs a bounded background job tied to the connection lifetime. It
// refuses to add once the connection is closing, so jobs.Wait can never be
// raced by an Add after it returns.
func (r *Receiver) spawn(c *connection, fn func()) bool {
	r.mu.Lock()
	if c.closing {
		r.mu.Unlock()
		return false
	}
	c.jobs.Add(1)
	r.mu.Unlock()
	go func() {
		defer c.jobs.Done()
		fn()
	}()
	return true
}

// maintenance drives keepalive pings, challenge expiry and binding renewals on
// one 1-second tick plus a ping counter. The tick is min(1s, RevalidateInterval)
// so tests can drive renewals quickly without a fixed per-connection revalidate
// ticker that misfires under late authentication.
func (r *Receiver) maintenance(c *connection) {
	defer c.cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	if r.cfg.RevalidateInterval < time.Second {
		tick.Reset(r.cfg.RevalidateInterval)
	}
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
			r.expireConn(c)
			r.revalidateConn(c)
		case <-ping.C:
			if c.writer.write(mxwire.Frame{Type: mxwire.FramePing}) != nil {
				// A failed keepalive means the peer is gone: cancel so the read
				// loop is released and both halves of the session stop together.
				return
			}
		}
	}
}

func (r *Receiver) handle(c *connection, f mxwire.Frame) error {
	if r.cfg.Mode == "single" && (f.Type == mxwire.FrameDomainAuth || f.Type == mxwire.FrameChallengeResponse || f.Type == mxwire.FrameDomainUnregister) {
		return errors.New("domain authentication unavailable in single mode")
	}
	switch f.Type {
	case mxwire.FramePong:
		return nil
	case mxwire.FramePing:
		return r.send(c, mxwire.FramePong, 0, 0, struct{}{})
	case mxwire.FrameDomainAuth:
		return r.auth(c, f)
	case mxwire.FrameChallengeResponse:
		return r.proof(c, f)
	case mxwire.FrameDomainUnregister:
		return r.unregister(c, f)
	case mxwire.FrameResolveResult, mxwire.FrameIngestResult:
		return r.dispatch(c, f)
	default:
		return errors.New("unexpected frame")
	}
}

func (r *Receiver) authenticateCore(q *http.Request) bool {
	if r.cfg.CoreKey == "" {
		return false
	}
	want := sha256.Sum256([]byte("Bearer " + r.cfg.CoreKey))
	got := sha256.Sum256([]byte(q.Header.Get("Authorization")))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// cleartextPeerAllowed reports whether a non-TLS session may be admitted from
// the given peer IP. Loopback is always allowed (the included receiver dials
// 127.0.0.1); other peers must be in the configured trusted-proxy allowlist.
// With no allowlist the result is false, so shared mode keeps requiring TLS.
func (r *Receiver) cleartextPeerAllowed(ip string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, p := range r.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// auth starts an initial authentication. The DNS check runs asynchronously, so
// the read loop never blocks on resolution.
func (r *Receiver) auth(c *connection, f mxwire.Frame) error {
	var a mxwire.DomainAuth
	if mxwire.DecodeFrame(f, &a) != nil {
		return errors.New("invalid auth")
	}
	d, e := mxwire.CanonicalDomain(a.Domain)
	if e != nil || d != a.Domain || a.KeyID == "" {
		return errors.New("invalid domain")
	}
	// Registration metadata is optional and validated when present. A malformed
	// value is dropped rather than rejecting the proof: it is accounting
	// metadata, not authority.
	contactEmail, setupID := "", ""
	if a.ContactEmail != "" || a.SetupID != "" {
		if mxwire.ValidContactEmail(a.ContactEmail) && mxwire.ValidSetupID(a.SetupID) {
			contactEmail, setupID = a.ContactEmail, a.SetupID
		} else {
			r.logProof(c, "registration_metadata", d, a.KeyID, "rejected", "invalid_metadata", 0)
		}
	}
	now := time.Now()
	r.mu.Lock()
	if f.ChannelID <= c.lastChannel {
		r.mu.Unlock()
		return errors.New("domain channel reused")
	}
	c.lastChannel = f.ChannelID
	if b, used := c.domains[f.ChannelID]; used {
		// A channel already assigned to this connection must not be
		// re-authenticated. A replaced binding is parked for pinned DATA
		// until it expires and must not be reclaimed on a new channel either.
		reason := "channel_assigned"
		if b != nil && b.state == bindReplaced {
			reason = "replaced"
		}
		r.mu.Unlock()
		return r.authReply(c, f.ChannelID, d, a.KeyID, false, reason, time.Time{})
	}
	if _, ok := c.challenges[f.ChannelID]; ok {
		r.mu.Unlock()
		return errors.New("pending auth")
	}
	if len(c.domains)+len(c.challenges) >= r.cfg.MaxDomainsPerConnection || len(c.challenges) >= maxChallengesPerConn {
		r.mu.Unlock()
		return r.authReply(c, f.ChannelID, d, a.KeyID, false, "domain_limit", time.Time{})
	}
	// A binding this connection already holds (and lost to another connection)
	// must not be reclaimed by a fresh initial auth on a new channel; old
	// pinned DATA is still served until the replaced binding expires.
	for _, b := range c.domains {
		if b.domain == d && b.state == bindReplaced {
			r.mu.Unlock()
			return r.authReply(c, f.ChannelID, d, a.KeyID, false, "replaced", time.Time{})
		}
	}
	c.challenges[f.ChannelID] = challenge{keyID: a.KeyID, domain: d, expires: now.Add(challengeTTL), contactEmail: contactEmail, setupID: setupID}
	r.mu.Unlock()
	if !r.spawn(c, func() { r.initialAuth(c, f.ChannelID, d, a.KeyID) }) {
		r.dropChallenge(c, f.ChannelID)
		return nil
	}
	return nil
}

func (r *Receiver) initialAuth(c *connection, ch uint64, d, keyID string) {
	started := time.Now()
	if !r.acquireAuth(c) {
		r.dropChallenge(c, ch)
		r.logProof(c, "start", d, keyID, "rejected", "source_limit", time.Since(started))
		_ = r.authReply(c, ch, d, keyID, false, "source_limit", time.Time{})
		return
	}
	defer r.releaseAuth(c)
	ctx, cancel := context.WithTimeout(c.ctx, r.cfg.AuthTimeout)
	defer cancel()
	txt, err := r.lookupTXT(ctx, d)
	r.logProof(c, "dns_lookup", d, keyID, resultWord(err == nil), boundedReason(err), time.Since(started))
	if err != nil {
		r.dropChallenge(c, ch)
		r.failIP(c)
		_ = r.authReply(c, ch, d, keyID, false, "dns_unavailable", time.Time{})
		return
	}
	_, err = mxwire.ParseDomainTXT(txt, keyID)
	r.logProof(c, "parse_key", d, keyID, resultWord(err == nil), boundedReason(err), time.Since(started))
	if err != nil {
		r.dropChallenge(c, ch)
		r.failIP(c)
		_ = r.authReply(c, ch, d, keyID, false, "key_unavailable", time.Time{})
		return
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		r.dropChallenge(c, ch)
		return
	}
	issued := mxwire.Challenge{Domain: d, KeyID: keyID, ReceiverID: c.receiver, ConnectionID: c.id, Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
	r.mu.Lock()
	ent, ok := c.challenges[ch]
	if !ok || ent.keyID != keyID || ent.binding != nil || !time.Now().Before(ent.expires) {
		r.mu.Unlock()
		return
	}
	ent.value = issued
	c.challenges[ch] = ent
	r.mu.Unlock()
	_ = r.send(c, mxwire.FrameChallenge, 0, ch, issued)
}

func (r *Receiver) dropChallenge(c *connection, ch uint64) {
	r.mu.Lock()
	delete(c.challenges, ch)
	r.mu.Unlock()
}

func (r *Receiver) authReply(c *connection, ch uint64, d, k string, ok bool, reason string, exp time.Time) error {
	r.logProof(c, "grant", d, k, resultWord(ok), reason, 0, "domain_channel_id", ch, "accepted", ok, "expires_at", exp)
	return r.send(c, mxwire.FrameAuthResult, 0, ch, mxwire.AuthResult{Domain: d, KeyID: k, Accepted: ok, Reason: reason, ExpiresAt: exp})
}

// proof consumes a challenge response and verifies the signature with fresh DNS,
// asynchronously, so the read loop never blocks on resolution.
func (r *Receiver) proof(c *connection, f mxwire.Frame) error {
	var x mxwire.ChallengeResponse
	if mxwire.DecodeFrame(f, &x) != nil {
		return errors.New("invalid proof")
	}
	now := time.Now()
	r.mu.Lock()
	issued, ok := c.challenges[f.ChannelID]
	if ok {
		delete(c.challenges, f.ChannelID)
	}
	r.mu.Unlock()
	if !ok || !now.Before(issued.expires) || issued.value.Nonce == "" || issued.domain != x.Domain || issued.keyID != x.KeyID || issued.value.Nonce != x.Nonce {
		r.failIP(c)
		return r.authReply(c, f.ChannelID, x.Domain, x.KeyID, false, "challenge_expired", time.Time{})
	}
	if !r.spawn(c, func() { r.verifyProof(c, f.ChannelID, issued, x) }) {
		return nil
	}
	return nil
}

func (r *Receiver) verifyProof(c *connection, ch uint64, issued challenge, x mxwire.ChallengeResponse) {
	started := time.Now()
	if !r.acquireAuth(c) {
		if issued.binding != nil {
			// A renewal that could not start because the source is at its
			// temporary authentication capacity is NOT a failed proof: the
			// existing grant is still valid. Leave the binding untouched and let
			// revalidateConn re-issue the renewal on a later tick, before the
			// grant expires. Only the grant's own expiry fails the binding.
			r.logProof(c, "renewal", x.Domain, x.KeyID, "deferred", "source_limit", time.Since(started))
			return
		}
		r.logProof(c, "start", x.Domain, x.KeyID, "rejected", "source_limit", time.Since(started))
		_ = r.authReply(c, ch, x.Domain, x.KeyID, false, "source_limit", time.Time{})
		return
	}
	defer r.releaseAuth(c)
	ctx, cancel := context.WithTimeout(c.ctx, r.cfg.AuthTimeout)
	txt, e := r.lookupTXT(ctx, x.Domain)
	cancel()
	r.logProof(c, "dns_lookup", x.Domain, x.KeyID, resultWord(e == nil), boundedReason(e), time.Since(started))
	pub, e2 := mxwire.ParseDomainTXT(txt, x.KeyID)
	r.logProof(c, "parse_key", x.Domain, x.KeyID, resultWord(e2 == nil), boundedReason(e2), time.Since(started))
	sigOK := e == nil && e2 == nil && mxwire.VerifyChallenge(pub, issued.value, x.Signature)
	r.logProof(c, "signature", x.Domain, x.KeyID, resultWord(sigOK), "", time.Since(started))
	if e != nil || e2 != nil || !sigOK {
		r.failIP(c)
		if issued.binding != nil {
			// A failed renewal invalidates the domain fail-closed: the proof
			// could not be completed against fresh DNS.
			r.revoke(issued.binding, "reauth_failed")
		}
		_ = r.authReply(c, ch, x.Domain, x.KeyID, false, "proof_invalid", time.Time{})
		return
	}
	now := time.Now()
	if !now.Before(issued.expires) {
		if issued.binding != nil {
			r.revoke(issued.binding, "reauth_failed")
		}
		r.logProof(c, "challenge", x.Domain, x.KeyID, "rejected", "challenge_expired", time.Since(started))
		_ = r.authReply(c, ch, x.Domain, x.KeyID, false, "challenge_expired", time.Time{})
		return
	}
	if issued.binding != nil {
		// Renewal: only the exact binding identity may be extended. The grant
		// is capped at AuthLifetime from now (never extended past a cached
		// ceiling) and the new renewAt is derived from the fresh grant.
		r.mu.Lock()
		b := issued.binding
		if c.closing || b.state != bindActive || b.c != c || b.channel != ch || c.domains[ch] != b || r.domains[b.domain] != b {
			r.mu.Unlock()
			r.logProof(c, "renewal", x.Domain, x.KeyID, "rejected", "superseded", time.Since(started))
			_ = r.authReply(c, ch, x.Domain, x.KeyID, false, "superseded", time.Time{})
			return
		}
		b.pub = pub
		b.expires = now.Add(authProofLifetime)
		// Renew on the configured cadence measured from the fresh grant, so a
		// renewal always starts before expiry even when auth completed late.
		b.renewAt = now.Add(r.cfg.RevalidateInterval)
		exp := b.expires
		expiresIn := time.Until(exp)
		email, setupID := b.contactEmail, b.setupID
		r.mu.Unlock()
		r.logProof(c, "renewal", x.Domain, x.KeyID, "renewed", "", time.Since(started), "expires_in_ms", expiresIn.Milliseconds(), "contact_email", email, "setup_id", setupID)
		_ = r.authReply(c, ch, x.Domain, x.KeyID, true, "", exp)
		return
	}
	// Initial authentication: install a new binding, superseding any other.
	r.mu.Lock()
	if c.closing {
		r.mu.Unlock()
		r.logProof(c, "registration", x.Domain, x.KeyID, "rejected", "closing", time.Since(started))
		return
	}
	if len(c.domains) >= r.cfg.MaxDomainsPerConnection && c.domains[ch] == nil {
		r.mu.Unlock()
		r.logProof(c, "registration", x.Domain, x.KeyID, "rejected", "domain_limit", time.Since(started))
		_ = r.authReply(c, ch, x.Domain, x.KeyID, false, "domain_limit", time.Time{})
		return
	}
	var replaced *binding
	if old := r.domains[x.Domain]; old != nil && !(old.c == c && old.channel == ch) {
		// Retain the old binding on its own connection as "replaced" so its
		// already-pinned DATA stays valid until it expires. It is removed from
		// the registry and this connection must not reclaim it.
		old.state = bindReplaced
		replaced = old
	}
	b := &binding{c: c, channel: ch, domain: x.Domain, keyID: x.KeyID, state: bindActive, expires: now.Add(authProofLifetime), renewAt: now.Add(r.cfg.RevalidateInterval), pub: pub, contactEmail: issued.contactEmail, setupID: issued.setupID}
	c.domains[ch] = b
	r.domains[x.Domain] = b
	r.mu.Unlock()
	r.logProof(c, "registration", x.Domain, x.KeyID, "active", "", time.Since(started), "contact_email", b.contactEmail, "setup_id", b.setupID)
	if replaced != nil && replaced.c != c {
		sc := replaced.c
		sdomain := replaced.domain
		sch := replaced.channel
		oldKeyID := replaced.keyID
		newKeyID := b.keyID
		r.logProof(replaced.c, "replacement", sdomain, oldKeyID, "replaced", "", 0,
			"old_key_id", oldKeyID, "new_key_id", newKeyID, "old_channel", sch, "new_channel", ch, "old_core_connection_id", replaced.c.id, "new_core_connection_id", c.id)
		r.spawn(c, func() {
			_ = r.send(sc, mxwire.FrameDomainRevoked, 0, sch, mxwire.DomainNotice{Domain: sdomain, Reason: "replaced"})
		})
	}
	_ = r.authReply(c, ch, x.Domain, x.KeyID, true, "", b.expires)
}

func (r *Receiver) unregister(c *connection, f mxwire.Frame) error {
	var n mxwire.DomainNotice
	if mxwire.DecodeFrame(f, &n) != nil {
		return errors.New("invalid unregister")
	}
	r.mu.Lock()
	b := c.domains[f.ChannelID]
	delete(c.domains, f.ChannelID)
	if b != nil {
		b.state = bindRevoked
		// Pointer compare: a replacement's newer binding must never be removed
		// by the old connection.
		if r.domains[b.domain] == b {
			delete(r.domains, b.domain)
		}
	}
	var pending []*pending
	for _, p := range c.pending {
		if b != nil && p.bindings[b.domain] == b {
			pending = append(pending, p)
		}
	}
	var domain, keyID string
	if b != nil {
		domain, keyID = b.domain, b.keyID
	}
	r.mu.Unlock()
	if b != nil {
		r.logProof(c, "revocation", domain, keyID, "revoked", "unregistered", 0)
	}
	for _, p := range pending {
		r.release(c, p)
	}
	return nil
}

func (r *Receiver) dispatch(c *connection, f mxwire.Frame) error {
	r.mu.Lock()
	p := c.pending[f.TxID]
	if p == nil || p.cancelled {
		r.mu.Unlock()
		return nil
	}
	expected, ch := p.expected, p.ch
	r.mu.Unlock()
	if expected != f.Type || (expected == mxwire.FrameResolveResult && ch != f.ChannelID) || (expected == mxwire.FrameIngestResult && f.ChannelID != 0) {
		return errors.New("mismatched result")
	}
	select {
	case p.result <- f:
		return nil
	default:
		return errors.New("duplicate result")
	}
}

// expireConn drops expired challenges and revokes bindings whose renewal never
// completed or whose lifetime lapsed. Replaced bindings are dropped silently
// once they expire: the connection was already told the domain was replaced.
func (r *Receiver) expireConn(c *connection) {
	now := time.Now()
	type revokeItem struct {
		b   *binding
		why string
	}
	var itemReasons []revokeItem
	r.mu.Lock()
	for ch, chl := range c.challenges {
		if now.Before(chl.expires) {
			continue
		}
		delete(c.challenges, ch)
		if b := chl.binding; b != nil && b.state == bindActive && c.domains[ch] == b && r.domains[b.domain] == b {
			itemReasons = append(itemReasons, revokeItem{b, "reauth_failed"})
		}
	}
	for _, b := range c.domains {
		if b.state == bindRevoked || now.Before(b.expires) {
			continue
		}
		itemReasons = append(itemReasons, revokeItem{b, "auth_expired"})
	}
	r.mu.Unlock()
	for _, it := range itemReasons {
		r.revoke(it.b, it.why)
	}
}

// revalidateConn starts renewal challenges for bindings whose renewAt deadline
// has passed. Renewals are paced: only enough are issued to keep at most
// maxRenewalsInFlight outstanding on the connection, in deterministic domain
// order. A renewal that cannot start (source capacity) is simply retried on a
// later tick while its grant is still valid; it is never revoked early.
func (r *Receiver) revalidateConn(c *connection) {
	now := time.Now()
	type due struct {
		channel uint64
		binding *binding
	}
	limit := r.cfg.maxRenewalsInFlight()
	var ready []due
	r.mu.Lock()
	inflight := 0
	for _, chl := range c.challenges {
		if chl.binding != nil {
			inflight++
		}
	}
	for ch, b := range c.domains {
		if b.state != bindActive || r.domains[b.domain] != b || now.Before(b.renewAt) {
			continue
		}
		if _, ok := c.challenges[ch]; ok {
			continue
		}
		ready = append(ready, due{channel: ch, binding: b})
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].binding.domain != ready[j].binding.domain {
			return ready[i].binding.domain < ready[j].binding.domain
		}
		return ready[i].channel < ready[j].channel
	})
	type renewal struct {
		channel uint64
		value   mxwire.Challenge
	}
	var issued []renewal
	for _, item := range ready {
		if inflight >= limit {
			break
		}
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			continue
		}
		challengeValue := mxwire.Challenge{Domain: item.binding.domain, KeyID: item.binding.keyID, ReceiverID: c.receiver, ConnectionID: c.id, Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
		c.challenges[item.channel] = challenge{keyID: item.binding.keyID, domain: item.binding.domain, expires: now.Add(challengeTTL), binding: item.binding, value: challengeValue}
		issued = append(issued, renewal{item.channel, challengeValue})
		inflight++
	}
	r.mu.Unlock()
	for _, item := range issued {
		_ = r.send(c, mxwire.FrameChallenge, 0, item.channel, item.value)
	}
}

// revoke marks a binding unusable. It sends a revocation notice only when the
// binding was the registry authority; a replaced binding is retired silently.
func (r *Receiver) revoke(b *binding, why string) {
	r.mu.Lock()
	if b.state == bindRevoked {
		r.mu.Unlock()
		return
	}
	active := r.domains[b.domain] == b && b.state == bindActive
	b.state = bindRevoked
	delete(b.c.domains, b.channel)
	if active {
		delete(r.domains, b.domain)
	}
	var wake []*pending
	for _, p := range b.c.pending {
		if p.bindings[b.domain] == b {
			wake = append(wake, p)
		}
	}
	c := b.c
	ch := b.channel
	domain := b.domain
	keyID := b.keyID
	wasActive := active
	r.mu.Unlock()
	if wasActive {
		_ = r.send(c, mxwire.FrameDomainRevoked, 0, ch, mxwire.DomainNotice{Domain: domain, Reason: why})
	}
	r.logProof(c, "revocation", domain, keyID, "revoked", why, 0, "active", wasActive)
	for _, p := range wake {
		r.release(c, p)
	}
}

// lookup returns the active registry binding for a domain.
func (r *Receiver) lookup(d string) (*binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.Mode == "single" {
		if r.single == nil || r.single.closing || r.single.ctx.Err() != nil {
			return nil, errors.New("unavailable")
		}
		return &binding{c: r.single, domain: d, channel: 1, state: bindActive}, nil
	}
	b := r.domains[d]
	if b == nil || b.state != bindActive || !time.Now().Before(b.expires) {
		return nil, errors.New("unavailable")
	}
	return b, nil
}

func (r *Receiver) nextTx(c *connection) uint64 { return c.txid.Add(1) }

// slot creates one pending transaction group, enforcing the global, per-domain
// and per-connection transaction caps.
func (r *Receiver) slot(c *connection, id uint64, d string) (*pending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.txGlobal >= r.cfg.MaxTransactions { // bounded global group count
		return nil, errors.New("global transaction limit")
	}
	if len(c.pending) >= r.cfg.MaxTransactionsPerConnection {
		return nil, errors.New("connection transaction limit")
	}
	if r.domainTx[d] >= r.cfg.MaxTransactionsPerDomain {
		return nil, errors.New("domain transaction limit")
	}
	p := &pending{
		tx: id, domain: d, result: make(chan mxwire.Frame, 1),
		bindings: map[string]*binding{}, counted: map[string]bool{d: true},
	}
	c.pending[id] = p
	r.txGlobal++
	r.domainTx[d]++
	return p, nil
}

// countDomain accounts one additional domain on an existing pending group, once.
func (r *Receiver) countDomain(p *pending, d string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.counted[d] {
		return nil
	}
	if r.domainTx[d] >= r.cfg.MaxTransactionsPerDomain {
		return errors.New("domain transaction limit")
	}
	p.counted[d] = true
	r.domainTx[d]++
	return nil
}

// release frees a pending group exactly once and wakes any waiter. The per-domain
// counters are only released here, never on a denied RCPT, so a later accepted
// recipient on the same connection cannot under-count (and a group is kept until
// its transaction closes).
func (r *Receiver) release(c *connection, p *pending) {
	r.mu.Lock()
	if p.released {
		r.mu.Unlock()
		return
	}
	p.released = true
	p.cancelled = true
	if c.pending[p.tx] == p {
		delete(c.pending, p.tx)
	}
	if r.txGlobal > 0 {
		r.txGlobal--
	}
	for d := range p.counted {
		if r.domainTx[d] > 0 {
			r.domainTx[d]--
			if r.domainTx[d] == 0 {
				delete(r.domainTx, d)
			}
		}
	}
	select {
	case p.result <- mxwire.Frame{}:
	default:
	}
	r.mu.Unlock()
}

func (r *Receiver) failIP(c *connection) {
	now := time.Now()
	r.mu.Lock()
	if st := r.ips[c.ip]; st != nil {
		st.cooldown = now.Add(ipFailureCooldown)
	}
	r.mu.Unlock()
}

// acquireAuth bounds concurrent authentication jobs per source IP and enforces
// the per-minute auth window. A source that exceeded its window is refused
// rather than allowed to grow unbounded DNS work.
func (r *Receiver) acquireAuth(c *connection) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.ips[c.ip]
	if st == nil {
		return false
	}
	if now.Before(st.cooldown) {
		return false
	}
	if now.Sub(st.authWindow) >= perIPAuthWindow {
		st.authWindow = now
		st.authCount = 0
	}
	if st.authCount >= r.cfg.authWindowMax() || st.auths >= r.cfg.maxAuthConcurrent() {
		return false
	}
	st.auths++
	st.authCount++
	return true
}

func (r *Receiver) releaseAuth(c *connection) {
	r.mu.Lock()
	if st := r.ips[c.ip]; st != nil && st.auths > 0 {
		st.auths--
	}
	r.mu.Unlock()
}

// lookupTXT resolves with a bounded global DNS worker pool.
func (r *Receiver) lookupTXT(ctx context.Context, d string) ([]string, error) {
	select {
	case r.dnsSem <- struct{}{}:
		defer func() { <-r.dnsSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.cfg.LookupTXT(ctx, "_mailmoose-mx."+d)
}

// Stop marks the receiver stopping so readiness fails and new sessions are
// refused. It does not tear down existing sessions; the caller shuts down the
// HTTP server.
func (r *Receiver) Stop() {
	r.stopping.Store(true)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.connections {
		c.cancel()
	}
}

func (r *Receiver) Stats() map[string]int64 {
	return map[string]int64{"active_connections": r.active.Load()}
}

// NewDelivery starts one SMTP transaction's delivery, grouping the recipients
// it resolves across one or more receiver connections.
func (r *Receiver) NewDelivery() mxagent.Delivery { return newTransaction(r) }
