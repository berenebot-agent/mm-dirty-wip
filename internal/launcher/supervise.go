package launcher

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/dellarb/mammothmail/dialmx/control"
)

// Edge is a running embedded edge child spawned by StartStandby.
type Edge struct {
	cmd *exec.Cmd
	log *slog.Logger
	// done is closed exactly once when the child exits. A closed channel is a
	// broadcast, so the main monitor and Stop can both observe the exit without
	// racing for a single channel value (which could strand a waiter).
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error

	// client is the standby control channel, set only by StartStandby. cmdW and
	// repR are the parent-side pipe ends, closed by Stop.
	client *control.Client
	cmdW   *os.File
	repR   *os.File
}

// Stop asks the edge to shut down over the control channel, then closes the
// parent's pipe ends so the child's blocking read releases even if the request
// was lost, and waits bounded for it to exit. It returns the child's exit
// error, if any. It is safe to call after the child has already exited and
// safe to call more than once: the exit channel is closed once, so a waiter
// can never block on a value already consumed elsewhere.
func (e *Edge) Stop(ctx context.Context) error {
	// Ask the child to close and exit over the control channel.
	_, _ = e.client.Shutdown(ctx)
	if e.cmdW != nil {
		_ = e.cmdW.Close()
	}
	if e.repR != nil {
		_ = e.repR.Close()
	}
	select {
	case <-e.done:
		return e.ExitError()
	case <-ctx.Done():
		// The edge did not stop in time. Signalling across uids is often not
		// permitted (the parent dropped privileges), so escalate to SIGKILL of
		// the child pid best-effort, then wait a short bounded time for the
		// reaper to observe the exit. Never wait without a deadline: a failed
		// kill must not hang shutdown.
		_ = e.cmd.Process.Kill()
		select {
		case <-e.done:
		case <-time.After(2 * time.Second):
			e.log.Warn("embedded mx edge did not exit after kill", "pid", e.cmd.Process.Pid)
		}
		return ctx.Err()
	}
}

// Wait returns a channel closed when the child exits. It is used by the
// parent's reaper so an unexpected edge exit is observed.
func (e *Edge) Wait() <-chan struct{} { return e.done }

// Pid returns the child's process id, or 0 before it is started. It is used for
// supervision and resource measurement.
func (e *Edge) Pid() int {
	if e.cmd == nil || e.cmd.Process == nil {
		return 0
	}
	return e.cmd.Process.Pid
}

// ExitError returns the child's exit error, or nil if it has not exited or
// exited cleanly. It is safe to call from any goroutine.
func (e *Edge) ExitError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// StopTimeout is the grace window for the edge to drain in-flight SMTP before
// it is force-killed.
const StopTimeout = 20 * time.Second
