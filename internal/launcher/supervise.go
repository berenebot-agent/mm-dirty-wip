package launcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
)

// Edge is a running embedded edge child.
type Edge struct {
	cmd    *exec.Cmd
	writer *os.File // parent's end of the legacy shutdown pipe
	log    *slog.Logger
	// done is closed exactly once when the child exits. A closed channel is a
	// broadcast, so the main monitor and Stop can both observe the exit without
	// racing for a single channel value (which could strand a waiter).
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error

	// client is the standby control channel, set only by StartStandby. cmdW and
	// repR are its parent-side pipe ends, closed by Stop.
	client *control.Client
	cmdW   *os.File
	repR   *os.File
}

// Start spawns the edge as a child under spec.UID/GID using SysProcAttr.
// Credential. It requires the caller to be root (the caller then drops its own
// privileges). The child inherits the read end of a pipe as fd 3 and is asked
// to stop by closing the parent's write end.
func Start(ctx context.Context, spec Spec, log *slog.Logger) (*Edge, error) {
	if log == nil {
		log = slog.Default()
	}
	if os.Getuid() != 0 {
		return nil, errors.New("launcher: must be root to start the embedded edge")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("launcher: shutdown pipe: %w", err)
	}
	cmd := exec.Command(spec.Binary)
	cmd.Env = spec.Env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(spec.UID), Gid: uint32(spec.GID)},
		// If the parent dies uncleanly, the kernel signals the child so it does
		// not linger without a supervisor.
		Pdeathsig: syscall.SIGTERM,
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("launcher: start edge: %w", err)
	}
	r.Close() // the child owns the read end now
	e := &Edge{cmd: cmd, writer: w, log: log, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		e.mu.Lock()
		e.err = err
		e.mu.Unlock()
		e.once.Do(func() { close(e.done) })
	}()
	log.Info("embedded mx edge started", "pid", cmd.Process.Pid, "uid", spec.UID, "binary", spec.Binary)
	return e, nil
}

// Stop asks the edge to shut down by closing the pipe, then waits bounded for
// it to exit. It returns the child's exit error, if any. It is safe to call
// after the child has already exited and safe to call more than once: the exit
// channel is closed once, so a waiter can never block on a value already
// consumed elsewhere.
func (e *Edge) Stop(ctx context.Context) error {
	if e.client != nil {
		// Standby edge: ask the child to close and exit over the control
		// channel, then close the parent's pipe ends so the child's blocking
		// read releases even if the request was lost.
		_, _ = e.client.Shutdown(ctx)
		if e.cmdW != nil {
			_ = e.cmdW.Close()
		}
		if e.repR != nil {
			_ = e.repR.Close()
		}
	} else {
		// Closing the write end gives the child EOF on fd 3, which it treats as
		// a shutdown request. This is the only cross-uid channel available
		// after the parent drops privileges.
		_ = e.writer.Close()
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
