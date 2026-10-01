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
type Delivery interface {
	Resolve(context.Context, []string) (mxwire.ResolveResponse, error)
	Ingest(context.Context, mxwire.IngestMetadata, io.Reader, int64, string) (mxwire.IngestResponse, error)
	Close() error
}

type DeliveryFactory func() Delivery

func NewServerWithHandoff(cfg Config, log *slog.Logger, factory DeliveryFactory) *Server {
	return NewServerWithDelivery(cfg, log, factory)
}

type Server struct {
	cfg     Config
	core    *CoreClient
	factory DeliveryFactory
	verify  *Verifier
	log     *slog.Logger

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
	// One CoreClient (and therefore one http.Client/transport pool) is shared by
	// every SMTP session and by the readiness probe. Creating a client per
	// session would leak a transport pool per message.
	core := NewCoreClient(cfg)
	return newServer(cfg, log, func() Delivery { return core }, core)
}

func NewServerWithDelivery(cfg Config, log *slog.Logger, factory DeliveryFactory) *Server {
	return newServer(cfg, log, factory, NewCoreClient(cfg))
}

func newServer(cfg Config, log *slog.Logger, factory DeliveryFactory, core *CoreClient) *Server {
	if log == nil {
		log = slog.Default()
	}
	// Each staged message reserves MaxMessageBytes+1 (the extra byte detects an
	// oversize message), so the aggregate budget must admit at least one full
	// reservation even when the operator configured staging equal to the
	// message cap.
	stagingBytes := cfg.MaxStagingBytes
	if stagingBytes < cfg.MaxMessageBytes+1 {
		stagingBytes = cfg.MaxMessageBytes + 1
	}
	return &Server{
		cfg: cfg, core: core, factory: factory, verify: NewVerifier(cfg), log: log,
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
	// Recover the per-TCP-connection wrapper installed in ListenAndServe. This
	// is the same wrapper before and after STARTTLS (unwrapConn peels the
	// *tls.Conn), so the smtp_connection_id is retained across the upgrade.
	conn := unwrapConn(c.Conn())
	helo := strings.TrimSuffix(c.Hostname(), ".")
	// A nil wrapper (a test that served a bare listener) still gets a
	// connection id so correlation never silently degrades.
	connID := newCorrelationID()
	if conn != nil {
		connID = conn.id
	}
	// Bound total concurrent connections: Reject politely when over the cap
	// rather than accumulating unbounded goroutines. The global cap rejection
	// is logged distinctly from the per-source rejection.
	select {
	case b.s.sem <- struct{}{}:
	default:
		b.s.log.Warn("mx connection rejected: too many connections",
			AttrConnectionID, connID, "reason", "global_cap", "limit", b.s.cfg.MaxConnections)
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Too many connections"}
	}
	atomic.AddInt64(&b.s.active, 1)
	ip := PeerIP(c.Conn().RemoteAddr())
	state, isTLS := c.TLSConnectionState()
	ipKey := ipString(ip)
	if !b.s.iplim.acquire(ipKey) {
		select {
		case <-b.s.sem:
		default:
		}
		atomic.AddInt64(&b.s.active, -1)
		b.s.log.Warn("mx connection rejected: too many from source",
			AttrConnectionID, connID, "reason", "source_cap", "peer", ipKey, "limit", defaultMaxPerIP)
		return nil, &smtp.SMTPError{Code: 421, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Too many connections from your address"}
	}
	sess := &session{
		srv:     b.s,
		factory: b.s.factory,
		conn:    conn,
		connID:  connID,
		peerIP:  ip,
		ipKey:   ipKey,
		tls:     isTLS,
		helo:    helo,
	}
	sess.logSessionStarted(state, isTLS)
	return sess, nil
}

// logSessionStarted records the session greeting and, when the session is
// encrypted, the negotiated TLS parameters. A second session on the same
// connection with TLS active means STARTTLS succeeded.
func (s *session) logSessionStarted(state tls.ConnectionState, isTLS bool) {
	// Count every backend session on this connection: a second session with TLS
	// active means STARTTLS upgraded an existing plaintext session.
	count := 0
	if s.conn != nil {
		count = s.conn.noteSession()
	}
	args := []any{AttrConnectionID, s.connID, "helo", s.helo, "tls", isTLS}
	if isTLS {
		args = append(args, "tls_version", tlsVersionName(state.Version), "tls_cipher", tlsCipherName(state.CipherSuite))
	}
	s.srv.log.Info("mx session started", args...)
	if isTLS && count > 1 {
		s.srv.log.Info("mx starttls established",
			AttrConnectionID, s.connID,
			"helo", s.helo,
			"tls_version", tlsVersionName(state.Version),
			"tls_cipher", tlsCipherName(state.CipherSuite),
		)
	}
}

// tlsVersionName renders a TLS version as a stable token.
func tlsVersionName(v uint16) string {
	if name := tls.VersionName(v); name != "" {
		return name
	}
	return fmt.Sprintf("0x%04x", v)
}

// tlsCipherName renders a TLS cipher suite as a stable token.
func tlsCipherName(id uint16) string {
	if name := tls.CipherSuiteName(id); name != "" {
		return name
	}
	return fmt.Sprintf("0x%04x", id)
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
	srv      *Server
	factory  DeliveryFactory
	delivery Delivery
	conn     *connWrapper
	connID   string
	peerIP   net.IP
	ipKey    string
	tls      bool
	helo     string
	from     string
	hasFrom  bool
	rcpts    []acceptedRcpt
	// txnID is the current MAIL transaction's correlation id; txnStart is when
	// it began; txnTerminal is set once Data has emitted that transaction's
	// terminal event, so Reset can tell a completed transaction from an
	// abandoned one.
	txnID       string
	txnStart    time.Time
	txnTerminal bool
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
	s.resetTransaction("reset")
}

func (s *session) Logout() error {
	s.resetTransaction("connection_closed")
	s.release()
	return nil
}

// resetTransaction clears the in-flight MAIL transaction. When a transaction
// was started but never reached its terminal event (no DATA decision), it was
// abandoned by RSET, a new MAIL, STARTTLS or connection close, and that is
// recorded as a lifecycle event.
func (s *session) resetTransaction(reason string) {
	if s.txnID != "" && !s.txnTerminal {
		s.logTxn(s.srv.log.Info, "mx transaction abandoned",
			"reason", reason,
			"recipients", len(s.rcpts),
			"duration_ms", durationMs(time.Since(s.txnStart)),
		)
	}
	if s.dataCancel != nil {
		s.dataCancel()
		s.dataCancel = nil
	}
	s.txnID = ""
	s.txnStart = time.Time{}
	s.txnTerminal = false
	s.from = ""
	s.hasFrom = false
	s.rcpts = nil
	if s.delivery != nil {
		_ = s.delivery.Close()
		s.delivery = nil
	}
}

// txnAttrs is the correlation identity of the current transaction, attached to
// the contexts handed to the Delivery so the receiver can log correlated
// events.
func (s *session) txnAttrs() TransactionAttrs {
	return TransactionAttrs{
		ConnectionID:  s.connID,
		TransactionID: s.txnID,
		PeerIP:        ipString(s.peerIP),
		HELO:          s.helo,
	}
}

// logTxn invokes a logger with the connection/transaction correlation keys
// prepended, then the caller's own attributes.
func (s *session) logTxn(log func(string, ...any), msg string, extra ...any) {
	args := s.txnAttrs().SlogArgs()
	args = append(args, extra...)
	log(msg, args...)
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
	// Every MAIL attempt is assigned a transaction id before any policy check,
	// so even a rejected attempt is correlatable. This does not change the SMTP
	// outcome.
	if s.txnID != "" && !s.txnTerminal {
		s.logTxn(s.srv.log.Info, "mx transaction abandoned", "reason", "superseded_by_mail", "recipients", len(s.rcpts), "duration_ms", time.Since(s.txnStart).Milliseconds())
	}
	s.txnID = newCorrelationID()
	s.txnStart = time.Now()
	// A MAIL that is rejected outright is terminal: Reset must not report it as
	// abandoned later. The success path clears this again below.
	s.txnTerminal = true
	var declaredSize int64
	if opts != nil {
		declaredSize = opts.Size
	}
	// No relay: accept any MAIL FROM including the null path, but never
	// advertise or permit AUTH/submission.
	if s.srv.cfg.RequireTLS && !s.tls {
		s.logTxn(s.srv.log.Info, "mx mail transaction rejected",
			"reason", "tls_required", "from", from, "declared_size", declaredSize)
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Must issue a STARTTLS command first"}
	}
	if s.delivery != nil {
		_ = s.delivery.Close()
	}
	s.delivery = s.factory()
	s.txnTerminal = false
	s.from = from
	s.hasFrom = true
	s.logTxn(s.srv.log.Info, "mx mail transaction started",
		"from", from,
		"declared_size", declaredSize,
	)
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
		s.logRcpt("rejected", to, "empty")
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.cfg.DNSTimeout)
	defer cancel()
	if s.delivery == nil {
		s.logRcpt("rejected", to, "need_mail")
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Need MAIL before RCPT"}
	}
	// Attach the transaction correlation so the receiver's Resolve log lines
	// share the edge's ids.
	ctx = WithTransactionContext(ctx, s.txnAttrs())
	resp, err := s.delivery.Resolve(ctx, []string{to})
	if err != nil {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.logRcpt("temporary", to, "resolve_error")
		s.srv.log.Warn("mx rcpt temporary failure", append(s.txnAttrs().SlogArgs(), "recipient", to)...)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing failure"}
	}
	for _, r := range resp.Results {
		if !strings.EqualFold(r.Recipient, to) {
			continue
		}
		if r.Accept {
			for _, accepted := range s.rcpts {
				if strings.EqualFold(accepted.address, to) {
					s.logRcpt("accepted", to, "duplicate")
					return nil
				}
			}
			s.rcpts = append(s.rcpts, acceptedRcpt{address: strings.ToLower(to), domain: r.Domain})
			s.logRcpt("accepted", to, "")
			return nil
		}
		if r.Temporary {
			s.logRcpt("temporary", to, "routing")
			s.srv.log.Warn("mx rcpt temporary failure", append(s.txnAttrs().SlogArgs(), "recipient", to)...)
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing failure"}
		}
	}
	atomic.AddInt64(&s.srv.reject, 1)
	s.logRcpt("rejected", to, "unknown")
	s.srv.log.Warn("mx recipient rejected", append(s.txnAttrs().SlogArgs(), "recipient", to)...)
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
}

// logRcpt records one recipient routing outcome. The reason is a bounded,
// non-secret classifier, never the recipient's content.
func (s *session) logRcpt(outcome, recipient, reason string) {
	extra := []any{"outcome", outcome, "recipient", recipient}
	if reason != "" {
		extra = append(extra, "reason", reason)
	}
	s.logTxn(s.srv.log.Info, "mx recipient routing", extra...)
}

// Data streams the whole original message to bounded staging, computes auth
// evidence once, then hands the accepted recipient set to the core in one
// signed request which fans out internally. One final SMTP response covers the
// transaction: 250 only when every accepted recipient has a durable success or
// a recorded duplicate; a transient failure makes the whole transaction
// temporary so the sender retries and committed recipients deduplicate.
func (s *session) Data(r io.Reader) error {
	// Capture the transaction id now: s.Reset() clears it before Data returns,
	// but the connection wrapper still needs it for the final-reply write
	// event, and Data's own terminal events must carry it.
	txID := s.txnID
	txnStart := s.txnStart
	// The transaction always reaches a terminal decision in Data (even for the
	// early protocol errors below), so Reset after DATA must not report it as
	// abandoned. s.Reset() clears txnStart before Data returns, so use the
	// captured value for durations.
	s.txnTerminal = true
	defer s.finishData(txID)

	if !s.hasFrom {
		s.logDataDecision(txID, "503", "need_mail", 0)
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Need MAIL before DATA"}
	}
	if len(s.rcpts) == 0 {
		s.logDataDecision(txID, "554", "no_recipients", 0)
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "No valid recipients"}
	}
	// Reserve one byte over the message cap before reading it: the staged
	// buffer is exactly MaxMessageBytes+1 so an oversize message is detected
	// rather than truncated. The reservation is held until the raw bytes have
	// been cleared/handed off AND the staging copy goroutine has exited: the
	// bytes are still resident through verification and ingest, so releasing at
	// copy completion would misstate the aggregate in-memory staging. If the
	// reservation does not fit, fail the transaction temporarily rather than
	// risk unbounded memory growth.
	const stageOverhead = 1
	reserve := s.srv.cfg.MaxMessageBytes + stageOverhead
	if !s.srv.staging.tryAcquire(reserve) {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.logDataDecision(txID, "451", "staging_busy", 0)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Server busy, try again later"}
	}
	s.logDataStaging(txID, "start", "recipients", len(s.rcpts), "reserved_bytes", reserve)
	stageStart := time.Now()
	// copyDone is closed by the staging copy goroutine exactly once when it has
	// stopped touching the staged buffer. The deferred cleanup below clears the
	// buffer, then releases the reservation as soon as the goroutine is known
	// to be gone. On a timeout the goroutine can still be draining a slow
	// reader; the cleanup must not block SMTP for the sender, so it hands the
	// release to a short-lived goroutine instead. That goroutine is itself
	// bounded because it only waits for a copy that the cancelled reader will
	// terminate, and the reservation it holds is accounted by the global budget.
	copyDone := make(chan struct{})
	var raw []byte
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
		raw = nil
		select {
		case <-copyDone:
			s.srv.staging.release(reserve)
		default:
			go func() {
				<-copyDone
				s.srv.staging.release(reserve)
			}()
		}
	}()
	raw, size, digest, err := StageMessageCtx(r, s.srv.cfg.MaxMessageBytes, s.srv.cfg.DataTimeout, func() {
		close(copyDone)
	}, &s.dataCancel)
	if err != nil {
		reason := stagingErrorReason(err)
		s.logDataStaging(txID, "failed", "outcome", reason, "duration_ms", durationMs(time.Since(stageStart)))
		if errors.Is(err, ErrTooLarge) {
			s.logDataDecision(txID, "552", "too_large", durationMs(time.Since(txnStart)))
			return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: fmt.Sprintf("Message too large: maximum size is %d bytes", s.srv.cfg.MaxMessageBytes)}
		}
		s.logDataDecision(txID, "451", reason, durationMs(time.Since(txnStart)))
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Staging failure"}
	}
	s.logDataStaging(txID, "staged", "size", size, "digest", digest, "duration_ms", durationMs(time.Since(stageStart)))

	fromDomain := FromHeaderDomain(raw)
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.cfg.DataTimeout)
	defer cancel()
	verifyStart := time.Now()
	auth := s.srv.verify.Verify(ctx, bytes.NewReader(raw), s.peerIP, s.helo, s.from, fromDomain)
	s.logAuthEvidence(txID, auth)
	s.logTxn(s.srv.log.Info, "mx auth verification completed", "duration_ms", time.Since(verifyStart).Milliseconds())

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
	// The receiver consumes the correlation ids from the context; this is the
	// public helper contract (observability.go), not a Delivery signature
	// change.
	ctx = WithTransactionContext(ctx, s.txnAttrs())
	ingestStart := time.Now()
	resp, err := s.delivery.Ingest(ctx, meta, bytes.NewReader(raw), size, digest)
	ingestMs := durationMs(time.Since(ingestStart))
	attrs := s.txnAttrs().SlogArgs()
	s.Reset()
	if err != nil {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.logCoreAck(txID, "error", "recipients", accepted, "size", size, "duration_ms", ingestMs)
		s.srv.log.Warn("mx message deferred", append(attrs, "from", from, "recipients", recipients, "size", size, "error", err)...)
		s.logDataDecision(txID, "451", "ingest_error", durationMs(time.Since(txnStart)))
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
	}

	var transientFail, quotaFail, delivered, spamCount, dupCount int
	for _, rr := range resp.PerRecipient {
		switch rr.MachineCode {
		case mxwire.CodeOK, mxwire.CodeDuplicate:
			delivered++
			if rr.Disposition == mxwire.DispositionSpam {
				spamCount++
				atomic.AddInt64(&s.srv.spam, 1)
			}
			if rr.MachineCode == mxwire.CodeDuplicate {
				dupCount++
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
	// Core acknowledgement is logged separately from the final SMTP decision:
	// the same durable outcome can map to different SMTP replies, and a reader
	// needs both to tell "core stored it" from "we told the sender 250".
	s.logCoreAck(txID, "acknowledged",
		"recipients", accepted,
		"delivered", delivered,
		"duplicate", dupCount,
		"spam", spamCount,
		"quota_failed", quotaFail,
		"transient_failed", transientFail,
		"size", size,
		"duration_ms", ingestMs,
	)
	if quotaFail > 0 {
		s.srv.log.Warn("mx message deferred", append(attrs, "from", from, "recipients", recipients, "size", size, "reason", "quota")...)
		s.logDataDecision(txID, "452", "quota", durationMs(time.Since(txnStart)))
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "Insufficient storage"}
	}
	if transientFail > 0 {
		atomic.AddInt64(&s.srv.authTemp, 1)
		s.srv.log.Warn("mx message deferred", append(attrs, "from", from, "recipients", recipients, "size", size, "reason", "transient")...)
		s.logDataDecision(txID, "451", "transient", durationMs(time.Since(txnStart)))
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
	}
	s.srv.log.Info("mx message accepted", append(attrs, "from", from, "recipients", recipients, "size", size, "delivered", delivered, "spam", spamCount)...)
	s.logDataDecision(txID, "250", "accepted", durationMs(time.Since(txnStart)))
	return nil
}

// finishData is deferred by Data and runs after the decision has been computed
// but before go-smtp writes the SMTP response. Arming here (rather than at the
// start) keeps a panic or an early re-entrant write from consuming the probe.
func (s *session) finishData(txID string) {
	s.armFinalReply(txID)
}

// armFinalReply asks the connection wrapper to record the outcome of the next
// transport write (the DATA response). Under STARTTLS the wrapper sees only the
// encrypted first fragment, so the resulting event reports transport success,
// not proof that the full SMTP reply reached the peer.
func (s *session) armFinalReply(txID string) {
	if s.conn != nil {
		s.conn.armReply(txID)
	}
}

// logDataStaging records a DATA staging lifecycle outcome.
func (s *session) logDataStaging(txID, stage string, extra ...any) {
	args := []any{AttrConnectionID, s.connID, AttrTransactionID, txID, "stage", stage}
	args = append(args, extra...)
	s.srv.log.Info("mx data staging", args...)
}

// logAuthEvidence records the full normalized SPF/DKIM/DMARC evidence array
// alongside the enabled toggles, so a reader can distinguish "verification
// disabled" (enabled false, empty result) from "enabled but no evidence"
// (enabled true, empty/none result). It never logs message content, subject,
// header values or key material. The evidence types are bounded diagnostics:
// results, domains, selectors and classifier strings only.
func (s *session) logAuthEvidence(txID string, auth mxwire.AuthResults) {
	s.srv.log.Info("mx auth evidence",
		AttrConnectionID, s.connID,
		AttrTransactionID, txID,
		"spf_enabled", s.srv.cfg.VerifySPF,
		"dkim_enabled", s.srv.cfg.VerifyDKIM,
		"dmarc_enabled", s.srv.cfg.VerifyDMARC,
		"auth_results", auth,
	)
}

// logCoreAck records the durable outcome the core reported, separate from the
// SMTP reply the sender eventually receives.
func (s *session) logCoreAck(txID, outcome string, extra ...any) {
	args := []any{AttrConnectionID, s.connID, AttrTransactionID, txID, "outcome", outcome}
	args = append(args, extra...)
	s.srv.log.Info("mx core ingest result", args...)
}

// logDataDecision records the final SMTP decision for the transaction, with the
// whole-transaction duration in milliseconds. It is deliberately separate from
// the core acknowledgement and from the transport reply-write outcome observed
// by the connection wrapper.
func (s *session) logDataDecision(txID, code, reason string, durationMs int64) {
	enhanced := map[string]string{"250": "2.0.0", "451": "4.3.0", "452": "4.2.2", "552": "5.3.4", "503": "5.5.1", "554": "5.5.1"}[code]
	s.srv.log.Info("mx smtp transaction decision",
		AttrConnectionID, s.connID,
		AttrTransactionID, txID,
		"smtp_code", code,
		"enhanced_code", enhanced,
		"reason", reason,
		"duration_ms", durationMs,
	)
}

// stagingErrorReason classifies a StageMessage error into a bounded,
// non-content token for logs. It does not change the SMTP behaviour.
func stagingErrorReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, smtp.ErrTooLongLine):
		return "line_too_long"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "empty"):
		return "empty"
	default:
		return "read_error"
	}
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
// the reader handed to the copy is cancelled and the call always returns a
// timeout error, even if the stranded copy later completes successfully: the
// caller has already failed the transaction, so accepting the late bytes would
// report success for work it has abandoned. cancelOut may be nil; when non-nil
// it receives the cancel function for the read, which the caller must invoke (or
// hand lifecycle to a session Reset) once done. onDone, when non-nil, runs
// exactly once when the copy goroutine has finished touching the staged buffer,
// before any result is delivered, so a caller can wait on it before releasing a
// resource (the RAM budget) the bytes occupy.
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
		n   int64
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// Exactly cap+1 bytes are allocated: the extra byte is how an oversize
		// message is detected, and nothing beyond it is ever read. No buffer
		// growth heuristic can over-allocate past the reservation.
		cap1 := maxBytes + 1
		out := result{}
		// Allocate lazily only when a safe, positive cap is known; a huge or
		// negative cap is rejected by the read below rather than by a wild
		// allocation.
		if cap1 <= 0 {
			out.err = ErrTooLarge
			onDone()
			ch <- out
			return
		}
		buf := make([]byte, cap1)
		// io.ReadFull fills the whole cap+1 unless the reader ends first. A
		// short message ends with io.ErrUnexpectedEOF (or io.EOF for an empty
		// one); that is a successful read of n bytes, not a failure.
		n, err := io.ReadFull(io.LimitReader(r, cap1), buf)
		switch {
		case err == nil:
			// Filled cap+1 bytes: oversize.
			out.err = ErrTooLarge
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			if n == 0 {
				out.err = fmt.Errorf("empty message")
			} else {
				out.b = buf[:n]
				out.n = int64(n)
			}
		case errors.Is(err, smtp.ErrDataTooLarge):
			// go-smtp's DATA reader enforces the same cap; normalize its
			// sentinel so the caller maps both paths to the same permanent 552.
			out.err = ErrTooLarge
		default:
			out.err = err
		}
		if ctx.Err() != nil {
			out.err = ctx.Err()
			out.b = nil
		}
		if out.err != nil && buf != nil {
			// Clear the whole staged buffer on any error (including a
			// cancellation/timeout) so abandoned bytes do not linger in this
			// long-lived process.
			for i := range buf {
				buf[i] = 0
			}
		}
		// onDone runs before the result is published, so a caller that receives
		// a result also observes copyDone closed: the goroutine holds no
		// reference to the buffer after this point.
		onDone()
		ch <- out
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, 0, "", res.err
		}
		sum := sha256.Sum256(res.b)
		return res.b, res.n, hex.EncodeToString(sum[:]), nil
	case <-time.After(timeout):
		// Cancel the read so the stranded goroutine releases the
		// budget-protected memory promptly, then fail immediately. The caller
		// owns waiting on onDone to release the reservation; SMTP is never
		// blocked here waiting for a slow sender to drain.
		cancel()
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
	// Wrap the listener so every accepted TCP connection is a connWrapper: it
	// carries the stable smtp_connection_id and observes connection lifecycle
	// and reply-write outcomes. go-smtp's Serve accepts from this listener.
	tracked := &trackingListener{Listener: ln, srv: s}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(tracked) }()
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
