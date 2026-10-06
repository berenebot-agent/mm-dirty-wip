// These tests live in package main rather than under tests/unit because the
// controller (mxRuntime, reconcile, apply) is unexported and its behaviour is
// the process lifecycle itself: the black-box tests/unit convention cannot reach
// it. The child is injected through the includedEdge interface, so the suite
// exercises the drain-before-configure ordering, retry and state reporting
// without spawning a process; the real child wiring is covered by the launcher
// standby integration test.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// newTestService builds a service on a temporary store for the cmd/server
// reconciliation tests.
func newTestService(t *testing.T) *app.Service {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "Runtime", "runtime@example.test", "unused-test-hash", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "runtime.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(context.Background(), u.AccountID, d.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestMXRuntimeLastDomainStandbyAndRestart(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	edge := &fakeEdge{}
	rt := newMXRuntime(svc, edge, testLogger())
	defer rt.shutdown()
	svc.MXRuntime = rt
	saved, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded, Hostname: "mx.test"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.Store.GetUserByEmail(ctx, "runtime@example.test")
	if err != nil {
		t.Fatal(err)
	}
	domains, err := svc.Store.ListDomains(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	d := domains[0]
	if err := svc.Store.DeleteDomainReceivingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	// Persisted Included settings alone must not activate anything at startup.
	rt.reconcile(ctx)
	if n, _, active := edge.calls(); n != 0 || active || rt.privateMgr != nil {
		t.Fatal("unused Included receiver started")
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if n, _, active := edge.calls(); n != 1 || !active || rt.privateMgr == nil {
		t.Fatal("first MX domain did not activate receiver")
	}
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "Other", "other-runtime@example.test", "unused-test-password", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Store.CreateDomain(ctx, other.AccountID, "other.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, other.AccountID, second.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.DeleteDomainReceivingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if n, _, active := edge.calls(); n != 1 || !active {
		t.Fatal("receiver stopped while another account still uses MX")
	}
	if err := svc.Store.DeleteDomainReceivingConfig(ctx, other.AccountID, second.ID); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if _, _, active := edge.calls(); active || rt.privateMgr != nil || rt.Status(ctx).State != stateDisabled {
		t.Fatal("last MX domain removal did not stop receiver and dialer")
	}
	settings, err := svc.GetMXReceiverSettings(ctx)
	if err != nil || settings.Revision != saved.Revision || settings.Hostname != "mx.test" {
		t.Fatal("standby discarded saved settings")
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if n, _, active := edge.calls(); n != 2 || !active || rt.privateMgr == nil {
		t.Fatal("MX reselection did not restart receiver")
	}
	child, err := svc.Store.CreateDomain(ctx, u.AccountID, "child.runtime.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetDomainParent(ctx, u.AccountID, child.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetDomainInheritance(ctx, u.AccountID, child.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if inherited, err := svc.Store.GetDomain(ctx, u.AccountID, child.ID); err != nil || inherited.ReceivingProvider != "mx" {
		t.Fatalf("inherited MX not resolved: %+v %v", inherited, err)
	}
	rt.reconcile(ctx)
	if _, _, active := edge.calls(); !active {
		t.Fatal("inherited domain stopped receiver")
	}
	// Switching the ancestor to a webhook removes the inherited MX path too.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", nil, false); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if _, _, active := edge.calls(); active || rt.privateMgr != nil {
		t.Fatal("provider switch left unused receiver running")
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, other.AccountID, second.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if err := svc.Store.DeleteDomain(ctx, other.AccountID, second.ID); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if _, _, active := edge.calls(); active || rt.privateMgr != nil {
		t.Fatal("last domain deletion left receiver running")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeEdge is an in-memory included child. It mirrors the real child's contract
// that Configure refuses while active, recording every call so a test can assert
// the controller drains before reconfiguring.
type fakeEdge struct {
	mu         sync.Mutex
	active     bool
	configure  []control.Settings
	deactivate int
	cfgErr     error
	waitCh     chan struct{}
}

func (f *fakeEdge) Configure(_ context.Context, s control.Settings) (control.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cfgErr != nil {
		return control.Status{}, f.cfgErr
	}
	if f.active {
		return control.Status{State: control.StateActive}, errors.New("runtime: receiver already active")
	}
	f.active = true
	f.configure = append(f.configure, s)
	return control.Status{State: control.StateActive, SMTPAddr: ":2525", SessionAddr: "127.0.0.1:8443"}, nil
}

func (f *fakeEdge) Deactivate(context.Context) (control.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = false
	f.deactivate++
	return control.Status{State: control.StateStandby}, nil
}

func (f *fakeEdge) Status(context.Context) (control.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active {
		return control.Status{State: control.StateActive, SMTPAddr: ":2525"}, nil
	}
	return control.Status{State: control.StateStandby}, nil
}

// Wait never reports an exit unless a test closes waitCh. The real child runs
// for the process's whole life.
func (f *fakeEdge) Wait() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waitCh == nil {
		f.waitCh = make(chan struct{})
	}
	return f.waitCh
}

func (f *fakeEdge) ExitError() error { return nil }

// exit simulates the child process exiting.
func (f *fakeEdge) exit() {
	f.mu.Lock()
	if f.waitCh == nil {
		f.waitCh = make(chan struct{})
	}
	ch := f.waitCh
	f.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (f *fakeEdge) calls() (configures, deactivates int, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configure), f.deactivate, f.active
}

// TestImportLegacyMXSettings covers the one-time environment transition.
func TestImportLegacyMXSettings(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	// No environment and no stored row: nothing is written, so a later
	// deliberate environment change can still be imported.
	importLegacyMXSettings(ctx, svc, config.Config{}, testLogger())
	if initialized, err := svc.Store.MXSettingsInitialized(ctx); err != nil || initialized {
		t.Fatalf("unexpected initialization: %v, %v", initialized, err)
	}

	// A legacy embedded environment imports as included with a generated key
	// and preserves an explicit verification choice.
	verifyDKIM := false
	cfg := config.Config{MXImport: config.MXReceiverImport{Set: true, Mode: "included", VerifyDKIM: &verifyDKIM}}
	importLegacyMXSettings(ctx, svc, cfg, testLogger())
	got, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != app.MXModeIncluded || got.BearerKey == "" || !got.KeyConfigured {
		t.Fatalf("imported included = %+v", got)
	}
	if !got.VerifySPFEnabled() || got.VerifyDKIMEnabled() || !got.VerifyDMARCEnabled() {
		t.Fatalf("imported verification toggles = %+v", got)
	}

	// A second run with a different environment is ignored: the stored row wins,
	// even when the environment is partial or stale.
	cfg2 := config.Config{MXImport: config.MXReceiverImport{Set: true, Mode: "remote", ReceiverURL: "https://r.example", CoreKey: "new-key"}, MXReceiverURL: "https://r.example", MXCoreKey: "new-key"}
	importLegacyMXSettings(ctx, svc, cfg2, testLogger())
	again, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Mode != app.MXModeIncluded || again.BearerKey != got.BearerKey {
		t.Fatalf("environment overrode stored settings: %+v", again)
	}
}

// TestImportLegacyIncludedTunablesAndTLS verifies the one-time import carries
// every legacy included SMTP tunable and reads the STARTTLS files into the
// persisted PEM pair.
func TestImportLegacyIncludedTunablesAndTLS(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	certPEM, keyPEM := tlsTestPair(t)
	certFile := writeTempFile(t, certPEM)
	keyFile := writeTempFile(t, keyPEM)
	requireTLS := true
	verifyDKIM := false

	importLegacyMXSettings(ctx, svc, config.Config{MXImport: config.MXReceiverImport{
		Set: true, Mode: "included",
		Hostname: "legacy.mx", MaxMessageBytes: 2 << 20, MaxStagingBytes: 5 << 20,
		MaxRecipients: 40, MaxConnections: 90, RequireTLS: &requireTLS, VerifyDKIM: &verifyDKIM,
		DNSResolver: "1.1.1.1:53", DNSTimeoutSeconds: 9, ReadTimeoutSeconds: 11,
		WriteTimeoutSeconds: 12, DataTimeoutSeconds: 300,
		TLSCertFile: certFile, TLSKeyFile: keyFile,
	}}, testLogger())

	got, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "legacy.mx" || got.MaxMessageBytes != 2<<20 || got.MaxStagingBytes != 5<<20 ||
		got.MaxRecipients != 40 || got.MaxConnections != 90 {
		t.Fatalf("limits not imported: %+v", got)
	}
	if !got.RequireTLSEnabled() || got.VerifyDKIMEnabled() || got.DNSResolver != "1.1.1.1:53" ||
		got.DNSTimeoutSeconds != 9 || got.ReadTimeoutSeconds != 11 || got.WriteTimeoutSeconds != 12 || got.DataTimeoutSeconds != 300 {
		t.Fatalf("tunables not imported: %+v", got)
	}
	if got.SMTPTLSCert != strings.TrimSpace(certPEM) || got.SMTPTLSKey != strings.TrimSpace(keyPEM) || !got.SMTPTLSKeyConfigured {
		t.Fatal("legacy STARTTLS pair not imported")
	}
}

// TestImportLegacyUnusableTLSFilesAbortsImport verifies a partial or unreadable
// legacy STARTTLS pair aborts the whole import rather than silently persisting a
// receiver without TLS: no row is written, so the core keeps running
// unconfigured for the operator to fix.
func TestImportLegacyUnusableTLSFilesAbortsImport(t *testing.T) {
	ctx := context.Background()
	for name, imp := range map[string]config.MXReceiverImport{
		"missing files":    {Set: true, Mode: "included", TLSCertFile: "/nonexistent/cert.pem", TLSKeyFile: "/nonexistent/key.pem"},
		"cert only":        {Set: true, Mode: "included", TLSCertFile: "/nonexistent/cert.pem"},
		"key only":         {Set: true, Mode: "included", TLSKeyFile: "/nonexistent/key.pem"},
		"cert real no key": {Set: true, Mode: "included", TLSCertFile: writeTempFile(t, "x")},
	} {
		t.Run(name, func(t *testing.T) {
			svc := newTestService(t)
			importLegacyMXSettings(ctx, svc, config.Config{MXImport: imp}, testLogger())
			if initialized, err := svc.Store.MXSettingsInitialized(ctx); err != nil || initialized {
				t.Fatalf("unusable TLS should leave no row: initialized=%v err=%v", initialized, err)
			}
		})
	}
}

// TestImportLegacyPartialRemoteRejectedOnce verifies a partial remote
// environment is rejected only when it is actually imported, and never blocks a
// deployment whose settings are already persisted.
func TestImportLegacyPartialRemoteRejectedOnce(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	// A remote mode with no key must not create a row.
	importLegacyMXSettings(ctx, svc, config.Config{MXImport: config.MXReceiverImport{Set: true, Mode: "remote", ReceiverURL: "https://r.example"}}, testLogger())
	if initialized, err := svc.Store.MXSettingsInitialized(ctx); err != nil || initialized {
		t.Fatalf("partial remote import created a row: %v, %v", initialized, err)
	}
}

// TestTLSConfigFromPEM verifies the runtime TLS hook: system roots by default,
// a custom pool for a valid bundle, and a hard error for non-PEM input.
func TestTLSConfigFromPEM(t *testing.T) {
	cfg, err := tlsConfigFromPEM("")
	if err != nil || cfg.RootCAs != nil {
		t.Fatalf("empty CA = %+v, %v", cfg, err)
	}
	if _, err := tlsConfigFromPEM("not a certificate"); err == nil {
		t.Fatal("non-PEM CA accepted")
	}
	block := &pem.Block{Type: "CERTIFICATE", Bytes: []byte("not really a cert")}
	if _, err := tlsConfigFromPEM(string(pem.EncodeToMemory(block))); err == nil {
		t.Fatal("undecodable certificate accepted")
	}
	certPEM := testSelfSignedPEM(t)
	cfg, err = tlsConfigFromPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil || len(cfg.RootCAs.Subjects()) == 0 {
		t.Fatal("valid CA did not populate RootCAs")
	}
}

// TestMXRuntimeReconcileStates verifies the generation-based reconciliation:
// unconfigured is disabled, included is unavailable in a rootless process, and
// remote goes to connecting (never a false active) and starts the private
// dialer.
func TestMXRuntimeReconcileStates(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	rt := newMXRuntime(svc, nil, testLogger())
	svc.MXRuntime = rt

	// Unconfigured.
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateDisabled || st.IncludedSupported {
		t.Fatalf("unconfigured status = %+v", st)
	}

	// Included requires a child; with none, it is reported unavailable rather
	// than failing the process.
	included, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded})
	if err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateUnavailable {
		t.Fatalf("rootless included status = %+v", st)
	}

	// Switch to remote: the dialer starts but must not be reported active before
	// a live handshake.
	remote, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://receiver.example", BearerKey: "k", Revision: included.Revision})
	if err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateConnecting {
		t.Fatalf("remote status before handshake = %+v", st)
	}
	rt.mu.Lock()
	running := rt.privateMgr != nil
	rt.mu.Unlock()
	if !running {
		t.Fatal("remote mode did not start the private dialer")
	}
	// A refresh with no ConnectionStatus API keeps it connecting, never active.
	rt.refreshLive(ctx, app.MXModeRemote)
	if st := rt.Status(ctx); st.State != stateConnecting {
		t.Fatalf("remote status after refresh = %+v", st)
	}

	// Clearing stops the dialer and reports disabled.
	if err := svc.ClearMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, remote.Revision); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateDisabled {
		t.Fatalf("cleared status = %+v", st)
	}
	rt.mu.Lock()
	running = rt.privateMgr != nil
	rt.mu.Unlock()
	if running {
		t.Fatal("cleared mode left the private dialer running")
	}
	rt.shutdown()
}

// TestMXRuntimeActiveConfigUpdate verifies a settings change while the included
// child is already active drains it before reconfiguring, so Configure never
// fails with "already active", and that the private dialer is stopped only after
// the drain.
func TestMXRuntimeActiveConfigUpdate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	edge := &fakeEdge{}
	rt := newMXRuntime(svc, edge, testLogger())
	svc.MXRuntime = rt

	first, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded, Hostname: "mx1.test"})
	if err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	configures, deactivatesBefore, active := edge.calls()
	if configures != 1 || !active {
		t.Fatalf("first configure: configures=%d active=%v", configures, active)
	}

	// Change a limit while active: the controller must drain before configuring.
	if _, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded, Hostname: "mx2.test", MaxRecipients: 25, Revision: first.Revision}); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	configures, deactivatesAfter, active := edge.calls()
	if configures != 2 || deactivatesAfter != deactivatesBefore+1 || !active {
		t.Fatalf("reconfigure: configures=%d deactivates=%d->%d active=%v", configures, deactivatesBefore, deactivatesAfter, active)
	}
	// Readiness is the private session handshake, which a fake child cannot
	// complete, so the honest state is "connecting" even though the child is
	// active.
	if st := rt.Status(ctx); st.State != stateConnecting {
		t.Fatalf("status after reconfigure = %+v", st)
	}
	// The verification defaults were carried through to the child.
	edge.mu.Lock()
	last := edge.configure[len(edge.configure)-1]
	edge.mu.Unlock()
	if !last.SMTP.VerifySPF || !last.SMTP.VerifyDKIM || !last.SMTP.VerifyDMARC {
		t.Fatalf("verification toggles not defaulted on: %+v", last.SMTP)
	}
	rt.shutdown()
}

// TestMXRuntimeForwardsIncludedSMTPFields verifies every persisted included SMTP
// field, including the STARTTLS PEM pair, reaches the child through the control
// Settings.
func TestMXRuntimeForwardsIncludedSMTPFields(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	edge := &fakeEdge{}
	rt := newMXRuntime(svc, edge, testLogger())
	svc.MXRuntime = rt
	certPEM, keyPEM := tlsTestPair(t)
	requireTLS := true
	verifyDKIM := false

	if _, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{
		Mode: app.MXModeIncluded, Hostname: "mx.test",
		MaxMessageBytes: 2 << 20, MaxStagingBytes: 4 << 20, MaxRecipients: 30, MaxConnections: 70,
		RequireTLS: &requireTLS, VerifyDKIM: &verifyDKIM,
		DNSResolver: "9.9.9.9:53", DNSTimeoutSeconds: 5, ReadTimeoutSeconds: 6, WriteTimeoutSeconds: 7, DataTimeoutSeconds: 300,
		SMTPTLSCert: strings.TrimSpace(certPEM), SMTPTLSKey: strings.TrimSpace(keyPEM),
	}); err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)

	edge.mu.Lock()
	last := edge.configure[len(edge.configure)-1]
	edge.mu.Unlock()
	if last.Hostname != "mx.test" || last.SMTP.Hostname != "mx.test" {
		t.Fatalf("hostname not forwarded: %+v", last)
	}
	if !last.SMTP.RequireTLS || last.SMTP.VerifyDKIM || !last.SMTP.VerifySPF || !last.SMTP.VerifyDMARC {
		t.Fatalf("toggles not forwarded: %+v", last.SMTP)
	}
	if last.SMTP.MaxMessageBytes != 2<<20 || last.SMTP.MaxStagingBytes != 4<<20 || last.SMTP.MaxRecipients != 30 || last.SMTP.MaxConnections != 70 {
		t.Fatalf("limits not forwarded: %+v", last.SMTP)
	}
	if last.SMTP.DNSResolver != "9.9.9.9:53" || last.SMTP.DNSTimeout != 5*time.Second ||
		last.SMTP.ReadTimeout != 6*time.Second || last.SMTP.WriteTimeout != 7*time.Second || last.SMTP.DataTimeout != 300*time.Second {
		t.Fatalf("dns/timeouts not forwarded: %+v", last.SMTP)
	}
	if last.TLSCertificatePEM != strings.TrimSpace(certPEM) || last.TLSPrivateKeyPEM != strings.TrimSpace(keyPEM) {
		t.Fatal("STARTTLS PEM pair not forwarded over the control channel")
	}
	rt.shutdown()
}

// TestMXRuntimeRetriesFailedActivation verifies a failed activation does not
// advance the applied revision, so the next tick retries, and that success then
// advances it.
func TestMXRuntimeRetriesFailedActivation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	edge := &fakeEdge{cfgErr: errors.New("boom")}
	rt := newMXRuntime(svc, edge, testLogger())
	svc.MXRuntime = rt

	saved, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded})
	if err != nil {
		t.Fatal(err)
	}
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateFailed {
		t.Fatalf("failed activation status = %+v", st)
	}
	rt.mu.Lock()
	applied := rt.applied
	rt.mu.Unlock()
	if applied == saved.Revision {
		t.Fatal("failed activation advanced the applied revision")
	}

	// Recover and retry: the same revision now applies and advances.
	edge.mu.Lock()
	edge.cfgErr = nil
	edge.mu.Unlock()
	rt.reconcile(ctx)
	if st := rt.Status(ctx); st.State != stateConnecting {
		t.Fatalf("recovered status = %+v", st)
	}
	// The child reports active; a ready handshake would make it active. Assert
	// the mapping directly since the fake dialer never completes a handshake.
	rt.applyConnectionStatus(mxdial.Status{State: "ready", SMTPHostname: "mx.test"})
	if st := rt.Status(ctx); st.State != stateActive {
		t.Fatalf("ready handshake should be active: %+v", st)
	}
	rt.mu.Lock()
	applied = rt.applied
	rt.mu.Unlock()
	if applied != saved.Revision {
		t.Fatalf("applied revision = %d, want %d", applied, saved.Revision)
	}
	rt.shutdown()
}

// TestMXRuntimeChildExitForcesRetry verifies an included child exit is reported
// as failed and forces a re-apply (the child cannot be respawned, so the retry
// fails and the status stays honestly failed).
func TestMXRuntimeChildExitForcesRetry(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	edge := &fakeEdge{}
	rt := newMXRuntime(svc, edge, testLogger())
	svc.MXRuntime = rt

	saved, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, app.MXReceiverInput{Mode: app.MXModeIncluded})
	if err != nil {
		t.Fatal(err)
	}
	// Drive the exit watcher and the reconciliation loop directly.
	rt.reconcile(ctx)
	rt.mu.Lock()
	if rt.applied != saved.Revision {
		rt.mu.Unlock()
		t.Fatalf("applied = %d, want %d", rt.applied, saved.Revision)
	}
	rt.mu.Unlock()

	go rt.watchChildExit()
	edge.exit()
	// The watcher sets the sentinel and wakes; wait for the sentinel.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rt.mu.Lock()
		applied := rt.applied
		rt.mu.Unlock()
		if applied == appliedSentinel {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child exit did not set the retry sentinel")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := rt.Status(ctx); st.State != stateFailed {
		t.Fatalf("child exit status = %+v", st)
	}
	// A reconcile retries (Configure still succeeds on the fake, but the real
	// child would fail); the key point is the sentinel forced the attempt.
	rt.reconcile(ctx)
	rt.mu.Lock()
	retried := rt.applied
	rt.mu.Unlock()
	if retried != saved.Revision {
		t.Fatalf("retry did not re-apply: applied = %d", retried)
	}
	rt.shutdown()
}

// tlsTestPair returns a valid self-signed certificate and PKCS#8 private key as
// PEM text, for the STARTTLS import tests.
func tlsTestPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// writeTempFile writes content to a temp file and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.pem")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testSelfSignedPEM returns a syntactically valid self-signed certificate PEM,
// used to exercise the CA pool path.
func testSelfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
