package launcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Edge is a running embedded edge child.
type Edge struct {
	cmd    *exec.Cmd
	writer *os.File // parent's end of the shutdown pipe
	log    *slog.Logger
	done   chan error
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
	e := &Edge{cmd: cmd, writer: w, log: log, done: make(chan error, 1)}
	go func() { e.done <- cmd.Wait() }()
	log.Info("embedded mx edge started", "pid", cmd.Process.Pid, "uid", spec.UID, "binary", spec.Binary)
	return e, nil
}

// Stop asks the edge to shut down by closing the pipe, then waits bounded for
// it to exit. It returns the child's exit error, if any.
func (e *Edge) Stop(ctx context.Context) error {
	// Closing the write end gives the child EOF on fd 3, which it treats as a
	// shutdown request. This is the only cross-uid channel available after the
	// parent drops privileges.
	_ = e.writer.Close()
	select {
	case err := <-e.done:
		return err
	case <-ctx.Done():
		// The edge did not stop in time; signal is not permitted across uids,
		// so escalate to SIGKILL of the child pid.
		_ = e.cmd.Process.Kill()
		<-e.done
		return ctx.Err()
	}
}

// Wait returns a channel that receives the child's exit. It is used by the
// parent's reaper so an unexpected edge exit is observed.
func (e *Edge) Wait() <-chan error { return e.done }

// StopTimeout is the grace window for the edge to drain in-flight SMTP before
// it is force-killed.
const StopTimeout = 20 * time.Second
