// Package runtime runs the included (embedded) Dial MX receiver inside a child
// process that is spawned in standby before the core drops privileges.
//
// The child owns no database and no application key, and writes no files. It
// binds its listeners only when the core activates it, using the existing
// mxagent SMTP edge and receiver session runtime. Activation and deactivation
// can repeat for the life of the process: Deactivate stops admission, drains
// in-flight work within a bound, then stops sessions, without exiting the child.
package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// Fixed included endpoints. The session endpoint is loopback-only so only the
// core can dial it; the SMTP edge accepts inbound mail on the container's
// network interface.
const (
	DefaultSessionAddr  = "127.0.0.1:8443"
	DefaultSMTPAddr     = ":2525"
	DefaultDrainTimeout = 10 * time.Second
)

// Config activates one receiver instance. The listener addresses are fields so
// tests can bind ephemeral ports; the core's Configure path fixes them to the
// constants above.
type Config struct {
	Hostname     string
	SMTP         mxagent.Config
	Secret       string
	SessionAddr  string
	DrainTimeout time.Duration
}

// Runtime is the included receiver child's lifecycle manager. It satisfies
// control.Handler. Its listener goroutines may update the reported state
// independently, so all state is guarded by mu.
type Runtime struct {
	log *slog.Logger

	mu     sync.Mutex
	run    *run
	state  control.State
	errMsg string
}

// run is one activation's live resources. stop is idempotent via once.
type run struct {
	cfg         Config
	r           *receiver.Receiver
	edge        *mxagent.Server
	httpSrv     *http.Server
	cancel      context.CancelFunc
	smtpAddr    string
	sessionAddr string
	edgeDone    chan struct{}
	httpDone    chan struct{}
	once        sync.Once
	// smtpDrained records whether every in-flight SMTP transaction finished
	// within the configured drain bound before sessions were stopped. It is set
	// once during stop and read afterwards.
	smtpDrained bool
}

// New returns a runtime in standby that logs through log.
func New(log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{log: log, state: control.StateStandby}
}

// Configure implements control.Handler: it maps the wire settings onto the
// fixed included endpoints and activates the receiver. A repeated Configure
// while active is handled deliberately: the current run is deactivated within
// the drain bound (so in-flight mail can commit) and re-armed with the new
// settings, rather than failing the core.
//
// The new configuration is fully validated before any deactivation: the PEM
// pair is parsed into an in-memory certificate and the RequireTLS/pair rules
// are checked first, so an invalid new configuration is reported without taking
// the running receiver down.
func (rt *Runtime) Configure(ctx context.Context, s control.Settings) (control.Status, error) {
	drain := s.DrainTimeout
	if drain <= 0 {
		drain = DefaultDrainTimeout
	}
	cfg := Config{
		Hostname:     s.Hostname,
		SMTP:         s.SMTP,
		Secret:       s.Secret,
		SessionAddr:  DefaultSessionAddr,
		DrainTimeout: drain,
	}
	// The included receiver's endpoints are fixed: the core dials the loopback
	// session endpoint and publishes the SMTP edge. Settings cannot move them.
	cfg.SMTP.ListenAddr = DefaultSMTPAddr

	cert, err := parseTLSPair(s.TLSCertificatePEM, s.TLSPrivateKeyPEM)
	if err != nil {
		return rt.Status(), err
	}
	if cert != nil {
		// The child cannot read the core's filesystem, so the in-memory
		// certificate supersedes any file paths that may have been carried for
		// parity with the standalone receiver.
		cfg.SMTP.TLSCertificate = cert
		cfg.SMTP.TLSCertFile = ""
		cfg.SMTP.TLSKeyFile = ""
	}
	if err := validateSMTPTLS(cfg.SMTP); err != nil {
		return rt.Status(), err
	}

	st, err := rt.Deactivate(ctx)
	if err != nil && !errors.Is(err, ErrDrainIncomplete) {
		// The previous run did not retire (the drain bound expired with the run
		// still present). Do not activate on top of it; the core retries.
		return st, fmt.Errorf("runtime: reconfigure drain: %w", err)
	}
	// ErrDrainIncomplete means the old run retired to standby but some in-flight
	// mail may not have committed. Activation proceeds: the new configuration is
	// validated and the child must not be left unusable because the drain cut.
	return rt.Activate(ctx, cfg)
}

// Activate binds the SMTP edge and session listeners and starts serving. It
// refuses when already active. A failed listener is recorded independently and
// reported through Status; it never takes the process down.
func (rt *Runtime) Activate(ctx context.Context, cfg Config) (control.Status, error) {
	rt.mu.Lock()
	if rt.run != nil {
		st := rt.statusLocked()
		rt.mu.Unlock()
		return st, errors.New("runtime: receiver already active")
	}
	rt.mu.Unlock()

	if cfg.SessionAddr == "" {
		cfg.SessionAddr = DefaultSessionAddr
	}
	if cfg.SMTP.ListenAddr == "" {
		cfg.SMTP.ListenAddr = DefaultSMTPAddr
	}
	normalizeSMTP(&cfg.SMTP)
	if cfg.Hostname == "" {
		cfg.Hostname = cfg.SMTP.Hostname
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "localhost"
	}
	cfg.SMTP.Hostname = cfg.Hostname
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	if err := validateSMTPTLS(cfg.SMTP); err != nil {
		return rt.Status(), err
	}

	r := receiver.New(receiver.Config{Mode: "single", CoreKey: cfg.Secret, SMTP: cfg.SMTP}, rt.log)
	edge := mxagent.NewServerWithHandoff(cfg.SMTP, rt.log, func() mxagent.Delivery {
		return r.NewDelivery()
	})

	smtpLn, err := net.Listen("tcp", cfg.SMTP.ListenAddr)
	if err != nil {
		return rt.Status(), fmt.Errorf("runtime: listen smtp: %w", err)
	}
	sessionLn, err := net.Listen("tcp", cfg.SessionAddr)
	if err != nil {
		_ = smtpLn.Close()
		return rt.Status(), fmt.Errorf("runtime: listen session: %w", err)
	}

	tracker := receiver.NewTransportTracker(rt.log)
	srv := &http.Server{
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext:       tracker.ConnContext,
		ConnState:         tracker.ConnState,
	}
	// Single mode over a loopback listener serves cleartext HTTP/2 (h2c): the
	// session never leaves the host and the core dials http://127.0.0.1:8443.
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP2(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	runCtx, cancel := context.WithCancel(context.Background())
	rn := &run{
		cfg:         cfg,
		r:           r,
		edge:        edge,
		httpSrv:     srv,
		cancel:      cancel,
		smtpAddr:    smtpLn.Addr().String(),
		sessionAddr: sessionLn.Addr().String(),
		edgeDone:    make(chan struct{}),
		httpDone:    make(chan struct{}),
	}

	rt.mu.Lock()
	rt.run = rn
	rt.state = control.StateActive
	rt.errMsg = ""
	rt.mu.Unlock()

	go func() {
		defer close(rn.edgeDone)
		if e := edge.ListenAndServe(runCtx, smtpLn); e != nil && runCtx.Err() == nil {
			rt.fail(rn, "smtp listener: "+e.Error())
		}
	}()
	go func() {
		defer close(rn.httpDone)
		if e := srv.Serve(sessionLn); e != nil && e != http.ErrServerClosed && runCtx.Err() == nil {
			rt.fail(rn, "session listener: "+e.Error())
		}
	}()

	rt.log.Info("included receiver activated", "smtp", rn.smtpAddr, "session", rn.sessionAddr)
	return rt.Status(), nil
}

// fail records an independent listener failure and tears the run down without
// clearing the failure state, so a later Status reports it.
func (rt *Runtime) fail(rn *run, msg string) {
	rt.mu.Lock()
	if rt.run != rn {
		rt.mu.Unlock()
		return
	}
	rt.state = control.StateFailed
	rt.errMsg = msg
	rt.mu.Unlock()

	rt.log.Error("included receiver listener failed", "error", msg)
	go func() {
		rn.stop(context.Background())
		_ = rn.wait(context.Background())
		rt.mu.Lock()
		if rt.run == rn {
			rt.run = nil
		}
		rt.mu.Unlock()
	}()
}

// Deactivate stops admission, drains in-flight work within a bound, then stops
// sessions. It never calls receiver.Stop before the drain, because Stop cancels
// pinned delivery. It is idempotent and re-entrant: if the bound elapses it
// returns the context error and a retry completes the drain.
func (rt *Runtime) Deactivate(ctx context.Context) (control.Status, error) {
	rt.mu.Lock()
	rn := rt.run
	if rn == nil {
		if rt.state == control.StateFailed {
			rt.state = control.StateStandby
			rt.errMsg = ""
		}
		st := rt.statusLocked()
		rt.mu.Unlock()
		return st, nil
	}
	rt.state = control.StateDraining
	rt.mu.Unlock()

	rn.stop(ctx)
	werr := rn.wait(ctx)
	drained := rn.smtpDrained

	rt.mu.Lock()
	if werr == nil {
		// The listeners have stopped: the run is retired and the child is ready
		// to be reactivated.
		if rt.run == rn {
			rt.run = nil
		}
		rt.state = control.StateStandby
		rt.errMsg = ""
	} else {
		// The bound elapsed. Keep the run so a retry can finish the drain; the
		// child remains in draining until then.
		rt.state = control.StateDraining
		rt.errMsg = werr.Error()
	}
	st := rt.statusLocked()
	rt.mu.Unlock()
	if werr == nil && !drained {
		// The listeners stopped, but the SMTP drain bound elapsed with
		// transactions still in flight. Report it so the core knows some mail
		// may not have committed; the receiver is nevertheless back in standby.
		return st, ErrDrainIncomplete
	}
	return st, werr
}

// ErrDrainIncomplete is returned by Deactivate when the SMTP drain bound
// elapsed with transactions still in flight. The receiver is still returned to
// standby; the error tells the core that some in-flight mail may not have
// committed.
var ErrDrainIncomplete = errors.New("runtime: SMTP drain bound elapsed; in-flight mail may have been cut")

// Status implements control.Handler.
func (rt *Runtime) Status() control.Status {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.statusLocked()
}

func (rt *Runtime) statusLocked() control.Status {
	st := control.Status{State: rt.state, Error: rt.errMsg}
	if rt.run != nil {
		st.SMTPAddr = rt.run.smtpAddr
		st.SessionAddr = rt.run.sessionAddr
		st.ActiveConnections = rt.run.r.Stats()["active_connections"] + rt.run.edge.Stats()["active_connections"]
	}
	return st
}

// Close stops any active run and leaves the runtime in standby. It is the final
// shutdown path for the child process.
func (rt *Runtime) Close() error {
	rt.mu.Lock()
	grace := DefaultDrainTimeout
	if rt.run != nil && rt.run.cfg.DrainTimeout > 0 {
		grace = rt.run.cfg.DrainTimeout
	}
	rt.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), grace+5*time.Second)
	defer cancel()
	_, err := rt.Deactivate(ctx)
	return err
}

// stop initiates the ordered shutdown exactly once, in two phases:
//
//  1. Stop accepting new SMTP mail and let in-flight SMTP transactions drain,
//     bounded by the configured drain timeout, while the core session stays
//     alive. A DATA handoff already in progress can therefore still complete its
//     ingest and commit through the core session. This is the deliberate drain.
//  2. Only then stop session admission and stop sessions. This order is what
//     prevents receiver.Stop from cancelling pinned delivery before the SMTP
//     transaction that owns it has committed.
//
// The SMTP and session listeners are owned by their servers (go-smtp's
// Shutdown and http.Server.Close close them), so they are not closed here.
func (rn *run) stop(ctx context.Context) {
	rn.once.Do(func() {
		// Phase 1: stop SMTP admission, drain in-flight SMTP against the live
		// core session.
		rn.cancel()
		drain := rn.cfg.DrainTimeout
		if drain <= 0 {
			drain = DefaultDrainTimeout
		}
		dctx, cancel := context.WithTimeout(ctx, drain)
		rn.smtpDrained = rn.waitEdge(dctx)
		cancel()
		// Phase 2: stop session admission and stop sessions. r.Stop cancels any
		// session context that is still open; by now the SMTP drain has either
		// completed or exhausted its bound.
		rn.r.Stop()
		_ = rn.httpSrv.Close()
	})
}

// waitEdge reports whether the SMTP edge finished draining before ctx expired.
func (rn *run) waitEdge(ctx context.Context) bool {
	select {
	case <-rn.edgeDone:
		return true
	case <-ctx.Done():
		return false
	}
}

// wait blocks until both listener goroutines have returned or ctx expires.
func (rn *run) wait(ctx context.Context) error {
	for _, done := range []chan struct{}{rn.edgeDone, rn.httpDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// defaultStagingBytes is the aggregate in-memory staging budget when the core
// does not supply one. It matches the standalone receiver's MX_STAGING_BYTES
// default so the included receiver can hold several concurrent messages rather
// than regressing to a single one.
const defaultStagingBytes = 256 << 20

// normalizeSMTP fills the same defaults the receiver and the SMTP edge expect,
// so a partially populated Settings cannot leave the edge with unbounded or
// zero limits.
func normalizeSMTP(s *mxagent.Config) {
	if s.Hostname == "" {
		s.Hostname = "localhost"
	}
	if s.MaxMessageBytes < 1<<20 {
		s.MaxMessageBytes = 30 << 20
	}
	// A zero staging budget means "unset", not "one message": default it to the
	// documented aggregate budget, then enforce the one-message floor.
	if s.MaxStagingBytes <= 0 {
		s.MaxStagingBytes = defaultStagingBytes
	}
	if s.MaxStagingBytes < s.MaxMessageBytes+1 {
		s.MaxStagingBytes = s.MaxMessageBytes + 1
	}
	if s.MaxConnections < 1 {
		s.MaxConnections = 256
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
}

// parseTLSPair builds an in-memory certificate from the PEM pair carried over
// the control channel. Both must be empty (no STARTTLS) or both present; a lone
// half is rejected. tls.X509KeyPair verifies the key matches the certificate.
func parseTLSPair(certPEM, keyPEM string) (*tls.Certificate, error) {
	certPEM = strings.TrimSpace(certPEM)
	keyPEM = strings.TrimSpace(keyPEM)
	if certPEM == "" && keyPEM == "" {
		return nil, nil
	}
	if certPEM == "" || keyPEM == "" {
		return nil, errors.New("runtime: TLS certificate and private key must be set together")
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("runtime: parse SMTP TLS certificate: %w", err)
	}
	return &cert, nil
}

// validateSMTPTLS validates the SMTP STARTTLS material before any listener is
// bound, so a malformed pair is reported as a clean activation error rather than
// a late listen failure. The included receiver receives an in-memory
// certificate; the file pair is accepted for parity with the standalone
// receiver. The session endpoint is always cleartext h2c on loopback, so only
// the SMTP certificate is relevant.
func validateSMTPTLS(s mxagent.Config) error {
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		return errors.New("runtime: MX_TLS_CERT and MX_TLS_KEY must be set together")
	}
	if s.RequireTLS && s.TLSCertificate == nil && s.TLSCertFile == "" {
		return errors.New("runtime: MX_REQUIRE_TLS requires a TLS certificate")
	}
	if s.TLSCertFile == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(s.TLSCertFile, s.TLSKeyFile); err != nil {
		return fmt.Errorf("runtime: load SMTP TLS certificate: %w", err)
	}
	return nil
}

var _ control.Handler = (*Runtime)(nil)
