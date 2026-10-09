package launcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/dellarb/mailmoose/dialmx"
	"github.com/dellarb/mailmoose/dialmx/control"
)

// StartStandby spawns the included receiver as a child under spec.UID/GID,
// before the caller drops its own privileges, and leaves it in standby: the
// child binds no SMTP or session listener until the core calls Configure. The
// child inherits two dedicated pipes (fd 3 = command read, fd 4 = reply write)
// and is controlled through them; it never receives APP_ENCRYPTION_KEY, the
// data directory or any bearer secret in its environment. The secret is sent
// over the private control channel on activation.
//
// This is the preferred embedded-edge entry point: the process stays alive
// across activate/deactivate cycles, so a configuration change never requires
// a child restart.
func StartStandby(ctx context.Context, spec Spec, log *slog.Logger) (*Edge, error) {
	if log == nil {
		log = slog.Default()
	}
	if os.Getuid() != 0 {
		return nil, errors.New("launcher: must be root to start the embedded edge")
	}
	// Parent writes commands on cmdW, child reads cmdR (fd 3). Child writes
	// replies on repW (fd 4), parent reads repR.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("launcher: control command pipe: %w", err)
	}
	repR, repW, err := os.Pipe()
	if err != nil {
		_ = cmdR.Close()
		_ = cmdW.Close()
		return nil, fmt.Errorf("launcher: control reply pipe: %w", err)
	}
	cmd := exec.Command(spec.Binary)
	cmd.Env = mergeEnv(spec.Env, StandbyEnv(3, 4))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{cmdR, repW} // fd 3 = command read, fd 4 = reply write
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(spec.UID), Gid: uint32(spec.GID)},
		// If the parent dies uncleanly, the kernel signals the child so it does
		// not linger without a supervisor.
		Pdeathsig: syscall.SIGTERM,
	}
	if err := cmd.Start(); err != nil {
		_ = cmdR.Close()
		_ = cmdW.Close()
		_ = repR.Close()
		_ = repW.Close()
		return nil, fmt.Errorf("launcher: start embedded edge: %w", err)
	}
	// The child owns the other ends now.
	_ = cmdR.Close()
	_ = repW.Close()

	e := &Edge{
		cmd:    cmd,
		log:    log,
		done:   make(chan struct{}),
		client: control.NewClient(repR, cmdW),
		cmdW:   cmdW,
		repR:   repR,
	}
	go func() {
		err := cmd.Wait()
		e.mu.Lock()
		e.err = err
		e.mu.Unlock()
		e.once.Do(func() { close(e.done) })
	}()
	log.Info("embedded mx edge started in standby", "pid", cmd.Process.Pid, "uid", spec.UID, "binary", spec.Binary)
	return e, nil
}

// Configure activates the standby edge with the given settings. It returns the
// edge's resulting status.
func (e *Edge) Configure(ctx context.Context, s control.Settings) (control.Status, error) {
	if e.client == nil {
		return control.Status{}, errors.New("launcher: edge has no standby control channel")
	}
	return e.client.Configure(ctx, s)
}

// Deactivate stops admission, drains bounded, then stops sessions on the
// standby edge, returning its resulting status. The child stays alive.
func (e *Edge) Deactivate(ctx context.Context) (control.Status, error) {
	if e.client == nil {
		return control.Status{}, errors.New("launcher: edge has no standby control channel")
	}
	return e.client.Deactivate(ctx)
}

// Status queries the standby edge's lifecycle state.
func (e *Edge) Status(ctx context.Context) (control.Status, error) {
	if e.client == nil {
		return control.Status{}, errors.New("launcher: edge has no standby control channel")
	}
	return e.client.Status(ctx)
}

// StandbyEnv is the minimal environment for a standby edge child. It carries
// PATH, the inherited control descriptor numbers, and the receiver log-level
// knob when the operator set it; the SMTP settings and bearer secret arrive
// over the control channel on activation. DIALMX_LOG_LEVEL is a non-secret
// verbosity selector, so forwarding it lets an operator turn on the edge's
// connect/session diagnostics without a rebuild. The DNS fallback list is
// likewise non-secret and is forwarded so the edge's SPF/DKIM/DMARC and TXT
// proof lookups recover from a resolver outage the same way the core does.
func StandbyEnv(cmdFD, replyFD int) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		control.CmdFDEnv + "=" + strconv.Itoa(cmdFD),
		control.ReplyFDEnv + "=" + strconv.Itoa(replyFD),
	}
	if lvl := strings.TrimSpace(os.Getenv(dialmx.EnvLogLevel)); lvl != "" {
		env = append(env, dialmx.EnvLogLevel+"="+lvl)
	}
	if servers := strings.TrimSpace(os.Getenv("MAILMOOSE_DNS_FALLBACK_SERVERS")); servers != "" {
		env = append(env, "MAILMOOSE_DNS_FALLBACK_SERVERS="+servers)
	}
	return env
}

// forbiddenEdgeEnv are names that must never reach the edge child, whether from
// the operator's environment or a caller-supplied Spec.Env.
var forbiddenEdgeEnv = map[string]bool{
	"APP_ENCRYPTION_KEY": true,
	"DATA_DIR":           true,
	"DIALMX_CORE_KEY":    true,
	control.CmdFDEnv:     true,
	control.ReplyFDEnv:   true,
}

// mergeEnv returns base plus extra entries whose keys are new and not
// forbidden, so a caller cannot accidentally forward the core's secrets.
func mergeEnv(extra, base []string) []string {
	out := append([]string{}, base...)
	seen := make(map[string]bool, len(base))
	for _, kv := range base {
		if k := envKey(kv); k != "" {
			seen[k] = true
		}
	}
	for _, kv := range extra {
		k := envKey(kv)
		if k == "" || forbiddenEdgeEnv[k] || seen[k] {
			continue
		}
		out = append(out, kv)
		seen[k] = true
	}
	return out
}

func envKey(kv string) string {
	i := strings.IndexByte(kv, '=')
	if i <= 0 {
		return ""
	}
	return strings.TrimSpace(kv[:i])
}
