package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// remoteMXReconcileInterval bounds how long an account Remote MX change can take
// to reach the receiver when a Wake is missed, and how long a failed activation
// waits before it is retried.
const remoteMXReconcileInterval = 30 * time.Second

// remoteMXAccount is one running per-account single-mode dialer.
type remoteMXAccount struct {
	cancel context.CancelFunc
	done   chan struct{}
	mgr    *mxdial.Manager
	// revision is the applied account_mx_receivers revision.
	revision int64
}

// remoteMXRuntime owns one mxdial.Manager per account whose Remote MX receiver is
// in use by at least one domain. Each account receiver is a standalone Dial MX
// receiver running in single mode (bearer key), so the manager for an account
// connects one session and the receiver authorizes any domain on it; the core
// keeps per-recipient account/domain authorization through the remotemx provider.
//
// Reconciliation is generation-based and per account: it reads the current
// "in use" account set and each account's persisted revision, starts a manager
// for a newly-used account, restarts one whose revision changed, and stops one
// that is no longer used. A receiver failure never aborts the process; it is
// reported in status and retried on the next tick.
type remoteMXRuntime struct {
	log *slog.Logger
	svc *app.Service

	mu       sync.Mutex
	accounts map[string]*remoteMXAccount
	stopping bool

	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
}

func newRemoteMXRuntime(svc *app.Service, log *slog.Logger) *remoteMXRuntime {
	return &remoteMXRuntime{
		log:      log,
		svc:      svc,
		accounts: map[string]*remoteMXAccount{},
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// WakeRemoteMX implements app.AccountMXReceiverRuntime. It never blocks.
func (rt *remoteMXRuntime) WakeRemoteMX() {
	select {
	case rt.wake <- struct{}{}:
	default:
	}
}

// RemoteMXStatus implements app.AccountMXReceiverRuntime. It reports the live
// session state of one account's receiver from the running dialer, or a
// disabled/unknown state when no dialer is running for it.
func (rt *remoteMXRuntime) RemoteMXStatus(_ context.Context, accountID string) app.AccountMXReceiverStatus {
	rt.mu.Lock()
	acct := rt.accounts[accountID]
	rt.mu.Unlock()
	st := app.AccountMXReceiverStatus{State: app.MXStateDisabled}
	if acct == nil {
		return st
	}
	st.Configured = true
	st.Revision = acct.revision
	live := acct.mgr.ConnectionStatus()
	switch strings.ToLower(strings.TrimSpace(live.State)) {
	case "ready":
		st.State = app.MXStateActive
	case "connecting", "":
		st.State = app.MXStateConnecting
	default:
		st.State = app.MXStateConnecting
		st.Detail = live.Reason
	}
	return st
}

// Run reconciles until ctx is cancelled.
func (rt *remoteMXRuntime) Run(ctx context.Context) {
	defer close(rt.done)
	t := time.NewTicker(remoteMXReconcileInterval)
	defer t.Stop()
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

// reconcile brings the set of running per-account dialers in line with the
// accounts that currently route a domain to Remote MX.
func (rt *remoteMXRuntime) reconcile(ctx context.Context) {
	inUse, err := rt.svc.Store.ListRemoteMXAccounts(ctx)
	if err != nil {
		rt.log.Error("remote mx account list failed", "error", err)
		return
	}
	want := map[string]bool{}
	for _, accountID := range inUse {
		settings, serr := rt.svc.AccountMXReceiverSettingsForRuntime(ctx, accountID)
		if serr != nil {
			rt.log.Error("remote mx settings read failed", "account_id", accountID, "error", serr)
			continue
		}
		// An account that routes to Remote MX but has no (or a cleared) receiver
		// is not dialed; its domains simply fail closed until it is configured.
		if settings.URL == "" || !settings.KeyConfigured {
			continue
		}
		want[accountID] = true
		rt.applyAccount(accountID, settings)
	}
	// Stop dialers for accounts that no longer need one.
	rt.mu.Lock()
	var toStop []*remoteMXAccount
	for accountID, acct := range rt.accounts {
		if !want[accountID] {
			toStop = append(toStop, acct)
			delete(rt.accounts, accountID)
		}
	}
	rt.mu.Unlock()
	for _, acct := range toStop {
		stopRemoteMXAccount(rt.log, acct)
	}
}

// applyAccount starts or restarts the dialer for one account when its revision
// changed.
func (rt *remoteMXRuntime) applyAccount(accountID string, settings app.AccountMXReceiver) {
	rt.mu.Lock()
	existing := rt.accounts[accountID]
	if existing != nil && existing.revision == settings.Revision {
		rt.mu.Unlock()
		return
	}
	rt.mu.Unlock()

	tlsCfg, err := tlsConfigFromPEM(settings.CA)
	if err != nil {
		rt.log.Error("remote mx invalid CA", "account_id", accountID, "error", err)
		return
	}
	cfg := mxdial.Config{
		DataDir:         rt.svc.Config.DataDir,
		MaxMessageBytes: rt.svc.Config.MaxMessageBytes,
		MaxTransactions: rt.svc.Config.InboundConcurrency,
		ReceiverURL:     settings.URL,
		CoreKey:         settings.BearerKey,
		TLSConfig:       tlsCfg,
		// The account may opt into a private/LAN receiver, but the operator's
		// global policy wins: when outbound is confined to the public internet
		// (RequirePublicOutbound), the tenant cannot re-enable private
		// destinations on their own.
		AllowPrivateDestinations: settings.AllowPrivate && !rt.svc.Config.RequirePublicOutbound(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	mgr := mxdial.New(rt.svc.RemoteMXBackend(), cfg)

	rt.mu.Lock()
	if rt.stopping {
		rt.mu.Unlock()
		cancel()
		close(done)
		return
	}
	old := rt.accounts[accountID]
	rt.accounts[accountID] = &remoteMXAccount{cancel: cancel, done: done, mgr: mgr, revision: settings.Revision}
	rt.mu.Unlock()
	if old != nil {
		stopRemoteMXAccount(rt.log, old)
	}
	go func() {
		defer close(done)
		mgr.Run(ctx)
	}()
}

// shutdown stops every running account dialer. It is called once when Run's
// context is cancelled.
func (rt *remoteMXRuntime) shutdown() {
	rt.mu.Lock()
	rt.stopping = true
	accts := make([]*remoteMXAccount, 0, len(rt.accounts))
	for _, acct := range rt.accounts {
		accts = append(accts, acct)
	}
	rt.accounts = map[string]*remoteMXAccount{}
	rt.mu.Unlock()
	for _, acct := range accts {
		stopRemoteMXAccount(rt.log, acct)
	}
}

// Shutdown stops the runtime and waits (bounded by ctx) for it to finish. It is
// safe to call more than once.
func (rt *remoteMXRuntime) Shutdown(ctx context.Context) error {
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

func stopRemoteMXAccount(log *slog.Logger, acct *remoteMXAccount) {
	if acct == nil {
		return
	}
	acct.cancel()
	select {
	case <-acct.done:
	case <-time.After(5 * time.Second):
		if log != nil {
			log.Warn("remote mx dialer did not stop within bound")
		}
	}
}

// startRemoteMXRuntime wires the per-account Remote MX controller into the
// application.
func startRemoteMXRuntime(svc *app.Service, log *slog.Logger) *remoteMXRuntime {
	rt := newRemoteMXRuntime(svc, log)
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
	svc.RemoteMXRuntime = rt
	go rt.Run(ctx)
	return rt
}

var _ app.AccountMXReceiverRuntime = (*remoteMXRuntime)(nil)
