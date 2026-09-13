package mxagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-smtp"

	"gatehouse-mail/internal/mxwire"
)

// Server is the policy-free SMTP edge. It enforces connection/recipient/size
// bounds and strict framing, stages the original bytes, computes auth evidence
// and issues one signed ingest per accepted recipient. It never returns SMTP
// success until the core has durably handled every accepted recipient.
type Server struct {
	cfg    Config
	core   *CoreClient
	verify *Verifier
	log    *slog.Logger

	sem      chan struct{}
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
	return &Server{
		cfg: cfg, core: NewCoreClient(cfg), verify: NewVerifier(cfg), log: log,
		sem: make(chan struct{}, cfg.MaxConnections),
	}
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
	return &session{
		srv:    b.s,
		conn:   c,
		peerIP: ip,
		helo:   strings.TrimSuffix(c.Hostname(), "."),
		start:  time.Now(),
	}, nil
}

func (b *SMTPBackend) release() {
	select {
	case <-b.s.sem:
	default:
	}
	atomic.AddInt64(&b.s.active, -1)
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
// core, and DATA stages, verifies and fans out one signed ingest per recipient.
type session struct {
	srv     *Server
	conn    *smtp.Conn
	peerIP  net.IP
	helo    string
	start   time.Time
	from    string
	hasFrom bool
	rcpts   []acceptedRcpt
}

type acceptedRcpt struct {
	address string
	domain  string
}

func (s *session) Reset() {
	s.from = ""
	s.hasFrom = false
	s.rcpts = nil
}

func (s *session) Logout() error {
	s.Reset()
	return nil
}

func (s *session) Mail(from string, opts *smtp.MailOptions) error {
	// No relay: accept any MAIL FROM including the null path, but never
	// advertise or permit AUTH/submission.
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
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary routing failure"}
		}
	}
	atomic.AddInt64(&s.srv.reject, 1)
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Unknown recipient"}
}

// Data stages the whole original message before any fan-out, computes auth
// evidence once, then delivers each accepted recipient through its own
// authorized core request. One final SMTP response covers the transaction:
// 250 only when every accepted recipient has a durable success or a recorded
// duplicate; a transient failure makes the whole transaction temporary so the
// sender retries and committed recipients deduplicate.
func (s *session) Data(r io.Reader) error {
	if !s.hasFrom {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "Need MAIL before DATA"}
	}
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "No valid recipients"}
	}
	raw, err := StageMessage(r, s.srv.cfg.StagingDir, s.srv.cfg.MaxMessageBytes, s.srv.cfg.DataTimeout)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message too large"}
		}
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Staging failure"}
	}
	digest := mxwire.BodyDigest(raw)
	fromDomain := FromHeaderDomain(raw)
	ctx := context.Background()
	auth := s.srv.verify.Verify(ctx, raw, s.peerIP, s.helo, s.from, fromDomain)

	var transientFail bool
	var quotaFail bool
	for _, rcpt := range s.rcpts {
		meta := mxwire.IngestMetadata{
			Recipient:           rcpt.address,
			EnvelopeFrom:        s.from,
			ClientIP:            ipString(s.peerIP),
			HELO:                s.helo,
			ContentDigest:       digest,
			Size:                int64(len(raw)),
			DeliveryFingerprint: mxwire.DeliveryFingerprint(s.from, rcpt.address, digest),
			AuthResults:         auth,
		}
		resp, err := s.srv.core.Ingest(ctx, meta, raw)
		if err != nil {
			transientFail = true
			s.srv.log.Warn("mx ingest failed", "recipient", rcpt.address, "error", err)
			continue
		}
		switch resp.MachineCode {
		case mxwire.CodeOK, mxwire.CodeDuplicate:
			if resp.Disposition == mxwire.DispositionSpam {
				atomic.AddInt64(&s.srv.spam, 1)
			}
			if resp.MachineCode == mxwire.CodeDuplicate {
				atomic.AddInt64(&s.srv.dup, 1)
			}
			continue
		case mxwire.CodeQuota:
			quotaFail = true
		case mxwire.CodeTempFail:
			transientFail = true
		case mxwire.CodeUnknownRecipient, mxwire.CodeUnauthorized, mxwire.CodeTooLarge, mxwire.CodeInvalid:
			// A previously accepted recipient the core now rejects is not a
			// reason to permanently drop the whole transaction; retry so a
			// subsequent RCPT round resolves current routing.
			transientFail = true
		default:
			transientFail = true
		}
	}
	s.Reset()
	if quotaFail {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 2, 2}, Message: "Insufficient storage"}
	}
	if transientFail {
		atomic.AddInt64(&s.srv.authTemp, 1)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure"}
	}
	atomic.AddInt64(&s.srv.accepted, int64(len(s.rcpts)))
	return nil
}

var ErrTooLarge = errors.New("message too large")

// StageMessage reads all of r with a hard bound and a wall-clock deadline,
// returning the exact original bytes. The message is bounded by MaxMessageBytes
// and the edge's staging area is scratch, never a durable accepted-mail queue.
func StageMessage(r io.Reader, dir string, maxBytes int64, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, io.LimitReader(r, maxBytes+1))
		ch <- result{b: buf.Bytes(), err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		if int64(len(res.b)) > maxBytes {
			return nil, ErrTooLarge
		}
		if len(res.b) == 0 {
			return nil, fmt.Errorf("empty message")
		}
		return res.b, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("data read timeout")
	}
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

func addressDomain(v string) string {
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

// EnsureStaging creates the bounded staging directory with 0700 permissions.
func EnsureStaging(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("staging dir is empty")
	}
	return os.MkdirAll(dir, 0o700)
}
