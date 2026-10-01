package launcher_test

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/launcher"
)

// standbyHelperEnv marks the re-executed test binary as the standby child.
const standbyHelperEnv = "LAUNCHER_STANDBY_HELPER"

// TestMain lets the test binary double as the embedded receiver child. When the
// launcher spawns it with standbyHelperEnv set and the inherited control
// descriptors, it serves the control protocol against a tiny in-memory handler
// instead of running the test suite.
func TestMain(m *testing.M) {
	if os.Getenv(standbyHelperEnv) == "1" {
		runStandbyHelper()
		return
	}
	os.Exit(m.Run())
}

func runStandbyHelper() {
	cmdFile, err := control.CommandFile()
	if err != nil {
		os.Exit(2)
	}
	replyFile, err := control.ReplyFile()
	if err != nil {
		os.Exit(2)
	}
	h := &helperHandler{state: control.StateStandby}
	_ = control.Serve(context.Background(), cmdFile, replyFile, h, slog.Default())
	_ = cmdFile.Close()
	_ = replyFile.Close()
	os.Exit(0)
}

// helperHandler mirrors the process runtime's externally visible contract
// without binding real listeners, so the test exercises the pipe wiring, the
// privilege spawn and the request/reply protocol.
type helperHandler struct {
	mu     sync.Mutex
	state  control.State
	host   string
	secret string
	closed bool
}

func (h *helperHandler) Configure(_ context.Context, s control.Settings) (control.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = control.StateActive
	h.host = s.Hostname
	h.secret = s.Secret
	return h.statusLocked(), nil
}

func (h *helperHandler) Deactivate(context.Context) (control.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = control.StateStandby
	return h.statusLocked(), nil
}

func (h *helperHandler) Status() control.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked()
}

func (h *helperHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

func (h *helperHandler) statusLocked() control.Status {
	return control.Status{State: h.state, SessionAddr: "127.0.0.1:8443", SMTPAddr: ":2525"}
}

// TestStartStandbyControlRoundTrip is an end-to-end test of the exported
// standby API: it spawns the real test binary as a child under the launcher's
// pipes, then drives Configure, Status, Deactivate and Stop. It is skipped when
// the process is not root, because the launcher requires it to set the child's
// credentials.
func TestStartStandbyControlRoundTrip(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("launcher requires root to spawn the edge under a separate uid")
	}
	log := slog.New(slog.NewTextHandler(discard{}, nil))
	e, err := launcher.StartStandby(t.Context(), launcher.Spec{
		Binary: os.Args[0],
		UID:    0,
		GID:    0,
		Env:    []string{standbyHelperEnv + "=1"},
	}, log)
	if err != nil {
		t.Fatalf("start standby: %v", err)
	}
	stopped := false
	defer func() {
		if stopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := e.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != control.StateStandby {
		t.Fatalf("initial state %q", st.State)
	}

	st, err = e.Configure(ctx, control.Settings{Hostname: "mx.test", Secret: "generated-secret"})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("configure state %q", st.State)
	}

	st, err = e.Status(ctx)
	if err != nil {
		t.Fatalf("status after configure: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("status state %q", st.State)
	}

	st, err = e.Deactivate(ctx)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if st.State != control.StateStandby {
		t.Fatalf("deactivate state %q", st.State)
	}

	if err := e.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped = true
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
