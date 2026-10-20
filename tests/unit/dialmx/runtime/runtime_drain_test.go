package runtime_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	receiverruntime "github.com/dellarb/mailmoose/dialmx/runtime"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
	"github.com/emersion/go-smtp"
)

// drainBackend is a fake core that can hold an ingest open until released, so a
// test can prove an in-flight SMTP transaction commits through the core session
// during deactivation rather than being cancelled early.
type drainBackend struct {
	mu       sync.Mutex
	stored   []string
	raw      string
	ingestIn chan struct{} // signalled when an ingest begins
	release  chan struct{} // closed by the test to let the ingest finish
	blocked  bool
}

func (b *drainBackend) Domains(context.Context) ([]mxdial.Domain, error) { return nil, nil }

func (b *drainBackend) Resolve(_ context.Context, _ string, rs []string) (mxwire.ResolveResponse, error) {
	x := mxwire.ResolveResponse{MachineCode: mxwire.CodeOK}
	for _, a := range rs {
		x.Results = append(x.Results, mxwire.ResolveRecipient{Recipient: a, Accept: true})
	}
	return x, nil
}

func (b *drainBackend) Ingest(_ context.Context, _ []string, m mxwire.IngestMetadata, path, _ string) (mxwire.IngestResponse, error) {
	if b.blocked {
		select {
		case b.ingestIn <- struct{}{}:
		default:
		}
		<-b.release
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.raw = string(raw)
	x := mxwire.IngestResponse{MachineCode: mxwire.CodeOK, MessageID: "msg"}
	for _, a := range m.Recipients {
		b.stored = append(b.stored, a)
		x.PerRecipient = append(x.PerRecipient, mxwire.RecipientIngestResult{
			Recipient: a, MachineCode: mxwire.CodeOK, Disposition: mxwire.DispositionStored, MessageID: "msg",
		})
	}
	return x, nil
}

// TestDeactivateDrainsInFlightMailThroughCore proves the deliberate drain: an
// SMTP DATA transaction that is mid-ingest when Deactivate is requested still
// commits through the still-live core session and the sender receives 250.
// Sessions are stopped only after the SMTP drain, so pinned delivery is never
// cancelled early.
func TestDeactivateDrainsInFlightMailThroughCore(t *testing.T) {
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	cfg := receiverruntime.Config{
		Hostname:     "mx.test",
		Secret:       "core-key",
		SessionAddr:  "127.0.0.1:0",
		DrainTimeout: 10 * time.Second,
		SMTP: mxagent.Config{
			Hostname:        "mx.test",
			ListenAddr:      "127.0.0.1:0",
			MaxMessageBytes: 1 << 20,
			MaxRecipients:   10,
			MaxConnections:  16,
			DataTimeout:     10 * time.Second,
			DNSTimeout:      2 * time.Second,
		},
	}
	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	be := &drainBackend{ingestIn: make(chan struct{}, 1), release: make(chan struct{}), blocked: true}
	mcfg := mxdial.Config{
		DataDir:                  t.TempDir(),
		CoreKey:                  "core-key",
		ReceiverURL:              "http://" + st.SessionAddr,
		AllowPrivateDestinations: true,
		ReconcileInterval:        20 * time.Millisecond,
	}
	manager := mxdial.New(be, mcfg)
	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan struct{})
	go func() { defer close(managerDone); manager.Run(ctx) }()
	defer func() { cancel(); <-managerDone }()

	// Wait for the private session to be ready before sending mail.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && manager.ConnectionStatus().State != "ready" {
		time.Sleep(20 * time.Millisecond)
	}
	if got := manager.ConnectionStatus().State; got != "ready" {
		t.Fatalf("core session not ready: %q", got)
	}

	// Send the message in a goroutine; the backend holds the ingest open.
	sendResult := make(chan error, 1)
	go func() { sendResult <- sendMessage(st.SMTPAddr, "alice@example.test") }()

	// Wait until the ingest is genuinely in flight.
	select {
	case <-be.ingestIn:
	case <-time.After(5 * time.Second):
		t.Fatal("ingest never started")
	}

	// Deactivate while the ingest is held. Phase 1 must wait for the SMTP drain,
	// so Deactivate must not return until we release the ingest.
	deactivated := make(chan error, 1)
	go func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dcancel()
		_, derr := rt.Deactivate(dctx)
		deactivated <- derr
	}()

	select {
	case err := <-deactivated:
		t.Fatalf("deactivate returned before the in-flight ingest finished: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Still draining, as required.
	}

	// Let the core commit, then the drain must finish.
	close(be.release)
	if err := <-sendResult; err != nil {
		t.Fatalf("SMTP send failed (pinned delivery was cut early): %v", err)
	}
	if err := <-deactivated; err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	be.mu.Lock()
	stored := append([]string(nil), be.stored...)
	raw := be.raw
	be.mu.Unlock()
	if len(stored) != 1 || !strings.Contains(raw, "in-flight body") {
		t.Fatalf("message not committed before standby: stored=%v raw=%q", stored, raw)
	}
	if got := rt.Status().State; got != control.StateStandby {
		t.Fatalf("state after deactivate %q", got)
	}
}

// TestDeactivateReportsIncompleteDrain proves that when the SMTP drain bound
// elapses with a transaction still in flight, Deactivate surfaces
// ErrDrainIncomplete (and still returns the child to standby) rather than
// claiming a clean drain.
func TestDeactivateReportsIncompleteDrain(t *testing.T) {
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	cfg := receiverruntime.Config{
		Hostname:     "mx.test",
		Secret:       "core-key",
		SessionAddr:  "127.0.0.1:0",
		DrainTimeout: 100 * time.Millisecond, // shorter than the held ingest
		SMTP: mxagent.Config{
			Hostname:        "mx.test",
			ListenAddr:      "127.0.0.1:0",
			MaxMessageBytes: 1 << 20,
			MaxRecipients:   10,
			MaxConnections:  16,
			DataTimeout:     10 * time.Second,
			DNSTimeout:      2 * time.Second,
		},
	}
	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	be := &drainBackend{ingestIn: make(chan struct{}, 1), release: make(chan struct{}), blocked: true}
	manager := mxdial.New(be, mxdial.Config{
		DataDir:                  t.TempDir(),
		CoreKey:                  "core-key",
		ReceiverURL:              "http://" + st.SessionAddr,
		AllowPrivateDestinations: true,
		ReconcileInterval:        20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan struct{})
	go func() { defer close(managerDone); manager.Run(ctx) }()
	defer func() { cancel(); <-managerDone }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && manager.ConnectionStatus().State != "ready" {
		time.Sleep(20 * time.Millisecond)
	}
	if got := manager.ConnectionStatus().State; got != "ready" {
		t.Fatalf("core session not ready: %q", got)
	}

	sendResult := make(chan error, 1)
	go func() { sendResult <- sendMessage(st.SMTPAddr, "alice@example.test") }()
	select {
	case <-be.ingestIn:
	case <-time.After(5 * time.Second):
		t.Fatal("ingest never started")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	_, derr := rt.Deactivate(dctx)
	if !errors.Is(derr, receiverruntime.ErrDrainIncomplete) {
		t.Fatalf("want ErrDrainIncomplete, got %v", derr)
	}
	if got := rt.Status().State; got != control.StateStandby {
		t.Fatalf("state after cut drain %q", got)
	}
	// The sender must not have received a success for a cut transaction.
	select {
	case err := <-sendResult:
		if err == nil {
			t.Fatal("sender received 250 for a transaction cut by the drain bound")
		}
	case <-time.After(5 * time.Second):
		// The client may be blocked writing; that is acceptable, the point is it
		// was not told the message was accepted.
	}
	close(be.release)
}

// sendMessage performs one SMTP transaction and returns the DATA decision. A
// non-nil error means the edge did not accept the message.
func sendMessage(addr, recipient string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("sender.test"); err != nil {
		return err
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		return err
	}
	if err := c.Rcpt(recipient, nil); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	raw := "From: sender@outside.test\r\nSubject: drain\r\n\r\nin-flight body"
	if _, err := io.WriteString(w, raw); err != nil {
		return err
	}
	return w.Close()
}

// TestRepeatedConfigureRestarts proves a repeated Configure while active is
// handled by draining and re-arming rather than failing the core.
func TestRepeatedConfigureRestarts(t *testing.T) {
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	settings := control.Settings{
		Hostname:     "mx.test",
		Secret:       "s1",
		DrainTimeout: time.Second,
		SMTP:         mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20},
	}
	st, err := rt.Configure(context.Background(), settings)
	if err != nil {
		t.Fatalf("first configure: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("first configure state %q", st.State)
	}
	if st.SessionAddr != receiverruntime.DefaultSessionAddr {
		t.Fatalf("session addr %q", st.SessionAddr)
	}
	settings.Secret = "s2"
	st, err = rt.Configure(context.Background(), settings)
	if err != nil {
		t.Fatalf("repeated configure: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("repeated configure state %q", st.State)
	}
}

// TestConfigureRejectsMalformedPEM proves the SMTP TLS pair is validated before
// any listener is bound.
func TestConfigureRejectsMalformedPEM(t *testing.T) {
	dir := t.TempDir()
	cert := dir + "/cert.pem"
	if err := os.WriteFile(cert, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()
	_, err := rt.Configure(context.Background(), control.Settings{
		Hostname: "mx.test", Secret: "s",
		SMTP: mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20, TLSCertFile: cert, TLSKeyFile: cert},
	})
	if err == nil {
		t.Fatal("expected malformed PEM to fail activation")
	}
}

// TestConfigureRejectsHalfTLSPair proves a lone certificate or key is refused
// with a clear error.
func TestConfigureRejectsHalfTLSPair(t *testing.T) {
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()
	_, err := rt.Configure(context.Background(), control.Settings{
		Hostname: "mx.test", Secret: "s",
		SMTP: mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20, TLSCertFile: "/tmp/only-cert.pem"},
	})
	if err == nil {
		t.Fatal("expected a lone TLS certificate to fail activation")
	}
}
