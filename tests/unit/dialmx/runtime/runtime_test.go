package runtime_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	receiverruntime "github.com/dellarb/mailmoose/dialmx/runtime"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// newRuntime returns a runtime logging to a discard handler, plus a config that
// binds ephemeral ports so tests never collide on the fixed included
// endpoints.
func newRuntime(t *testing.T) (*receiverruntime.Runtime, receiverruntime.Config) {
	t.Helper()
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	return rt, receiverruntime.Config{
		Hostname:     "mx.test",
		Secret:       "test-secret",
		SessionAddr:  "127.0.0.1:0",
		DrainTimeout: 500 * time.Millisecond,
		SMTP: mxagent.Config{
			Hostname:        "mx.test",
			ListenAddr:      "127.0.0.1:0",
			MaxMessageBytes: 1 << 20,
		},
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestActivateBindsAndStatusReports(t *testing.T) {
	rt, cfg := newRuntime(t)
	defer rt.Close()

	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("state %q", st.State)
	}
	if st.SMTPAddr == "" || st.SessionAddr == "" {
		t.Fatalf("missing bound addresses: %+v", st)
	}

	// The session listener must actually accept connections now.
	c, err := net.DialTimeout("tcp", st.SessionAddr, time.Second)
	if err != nil {
		t.Fatalf("session listener not accepting: %v", err)
	}
	_ = c.Close()

	if got := rt.Status(); got.State != control.StateActive || got.SMTPAddr != st.SMTPAddr {
		t.Fatalf("status after activate: %+v", got)
	}
}

func TestActivateTwiceRefused(t *testing.T) {
	rt, cfg := newRuntime(t)
	defer rt.Close()
	if _, err := rt.Activate(context.Background(), cfg); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := rt.Activate(context.Background(), cfg); err == nil {
		t.Fatal("second activate should be refused")
	}
}

func TestDeactivateStopsAdmissionThenAllowsReactivate(t *testing.T) {
	rt, cfg := newRuntime(t)
	defer rt.Close()

	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	sessionAddr := st.SessionAddr

	dst, err := rt.Deactivate(context.Background())
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if dst.State != control.StateStandby {
		t.Fatalf("state after deactivate %q", dst.State)
	}

	// The old listener must be closed: a fresh dial fails.
	if c, derr := net.DialTimeout("tcp", sessionAddr, 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatal("session listener still accepting after deactivate")
	}

	// The child stays alive: a second activation succeeds.
	st2, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if st2.State != control.StateActive {
		t.Fatalf("reactivate state %q", st2.State)
	}
}

func TestDeactivateIsIdempotent(t *testing.T) {
	rt, cfg := newRuntime(t)
	defer rt.Close()
	if _, err := rt.Activate(context.Background(), cfg); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := rt.Deactivate(context.Background()); err != nil {
		t.Fatalf("first deactivate: %v", err)
	}
	if _, err := rt.Deactivate(context.Background()); err != nil {
		t.Fatalf("second deactivate: %v", err)
	}
}

func TestDeactivateDrainsWithinBound(t *testing.T) {
	rt, cfg := newRuntime(t)
	cfg.DrainTimeout = 2 * time.Second
	// A held SMTP connection drains when its read timeout fires; keep it short
	// so the graceful shutdown completes inside the test bound.
	cfg.SMTP.ReadTimeout = 100 * time.Millisecond
	defer rt.Close()

	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	// Hold an open SMTP connection with no protocol bytes so the drain has
	// something in flight; Deactivate must still return within the bound.
	c, err := net.DialTimeout("tcp", st.SMTPAddr, time.Second)
	if err != nil {
		t.Fatalf("smtp dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := rt.Deactivate(ctx); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("deactivate exceeded bound: %s", elapsed)
	}
}

func TestActivateFailureKeepsStandby(t *testing.T) {
	// Occupy a fixed port, then point the runtime's session listener at it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()

	rt, cfg := newRuntime(t)
	defer rt.Close()
	cfg.SessionAddr = blocker.Addr().String()

	if _, err := rt.Activate(context.Background(), cfg); err == nil {
		t.Fatal("expected activation to fail on a busy port")
	}
	if got := rt.Status(); got.State != control.StateStandby {
		t.Fatalf("state after failed activate %q", got.State)
	}
}

func TestConfigureUsesFixedEndpoints(t *testing.T) {
	// Configure maps wire settings onto the fixed endpoints; assert the mapping
	// by observing that a settings-supplied listen address is ignored.
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	// Bind the fixed SMTP port for the duration of the test so the real
	// endpoint is available; if it is occupied, skip rather than flake.
	probe, err := net.Listen("tcp", receiverruntime.DefaultSMTPAddr)
	if err != nil {
		t.Skipf("fixed SMTP endpoint %s unavailable: %v", receiverruntime.DefaultSMTPAddr, err)
	}
	_ = probe.Close()
	probe, err = net.Listen("tcp", receiverruntime.DefaultSessionAddr)
	if err != nil {
		t.Skipf("fixed session endpoint %s unavailable: %v", receiverruntime.DefaultSessionAddr, err)
	}
	_ = probe.Close()

	st, err := rt.Configure(context.Background(), control.Settings{
		Hostname: "mx.test",
		Secret:   "s",
		SMTP: mxagent.Config{
			Hostname:        "mx.test",
			ListenAddr:      "127.0.0.1:59999", // must be overridden
			MaxMessageBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, port, err := net.SplitHostPort(st.SMTPAddr); err != nil || port != "2525" {
		t.Fatalf("smtp addr %q, want port 2525", st.SMTPAddr)
	}
	if st.SessionAddr != receiverruntime.DefaultSessionAddr {
		t.Fatalf("session addr %q, want %q", st.SessionAddr, receiverruntime.DefaultSessionAddr)
	}
}
