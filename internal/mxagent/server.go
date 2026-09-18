package mxagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// Server is the policy-free SMTP edge. It enforces connection/recipient/size
// bounds and strict framing, streams the original message to bounded staging,
// computes auth evidence and issues one signed ingest for the whole accepted
// recipient set. The core fans out internally. It never returns SMTP success
// until the core has durably handled every accepted recipient.
type Server struct {
	cfg    Config
	core   *CoreClient
	verify *Verifier
	log    *slog.Logger

	sem      chan struct{}
	staging  *byteBudget
	iplim    *ipLimiter
	active   int64
	accepted int64
	spam     int64
	dup      int64
	reject   int64
	authTemp int64
}

func NewServer(cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	stagingBytes := cfg.MaxStagingBytes
	if stagingBytes < cfg.MaxMessageBytes {
		stagingBytes = cfg.MaxMessageBytes
	}
	return &Server{
		cfg: cfg, core: NewCoreClient(cfg), verify: NewVerifier(cfg), log: log,
		sem:     make(chan struct{}, cfg.MaxConnections),
		staging: newByteBudget(stagingBytes),
		iplim:   newIPLimiter(defaultMaxPerIP),
	}
}

// defaultMaxPerIP bounds concurrent connections from one source IP. The global
// cap (MX_MAX_CONNECTIONS, default 256) bounds the total; the per-IP cap keeps
// one abusive sender from consuming all of it.
const defaultMaxPerIP = 16

// ipLimiter bounds concurrent sessions per source IP.
type ipLimiter struct {
	mu    sync.Mutex
	limit int
	n     map[string]int
}

func newIPLimiter(limit int) *ipLimiter {
	if limit <= 0 {
		limit = defaultMaxPerIP
	}
	return &ipLimiter{limit: limit, n: map[string]int{}}
}

func (l *ipLimiter) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[ip] >= l.limit {
		return false
	}
	l.n[ip]++
	return true
}

func (l *ipLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[ip] <= 1 {
		delete(l.n, ip)
		return
	}
	l.n[ip]--
}

// byteBudget is a weighted semaphore bounding the total bytes staged in memory
// across concurrent transactions. It exists so in-memory staging cannot grow
// without limit and trigger an OOM kill; a reservation that would exceed the
// budget is refused and the caller returns a temporary SMTP failure.
type byteBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
}

func newByteBudget(limit int64) *byteBudget { return &byteBudget{limit: limit} }

// tryAcquire reserves n bytes, reporting whether the reservation fit.
func (b *byteBudget) tryAcquire(n int64) bool {
	if n < 0 {
		n = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) release(n int64) {
	if n < 0 {
		n = 0
	}
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	b.mu.Unlock()
}

// SMTPBackend implements smtp.Backend. NewSession captures the connection
// identity (peer IP, HELO) once, so verification consumes out-of-band facts and
// never a header.
type SMTPBackend struct {
	s *Server
}

func (b *SMTPBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	// Bound total concurrent connections: Reject politely when over the cap
	// rather than accumulating unbounded goroutines.
	select {
	case b.s.sem <- struct{}{}:
	default:
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Too many connections"}
	}
	atomic.AddInt64(&b.s.active, 1)
	ip := PeerIP(c.Conn().RemoteAddr())
	_, isTLS := c.TLSConnectionState()
	ipKey := ipString(ip)
	if !b.s.iplim.acquire(ipKey) {
		select {
		case <-b.s.sem:
		default:
		}
		atomic.AddInt64(&b.s.active, -1)
		b.s.log.Warn("mx connection rejected: too many from source", "peer", ipKey)
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Too many connections from your address"}
	}
	return &session{
		srv:    b.s,
		peerIP: ip,
		ipKey:  ipKey,
		tls:    isTLS,
		helo:   strings.TrimSuffix(c.Hostname(), "."),
	}, nil
}

func PeerIP(addr net.Addr) net.IP {
	if addr == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return net.ParseIP(addr.String())
	}
	return net.ParseIP(host)
}

// session is one SMTP transaction. It is policy-free: RCPT resolution asks the
// core, and DATA stages, verifies and hands the whole accepted recipient set to
// the core in one signed request.
type session struct {
	srv     *Server
	peerIP  net.IP
	ipKey   string
	tls     bool
	helo    string
	from    string
	hasFrom bool
	rcpts   []acceptedRcpt
	// releaseOnce guarantees the connection slot is returned exactly once even
	// though go-smtp may call Logout more than once (on STARTTLS re-greet and on
	// connection close).
	releaseOnce sync.Once
	// dataCancel cancels an in-flight DATA read when the DATA deadline fires, so
	// a timed-out slow reader cannot keep filling the staging buffer (and
	// holding the RAM-budget reservation) after the transaction has failed.
	dataCancel context.CancelFunc
}

type acceptedRcpt struct {
	address string
	domain  string
}

func (s *session) Reset() {
	if s.dataCancel != nil {
		s.dataCancel()
		s.dataCancel = nil
	}
	s.from = ""
	s.hasFrom = false
	s.rcpts = nil
}

func (s *session) Logout() error {
	s.Reset()
	s.release()
	return nil
}

// release returns the global and per-source concurrency slots acquired in
// NewSession.
func (s *session) release() {
	s.releaseOnce.Do(func() {
		select {
		case <-s.srv.sem:
		default:
		}
		s.srv.iplim.release(s.ipKey)
		atomic.AddInt64(&s.srv.active, -1)
	})
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	// No relay: accept any MAIL FROM including the null path, but never
	// advertise or permit AUTH/submission.
	if s.srv.cfg.RequireTLS && !s.tls {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Must issue a STARTTLS command first"}
	}
	s.from = from
	s.hasFrom = true
	_ = opts
	return nil
}

// Rcpt resolves the recipient against current core configuration before DATA.
// Unknown, unauthorized or non-MX recipients are uniformly rejected 550 5.1.1.
// A core/DB/auth failure is a temporary 451, never evidence of an unknown
// recipient.
func (s *session) Rcpt(to string, opts *smtp.RcptOptions) error {
	_ = opts
	to = strings.TrimSpace(to)
	if to == "" {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.cfg.DNSTimeout)
	defer cancel()
	resp, err := s.srv.core.Resolve(ctx, []string{to})
	if err != nil {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.srv.log.Warn("mx rcpt temporary failure", "recipient", to, "peer", ipString(s.peerIP), "helo", s.helo)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing failure"}
	}
	for _, r := range resp.Results {
		if !strings.EqualFold(r.Recipient, to) {
			continue
		}
		if r.Accept {
			s.rcpts = append(s.rcpts, acceptedRcpt{address: strings.ToLower(to), domain: r.Domain})
			return nil
		}
		if r.Temporary {
			s.srv.log.Warn("mx rcpt temporary failure", "recipient", to, "peer", ipString(s.peerIP), "helo", s.helo)
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing failure"}
		}
	}
	atomic.AddInt64(&s.srv.reject, 1)
	s.srv.log.Warn("mx recipient rejected", "recipient", to, "peer", ipString(s.peerIP), "helo", s.helo)
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
}

// Data streams the whole original message to bounded staging, computes auth
// evidence once, then hands the accepted recipient set to the core in one
// signed request which fans out internally. One final SMTP response covers the
// transaction: 250 only when every accepted recipient has a durable success or
// a recorded duplicate; a transient failure makes the whole transaction
// temporary so the sender retries and committed recipients deduplicate.
func (s *session) Data(r io.Reader) error {
	if !s.hasFrom {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Need MAIL before DATA"}
	}
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "No valid recipients"}
	}
	// Reserve the maximum this message could consume before reading it, so
	// concurrent in-memory staging cannot exceed MX_STAGING_BYTES. If the
	// reservation does not fit, fail the transaction temporarily rather than
	// risk unbounded memory growth.
	if !s.srv.staging.tryAcquire(s.srv.cfg.MaxMessageBytes) {
		atomic.AddInt64(&s.srv.authTemp, 1)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Server busy, try again later"}
	}
	// The reservation is released by the staging copy goroutine when it exits,
	// not when Data returns: a timed-out read may outlive the transaction, and
	// releasing early would misstate the aggregate in-memory staging.
	raw, size, digest, err := StageMessageCtx(r, s.srv.cfg.MaxMessageBytes, s.srv.cfg.DataTimeout, func() {
		s.srv.staging.release(s.srv.cfg.MaxMessageBytes)
	}, &s.dataCancel)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: fmt.Sprintf("Message too large: maximum size is %d bytes", s.srv.cfg.MaxMessageBytes)}
		}
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Staging failure"}
	}
	// Zero the staging buffer (and drop the reference) once the transaction
	// completes, so the message bytes do not linger in this long-lived SMTP
	// process until GC.
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
		raw = nil
	}()

	fromDomain := FromHeaderDomain(raw)
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.cfg.DataTimeout)
	defer cancel()
	auth := s.srv.verify.Verify(ctx, bytes.NewReader(raw), s.peerIP, s.helo, s.from, fromDomain)

	recipients := make([]string, 0, len(s.rcpts))
	for _, rcpt := range s.rcpts {
		recipients = append(recipients, rcpt.address)
	}
	meta := mxwire.IngestMetadata{
		Recipients:   recipients,
		EnvelopeFrom: s.from,
		ClientIP:     ipString(s.peerIP),
		HELO:         s.helo,
		AuthResults:  auth,
	}
	accepted := len(s.rcpts)
	from := s.from
	resp, err := s.srv.core.Ingest(ctx, meta, bytes.NewReader(raw), size, digest)
	s.Reset()
	if err != nil {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.srv.log.Warn("mx message deferred", "from", from, "recipients", recipients, "peer", ipString(s.peerIP), "helo", s.helo, "size", size, "error", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
	}

	var transientFail, quotaFail, delivered, spamCount int
	for _, rr := range resp.PerRecipient {
		switch rr.MachineCode {
		case mxwire.CodeOK, mxwire.CodeDuplicate:
			delivered++
			if rr.Disposition == mxwire.DispositionSpam {
				spamCount++
				atomic.AddInt64(&s.srv.spam, 1)
			}
			if rr.MachineCode == mxwire.CodeDuplicate {
				atomic.AddInt64(&s.srv.dup, 1)
			}
		case mxwire.CodeQuota:
			quotaFail++
		default:
			// Unknown recipient, unauthorized, too large, invalid or a
			// transient failure: retry so a later RCPT round resolves current
			// routing; recipients already committed deduplicate.
			transientFail++
		}
	}
	// Any recipient the core did not report is treated as transient so the
	// sender retries rather than silently dropping it.
	if delivered+quotaFail+transientFail < accepted {
		transientFail += accepted - (delivered + quotaFail + transientFail)
	}
	atomic.AddInt64(&s.srv.accepted, int64(delivered))
	if quotaFail > 0 {
		s.srv.log.Warn("mx message deferred", "from", from, "recipients", recipients, "peer", ipString(s.peerIP), "helo", s.helo, "size", size, "reason", "quota")
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "Insufficient storage"}
	}
	if transientFail > 0 {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.srv.log.Warn("mx message deferred", "from", from, "recipients", recipients, "peer", ipString(s.peerIP), "helo", s.helo, "size", size, "reason", "transient")
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
	}
	s.srv.log.Info("mx message accepted", "from", from, "recipients", recipients, "peer", ipString(s.peerIP), "helo", s.helo, "size", size, "delivered", delivered, "spam", spamCount)
	return nil
}

var ErrTooLarge = errors.New("message too large")

// StageMessage reads the whole message into memory, bounded by maxBytes and a
// wall-clock deadline, returning the original bytes, size and hex SHA-256.
// Staging is RAM-only and released when the transaction completes; it is
// scratch, never a durable accepted-mail queue. The caller bounds the aggregate
// across concurrent transactions (MX_STAGING_BYTES).
func StageMessage(r io.Reader, maxBytes int64, timeout time.Duration) ([]byte, int64, string, error) {
	return StageMessageCtx(r, maxBytes, timeout, nil, nil)
}

// StageMessageCtx is StageMessage with a cancellable read. When timeout fires,
// the reader handed to the copy is cancelled, so the stranded goroutine stops
// promptly instead of continuing to fill the freed staging buffer. cancelOut
// may be nil; when non-nil it receives the cancel function for the read, which
// the caller must invoke (or hand lifecycle to a session Reset) once done.
// onDone, when non-nil, runs once when the copy goroutine exits, so a caller
// can release a resource (the RAM budget) the goroutine still holds.
func StageMessageCtx(r io.Reader, maxBytes int64, timeout time.Duration, onDone func(), cancelOut *context.CancelFunc) ([]byte, int64, string, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if onDone == nil {
		onDone = func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelOut != nil {
		*cancelOut = cancel
	}
	// Cancellation of the read also releases go-smtp's DATA flow, so the
	// goroutine cannot outlast the transaction.
	r, readerCancel := readerWithCancel(ctx, r)
	defer readerCancel()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer onDone()
		var buf bytes.Buffer
		h := sha256.New()
		// Read one byte past the cap so an oversize message is detected rather
		// than silently truncated.
		n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(r, maxBytes+1))
		if err == nil && n > maxBytes {
			err = ErrTooLarge
		}
		// go-smtp's DATA reader enforces the same cap and surfaces its own
		// sentinel once the cap is reached; normalize it so the caller maps
		// both paths to the same permanent 552.
		if errors.Is(err, smtp.ErrDataTooLarge) {
			err = ErrTooLarge
		}
		if err == nil && n == 0 {
			err = fmt.Errorf("empty message")
		}
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{b: buf.Bytes()}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, 0, "", res.err
		}
		sum := sha256.Sum256(res.b)
		return res.b, int64(len(res.b)), hex.EncodeToString(sum[:]), nil
	case <-time.After(timeout):
		// Cancel the read first so the goroutine releases the budget-protected
		// memory promptly, then wait a bounded moment for it to exit before
		// the caller's deferred release makes the budget available again.
		cancel()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case res := <-ch:
			if res.err != nil {
				return nil, 0, "", res.err
			}
			sum := sha256.Sum256(res.b)
			return res.b, int64(len(res.b)), hex.EncodeToString(sum[:]), nil
		case <-timer.C:
		}
		return nil, 0, "", fmt.Errorf("data read timeout")
	}
}

// cancelReader is an io.Reader that fails fast once ctx is cancelled.
type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// readerWithCancel wraps r so reads fail once ctx is cancelled. The returned
// cancel mirrors the ctx cancel for callers that want one handle.
func readerWithCancel(ctx context.Context, r io.Reader) (io.Reader, context.CancelFunc) {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &cancelReader{ctx: cancelCtx, r: r}, cancel
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

// FromHeaderDomain extracts the RFC5322.From domain for DMARC. It is the
// message's own From, which is exactly what DMARC is evaluated against; it is
// never used as an authenticated identity.
func FromHeaderDomain(raw []byte) string {
	// Bounded scan of the header block only.
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		idx = bytes.Index(raw, []byte("\n\n"))
	}
	headers := raw
	if idx >= 0 {
		headers = raw[:idx]
	}
	// Unfold continuation lines so a wrapped From: header is parsed whole.
	headers = bytes.ReplaceAll(headers, []byte("\r\n"), []byte("\n"))
	headers = bytes.ReplaceAll(headers, []byte("\n "), []byte(" "))
	headers = bytes.ReplaceAll(headers, []byte("\n\t"), []byte(" "))
	for _, line := range bytes.Split(headers, []byte("\n")) {
		l := bytes.TrimSpace(line)
		if len(l) < 5 || !bytes.EqualFold(l[:5], []byte("From:")) {
			continue
		}
		v := string(bytes.TrimSpace(l[5:]))
		return addressDomain(v)
	}
	return ""
}

// addressDomain extracts the domain from a From header value. It prefers
// net/mail (which handles display names, angle addresses and RFC 5322 comments)
// and falls back to a bounded heuristic only when parsing fails.
func addressDomain(v string) string {
	if addr, err := mail.ParseAddress(v); err == nil {
		if at := strings.LastIndex(addr.Address, "@"); at >= 0 {
			return strings.ToLower(strings.TrimSpace(addr.Address[at+1:]))
		}
	}
	v = strings.TrimSpace(v)
	lt := strings.LastIndex(v, "<")
	gt := strings.LastIndex(v, ">")
	if lt >= 0 && gt > lt {
		v = v[lt+1 : gt]
	}
	at := strings.LastIndex(v, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(v[at+1:]), "\"'"))
}

// ListenAndServe builds the go-smtp server and serves until ctx is cancelled.
// It advertises only capabilities the pipeline actually handles: no AUTH, no
// SMTPUTF8, no DSN, no BINARYMIME, no chunking is advertised by the edge.
func (s *Server) ListenAndServe(ctx context.Context, ln net.Listener) error {
	backend := &SMTPBackend{s: s}
	srv := smtp.NewServer(backend)
	// Route go-smtp's internal errors through the prefixed logger so the
	// container stream never carries unlabelled stdlib log lines.
	srv.ErrorLog = slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)
	srv.Domain = s.cfg.Hostname
	srv.MaxRecipients = s.cfg.MaxRecipients
	srv.MaxMessageBytes = s.cfg.MaxMessageBytes
	srv.MaxLineLength = 2000
	srv.ReadTimeout = s.cfg.ReadTimeout
	srv.WriteTimeout = s.cfg.WriteTimeout
	srv.AllowInsecureAuth = false
	srv.EnableSMTPUTF8 = false
	srv.EnableREQUIRETLS = false
	srv.EnableBINARYMIME = false
	srv.EnableDSN = false
	if s.cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.log.Info("mx edge listening", "addr", ln.Addr().String(), "hostname", s.cfg.Hostname)
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Stats returns a snapshot of counters for health/metrics endpoints.
func (s *Server) Stats() map[string]int64 {
	return map[string]int64{
		"active_connections": atomic.LoadInt64(&s.active),
		"accepted":           atomic.LoadInt64(&s.accepted),
		"spam":               atomic.LoadInt64(&s.spam),
		"duplicates":         atomic.LoadInt64(&s.dup),
		"rejected":           atomic.LoadInt64(&s.reject),
		"transient":          atomic.LoadInt64(&s.authTemp),
	}
}

// HealthHandler serves the edge's health and readiness. Readiness reflects
// usable core connectivity: it calls the core resolve endpoint with an empty
// recipient list, which the core accepts without side effects.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "stats": s.Stats()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := s.core.Resolve(ctx, nil); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "degraded", "error": "core unreachable"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
	})
	return mux
}

// ServeHealth serves the health handler until ctx is cancelled.
func (s *Server) ServeHealth(ctx context.Context, addr string) error {
	srv := &http.Server{Handler: s.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
