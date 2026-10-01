package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/launcher"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// mxReconcileInterval bounds how long a settings change can take to reach the
// receiver when a Wake is missed, and how long a failed activation waits before
// it is retried. Wake delivers changes promptly; the ticker is the safety net
// and the retry bound.
const mxReconcileInterval = 30 * time.Second

// includedSessionURL is the fixed loopback session endpoint the standby child
// binds. It mirrors dialmx/runtime.DefaultSessionAddr; the literal is repeated
// here so the core does not import the receiver's runtime package.
const includedSessionURL = "http://127.0.0.1:8443"

// Runtime MX states are the shared app vocabulary so the controller and the UI
// never drift on state strings.
const (
	stateDisabled    = app.MXStateDisabled
	stateActive      = app.MXStateActive
	stateConnecting  = app.MXStateConnecting
	stateFailed      = app.MXStateFailed
	stateUnavailable = app.MXStateUnavailable
)

// includedEdge is the standby child control surface the controller drives. It is
// satisfied by *launcher.Edge and exists so tests can observe the drain-before-
// configure ordering and the child-exit handling without spawning a real
// process.
type includedEdge interface {
	Configure(ctx context.Context, s control.Settings) (control.Status, error)
	Deactivate(ctx context.Context) (control.Status, error)
	Status(ctx context.Context) (control.Status, error)
	// Wait reports child process exit. A child that exits cannot be restarted
	// by this process (it was spawned once before the privilege drop), so the
	// runtime reports the failure honestly and retries Configure, which will
	// itself fail.
	Wait() <-chan struct{}
	ExitError() error
}

// appliedSentinel marks an applied revision that must be re-applied on the next
// reconcile. It is used when the included child exits: the persisted revision
// (always >= 0) no longer matches, so the ticker retries.
const appliedSentinel = -1

// mxRuntime is the process-owned MX receiver controller. It implements the
// app.MXReceiverRuntime hook the application calls after a settings change, and
// owns both receiver shapes:
//
//   - included: a standby child spawned before the privilege drop. The runtime
//     activates it over the private control channel with the persisted (or
//     freshly generated and persisted) bearer key, and points the private dialer
//     at the child's loopback session endpoint.
//   - remote: no child is used. The private dialer connects outbound to the
//     operator's receiver URL with the persisted key and CA.
//
// Reconciliation is generation-based: it reads the persisted revision and
// applies it when it differs from the last successfully applied revision. A
// failure is recorded in status, does not advance the applied revision, and is
// retried on the next tick, so a misconfigured or unavailable receiver cannot
// take mailboxes or the API down and recovers once the cause is fixed.
type mxRuntime struct {
	log *slog.Logger
	svc *app.Service

	// edge is the standby child, present only when the process started as root
	// and the binary is available. It is nil in a rootless deployment, where
	// remote mode is the only supported receiver shape.
	edge includedEdge

	dataDir         string
	maxMessageBytes int64
	maxTransactions int

	mu      sync.Mutex
	applied int64 // last successfully applied persisted revision
	mode    string
	state   string
	detail  string
	// pending is true when the latest read revision has not been applied yet
	// (in flight, or failed and awaiting retry).
	pending     bool
	smtpAddr    string
	sessionAddr string
	activeConns int64

	// private is the running single-core dialer for the current receiver.
	privateCancel context.CancelFunc
	privateDone   chan struct{}
	privateMgr    *mxdial.Manager

	// cancel stops Run; done is closed when Run has finished its shutdown. main
	// waits on done (bounded) so the child and dialer are stopped before the
	// store closes.
	cancel context.CancelFunc
	done   chan struct{}

	wake chan struct{}
}

// newMXRuntime builds the controller. edge may be nil in a rootless process.
func newMXRuntime(svc *app.Service, edge includedEdge, log *slog.Logger) *mxRuntime {
	return &mxRuntime{
		log:             log,
		svc:             svc,
		edge:            edge,
		dataDir:         svc.Config.DataDir,
		maxMessageBytes: svc.Config.MaxMessageBytes,
		maxTransactions: svc.Config.InboundConcurrency,
		state:           stateDisabled,
		wake:            make(chan struct{}, 1),
		done:            make(chan struct{}),
	}
}

// Wake implements app.MXReceiverRuntime. It never blocks.
func (rt *mxRuntime) Wake() {
	select {
	case rt.wake <- struct{}{}:
	default:
	}
}

// Status implements app.MXReceiverRuntime. While a newer revision is being
// applied it reports "connecting", so a poller never sees the previous shape's
// "active" for a configuration that has not taken effect.
func (rt *mxRuntime) Status(context.Context) app.MXReceiverStatus {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	state := rt.state
	if rt.pending && state != stateFailed {
		state = stateConnecting
	}
	return app.MXReceiverStatus{
		State:             state,
		Detail:            rt.detail,
		IncludedSupported: rt.edge != nil,
		SMTPAddr:          rt.smtpAddr,
		SessionAddr:       rt.sessionAddr,
		ActiveConnections: rt.activeConns,
	}
}

// Run reconciles until ctx is cancelled. It applies the persisted configuration
// on every wake or ticker, then shuts the receiver down. done is closed when Run
// returns so the caller can wait for the child and dialer to stop.
func (rt *mxRuntime) Run(ctx context.Context) {
	defer close(rt.done)
	t := time.NewTicker(mxReconcileInterval)
	defer t.Stop()
	if rt.edge != nil {
		// A child that exits cannot be restarted by this process. Mark the
		// failure immediately and force a re-apply so status stays honest.
		go rt.watchChildExit()
	}
	rt.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			rt.shutdown()
			return
		case <-rt.wake:
			rt.reconcile(ctx)
		case <-t.C:
			rt.reconcile(ctx)
		}
	}
}

// watchChildExit reports an included child's exit and forces the next reconcile
// to try again (it will fail, since the child cannot be respawned, so the status
// remains failed and honest).
func (rt *mxRuntime) watchChildExit() {
	<-rt.edge.Wait()
	err := rt.edge.ExitError()
	rt.mu.Lock()
	rt.applied = appliedSentinel
	rt.pending = false
	rt.mu.Unlock()
	msg := "included receiver exited"
	if err != nil {
		msg += ": " + err.Error()
	}
	rt.setStatus(stateFailed, msg, "", "")
	rt.log.Error("included receiver exited unexpectedly", "error", err)
	rt.Wake()
}

// Shutdown stops the runtime and waits (bounded by ctx) for it to finish. It is
// safe to call more than once. main calls it after the HTTP listeners stop and
// before the store closes, so the child and dialer are gone first.
func (rt *mxRuntime) Shutdown(ctx context.Context) error {
	rt.mu.Lock()
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-rt.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reconcile reads the persisted configuration and applies it when the revision
// changed. A read failure is recorded as a failed state rather than treated as
// "disabled", so an operator sees a broken database rather than a silently off
// receiver. A failed apply leaves rt.applied behind the read revision, so the
// next tick retries; a successful apply advances it.
func (rt *mxRuntime) reconcile(ctx context.Context) {
	settings, err := rt.svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		rt.setStatus(stateFailed, "cannot read MX receiver settings: "+err.Error(), "", "")
		rt.log.Error("mx receiver settings read failed", "error", err)
		return
	}
	rt.mu.Lock()
	unchanged := settings.Revision == rt.applied
	rt.mu.Unlock()
	if unchanged {
		// Already applied: only refresh the live view.
		rt.refreshLive(ctx, settings.Mode)
		return
	}
	rt.markPending(true)
	if err := rt.apply(ctx, settings); err != nil {
		// Leave applied behind so the ticker retries; the pending flag is
		// cleared so status reports the failure rather than a perpetual
		// "connecting".
		rt.mu.Lock()
		rt.pending = false
		rt.mu.Unlock()
		rt.log.Error("mx receiver apply failed", "mode", settings.Mode, "revision", settings.Revision, "error", err)
		rt.setStatus(stateFailed, err.Error(), "", "")
		return
	}
	rt.mu.Lock()
	rt.applied = settings.Revision
	rt.pending = false
	rt.mode = settings.Mode
	rt.mu.Unlock()
	// Snapshot the live view immediately so a caller does not read the previous
	// shape's state for up to a full reconcile interval.
	rt.refreshLive(ctx, settings.Mode)
}

// apply reconciles the live receiver to the desired settings. It is serialised
// by the single Run goroutine. It returns an error when the desired shape could
// not be brought up, so the caller can retry; a transient or misconfigured
// receiver never aborts the process.
func (rt *mxRuntime) apply(ctx context.Context, s app.MXReceiverSettings) error {
	// Every mode change drains and stops the previous shape first. The child is
	// deactivated (draining in-flight work) while the old private dialer is
	// still running, so a session pinned to the child finishes; only then is
	// the dialer stopped. This is what makes an active included reconfiguration
	// safe: Configure refuses an already-active child, so the drain must
	// complete first.
	rt.deactivateChild(ctx)
	rt.stopPrivate()

	switch s.Mode {
	case app.MXModeNone:
		rt.setStatus(stateDisabled, "", "", "")
	case app.MXModeIncluded:
		if err := rt.applyIncluded(ctx, s); err != nil {
			return err
		}
	case app.MXModeRemote:
		if err := rt.applyRemote(s); err != nil {
			return err
		}
	default:
		return errors.New("unknown receiver mode " + s.Mode)
	}
	return nil
}

func (rt *mxRuntime) applyIncluded(ctx context.Context, s app.MXReceiverSettings) error {
	if rt.edge == nil {
		// The included receiver is a separate-uid child; a rootless core cannot
		// provide the isolation, so the shape is unavailable here. This is not a
		// transient failure, so the caller records it and stops retrying.
		rt.setStatus(stateUnavailable, "included receiver requires the container to start as root; use remote mode", "", "")
		return nil
	}
	hostname := s.Hostname
	if hostname == "" {
		hostname = edgeHostname()
	}
	// The included edge must never advertise a message limit larger than the
	// core ingest path accepts. When the core cap is known, clamp to the smaller
	// of the operator's setting and the core cap; zero means "child default".
	effectiveMaxMessage := s.MaxMessageBytes
	if rt.maxMessageBytes > 0 && (effectiveMaxMessage == 0 || effectiveMaxMessage > rt.maxMessageBytes) {
		effectiveMaxMessage = rt.maxMessageBytes
	}
	st, err := rt.edge.Configure(ctx, control.Settings{
		Hostname: hostname,
		SMTP: mxagent.Config{
			Hostname: hostname,
			// Verification defaults on; an operator can only disable it
			// explicitly. Resolving nil here prevents the zero-value booleans
			// from silently turning SPF/DKIM/DMARC off.
			VerifySPF:       s.VerifySPFEnabled(),
			VerifyDKIM:      s.VerifyDKIMEnabled(),
			VerifyDMARC:     s.VerifyDMARCEnabled(),
			RequireTLS:      s.RequireTLSEnabled(),
			MaxMessageBytes: effectiveMaxMessage,
			MaxStagingBytes: s.MaxStagingBytes,
			MaxRecipients:   s.MaxRecipients,
			MaxConnections:  s.MaxConnections,
			DNSResolver:     s.DNSResolver,
			DNSTimeout:      seconds(s.DNSTimeoutSeconds),
			ReadTimeout:     seconds(s.ReadTimeoutSeconds),
			WriteTimeout:    seconds(s.WriteTimeoutSeconds),
			DataTimeout:     seconds(s.DataTimeoutSeconds),
		},
		// The STARTTLS pair travels as PEM over the private control channel; the
		// child cannot read the core's /data. The runtime builds the
		// certificate in memory (mxagent.Config.TLSCertificate).
		TLSCertificatePEM: s.SMTPTLSCert,
		TLSPrivateKeyPEM:  s.SMTPTLSKey,
		Secret:            s.BearerKey,
	})
	if err != nil {
		return errors.New("included receiver activation failed: " + err.Error())
	}
	rt.mu.Lock()
	rt.smtpAddr = st.SMTPAddr
	rt.sessionAddr = st.SessionAddr
	rt.activeConns = st.ActiveConnections
	rt.mu.Unlock()
	// The core dials the child's fixed loopback session endpoint with the same
	// key that was just sent over the control channel. The child being active is
	// not enough: readiness is the private dialer's real handshake, so the state
	// starts "connecting" until the manager reports the session ready.
	rt.startPrivate(mxdial.Config{
		DataDir:         rt.dataDir,
		MaxMessageBytes: rt.maxMessageBytes,
		MaxTransactions: rt.maxTransactions,
		ReceiverURL:     includedSessionURL,
		CoreKey:         s.BearerKey,
	})
	rt.setStatus(stateConnecting, "", st.SMTPAddr, st.SessionAddr)
	return nil
}

func (rt *mxRuntime) applyRemote(s app.MXReceiverSettings) error {
	tlsCfg, err := tlsConfigFromPEM(s.CA)
	if err != nil {
		return errors.New("invalid receiver CA: " + err.Error())
	}
	rt.startPrivate(mxdial.Config{
		DataDir:         rt.dataDir,
		MaxMessageBytes: rt.maxMessageBytes,
		MaxTransactions: rt.maxTransactions,
		ReceiverURL:     s.URL,
		CoreKey:         s.BearerKey,
		TLSConfig:       tlsCfg,
	})
	// Remote readiness is decided by the live session handshake, not by the
	// mere existence of a dialer; the state starts "connecting" until the
	// manager reports it.
	rt.setStatus(stateConnecting, "", "", "")
	return nil
}

// startPrivate stops any running private dialer and starts a new one. It never
// blocks: the dialer retries internally, so a receiver that is briefly down does
// not stall reconciliation.
func (rt *mxRuntime) startPrivate(cfg mxdial.Config) {
	cfg.AllowPrivateDestinations = true
	rt.stopPrivate()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	mgr := mxdial.New(rt.svc.PrivateMXBackend(), cfg)
	rt.mu.Lock()
	rt.privateCancel = cancel
	rt.privateDone = done
	rt.privateMgr = mgr
	rt.mu.Unlock()
	go func() {
		defer close(done)
		mgr.Run(ctx)
	}()
}

// stopPrivate cancels and waits for the running private dialer, if any.
func (rt *mxRuntime) stopPrivate() {
	rt.mu.Lock()
	cancel := rt.privateCancel
	done := rt.privateDone
	rt.privateCancel = nil
	rt.privateDone = nil
	rt.privateMgr = nil
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		rt.log.Warn("private mx dialer did not stop within bound")
	}
}

// deactivateChild stops the included child's listeners and leaves it in standby.
// It is a no-op when there is no child. A child failure is reported, never
// fatal.
func (rt *mxRuntime) deactivateChild(ctx context.Context) {
	if rt.edge == nil {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, launcher.StopTimeout)
	defer cancel()
	if _, err := rt.edge.Deactivate(dctx); err != nil {
		rt.log.Warn("included receiver deactivate", "error", err)
	}
	rt.mu.Lock()
	rt.smtpAddr = ""
	rt.sessionAddr = ""
	rt.activeConns = 0
	rt.mu.Unlock()
}

// refreshLive updates the cached live view for an already-applied
// configuration. It never blocks on the network for the UI path: Status() reads
// the cached values this writes. For the included shape it polls the child's
// lifecycle state (a listener failure it reported becomes "failed") and gates
// readiness on the private dialer's real handshake; for remote it reads the
// dialer's cached session state.
func (rt *mxRuntime) refreshLive(ctx context.Context, mode string) {
	rt.mu.Lock()
	pending := rt.pending
	mgr := rt.privateMgr
	rt.mu.Unlock()
	if pending {
		// A newer revision is still being applied; do not overwrite its state.
		return
	}
	switch mode {
	case app.MXModeIncluded:
		rt.refreshIncluded(ctx, mgr)
	case app.MXModeRemote:
		if mgr != nil {
			rt.applyConnectionStatus(mgr.ConnectionStatus())
		}
	}
}

// refreshIncluded polls the child's lifecycle state and combines it with the
// private dialer's handshake state. An error talking to the child means the
// child is gone or the control channel is broken, which is reported as failed
// rather than ignored. A live child is not sufficient for readiness: the session
// must have completed its handshake.
func (rt *mxRuntime) refreshIncluded(ctx context.Context, mgr *mxdial.Manager) {
	if rt.edge == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := rt.edge.Status(cctx)
	if err != nil {
		rt.setStatus(stateFailed, "included receiver status unavailable: "+err.Error(), "", "")
		return
	}
	rt.mu.Lock()
	rt.activeConns = st.ActiveConnections
	rt.mu.Unlock()
	if st.State == control.StateFailed {
		rt.setStatus(stateFailed, st.Error, st.SMTPAddr, st.SessionAddr)
		return
	}
	if mgr == nil {
		rt.setStatus(stateConnecting, "", st.SMTPAddr, st.SessionAddr)
		return
	}
	// The child is up; readiness is the loopback session's real handshake.
	cs := mgr.ConnectionStatus()
	if strings.EqualFold(strings.TrimSpace(cs.State), "ready") {
		rt.setStatus(stateActive, "", st.SMTPAddr, st.SessionAddr)
		return
	}
	reason := cs.Reason
	if reason == "" {
		reason = "connecting to included receiver"
	}
	rt.setStatus(stateConnecting, reason, st.SMTPAddr, st.SessionAddr)
}

// applyConnectionStatus maps the private dialer's real handshake state onto the
// receiver status. Only a reported "ready" is active; anything else (including
// an empty state) is connecting, so a receiver that has not completed a
// handshake is never shown as ready.
func (rt *mxRuntime) applyConnectionStatus(st mxdial.Status) {
	switch strings.ToLower(strings.TrimSpace(st.State)) {
	case "ready":
		rt.mu.Lock()
		rt.smtpAddr = st.SMTPHostname
		rt.mu.Unlock()
		rt.setStatus(stateActive, "", st.SMTPHostname, "")
	default:
		reason := st.Reason
		if reason == "" {
			reason = "connecting to receiver"
		}
		rt.setStatus(stateConnecting, reason, "", "")
	}
}

func (rt *mxRuntime) markPending(pending bool) {
	rt.mu.Lock()
	rt.pending = pending
	rt.mu.Unlock()
}

func (rt *mxRuntime) setStatus(state, detail, smtpAddr, sessionAddr string) {
	rt.mu.Lock()
	rt.state = state
	rt.detail = detail
	if smtpAddr != "" {
		rt.smtpAddr = smtpAddr
	}
	if sessionAddr != "" {
		rt.sessionAddr = sessionAddr
	}
	rt.mu.Unlock()
}

// shutdown drains the included child first (letting the still-running private
// dialer finish in-flight work), then stops the dialer. It is called once when
// Run's context is cancelled.
func (rt *mxRuntime) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), launcher.StopTimeout)
	defer cancel()
	rt.deactivateChild(ctx)
	rt.stopPrivate()
}

// seconds converts a positive whole-second override into a duration; zero means
// "use the child default".
func seconds(v int) time.Duration {
	if v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Second
}

// tlsConfigFromPEM builds a verified TLS configuration from an operator PEM CA
// bundle stored with the settings. An empty bundle means system roots only. A
// non-PEM bundle is an error, never a silent skip of verification.
func tlsConfigFromPEM(pemData string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return cfg, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pemData)) {
		return nil, errors.New("CA bundle contains no certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// startMXRuntime wires the process-owned MX controller into the application. It
// is called after the database and service exist, and before serving. edge may
// be nil when the process is rootless. The returned runtime owns a cancellable
// context: main calls Shutdown to stop it and wait for the child and dialer.
func startMXRuntime(svc *app.Service, edge includedEdge, log *slog.Logger) *mxRuntime {
	rt := newMXRuntime(svc, edge, log)
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	svc.MXRuntime = rt
	go rt.Run(ctx)
	return rt
}

var _ app.MXReceiverRuntime = (*mxRuntime)(nil)
