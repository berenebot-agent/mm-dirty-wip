// Package control implements the private command channel between the core
// process and its included (embedded) Dial MX receiver child.
//
// The channel is a pair of inherited pipes (or a socketpair): the core writes
// requests, the child writes replies. Every frame is a 4-byte big-endian length
// prefix followed by a bounded JSON payload, so neither side can be made to
// allocate without limit by a misbehaving peer. The protocol is deliberately
// small: the receiver is spawned once in standby (no SMTP or session listeners
// bound, no database, no application key) and is then activated, deactivated
// and queried without exiting the child. The core auto-generates the bearer
// secret and sends it here on activation; it is never read from the child's
// environment or written to disk.
package control

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mxagent"
)

// Environment variables naming the inherited file descriptors. The command
// descriptor is read by the child; the reply descriptor is written by it. Both
// are inherited across the privilege drop, like the SMTP edge's shutdown pipe.
const (
	CmdFDEnv   = "DIALMX_CONTROL_CMD_FD"
	ReplyFDEnv = "DIALMX_CONTROL_REPLY_FD"

	// MaxFrameBytes bounds one frame payload. Settings carry a full
	// mxagent.Config (a few dozen short fields), a hostname and a 64-byte
	// secret; status replies are tiny. 64 KiB is ample and stops a corrupt
	// length prefix from forcing a large read.
	MaxFrameBytes = 64 << 10

	// DefaultTimeout bounds one request/reply round trip when the caller's
	// context carries no earlier deadline.
	DefaultTimeout = 30 * time.Second
)

// Request types.
const (
	TypeConfigure  = "configure"
	TypeDeactivate = "deactivate"
	TypeStatus     = "status"
	TypeShutdown   = "shutdown"
)

// State is the receiver child's lifecycle state. It is independent of the
// process lifetime: the child runs for the process's whole life and moves
// between these states as the core activates and deactivates it.
type State string

const (
	// StateStandby means the child is alive but bound to no listeners.
	StateStandby State = "standby"
	// StateActive means both the SMTP edge and the session listener are bound.
	StateActive State = "active"
	// StateDraining means admission has stopped and in-flight work is draining.
	StateDraining State = "draining"
	// StateFailed means a listener failed independently; the failure is
	// reported in Status.Error and the child remains under core control.
	StateFailed State = "failed"
)

// Settings activates the included receiver. The core builds these from the
// persisted configuration; the child applies its own fixed endpoints. Secret
// is the auto-generated bearer credential shared with the core's private dialer.
type Settings struct {
	// Hostname is the SMTP greeting hostname.
	Hostname string `json:"hostname"`
	// SMTP is the operator's SMTP edge configuration (bounds, verification
	// toggles, DNS resolver and timeouts). It never carries the core's
	// application key or data directory.
	SMTP mxagent.Config `json:"smtp"`
	// TLSCertificatePEM and TLSPrivateKeyPEM carry an optional STARTTLS
	// certificate and key as PEM text. They travel over the private control
	// channel because the child runs under a separate uid and cannot read the
	// core's /data or the application key; the file-path fields on
	// mxagent.Config are for the standalone environment-configured receiver.
	// Both are empty, or both are present.
	TLSCertificatePEM string `json:"tls_certificate_pem,omitempty"`
	TLSPrivateKeyPEM  string `json:"tls_private_key_pem,omitempty"`
	// Secret is the bearer credential the core dials in with. It is never
	// logged.
	Secret string `json:"secret"`
	// DrainTimeout bounds a deliberate deactivation's drain of in-flight SMTP
	// transactions. Zero selects the receiver's default.
	DrainTimeout time.Duration `json:"drain_timeout,omitempty"`
}

// Status is the child's current lifecycle state, returned by every reply that
// changes or queries state.
type Status struct {
	State             State  `json:"state"`
	SessionAddr       string `json:"session_addr,omitempty"`
	SMTPAddr          string `json:"smtp_addr,omitempty"`
	ActiveConnections int64  `json:"active_connections"`
	Error             string `json:"error,omitempty"`
}

// Handler is implemented by the receiver side (the process runtime). Every
// method is called from the single control-loop goroutine, so implementations
// need not serialise calls against each other (they must still be safe against
// their own listener goroutines).
type Handler interface {
	Configure(ctx context.Context, s Settings) (Status, error)
	Deactivate(ctx context.Context) (Status, error)
	Status() Status
	Close() error
}

type request struct {
	Type     string    `json:"type"`
	Settings *Settings `json:"settings,omitempty"`
}

type reply struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// Enabled reports whether standby control mode was requested via the inherited
// descriptors. When false the command entrypoint runs the standalone
// environment-configured receiver unchanged.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(CmdFDEnv)) != "" && strings.TrimSpace(os.Getenv(ReplyFDEnv)) != ""
}

// CommandFile returns the inherited command read end, or an error when the
// descriptor was not provided or is invalid.
func CommandFile() (*os.File, error) { return fdFile(CmdFDEnv, "dialmx-control-cmd") }

// ReplyFile returns the inherited reply write end, or an error when the
// descriptor was not provided or is invalid.
func ReplyFile() (*os.File, error) { return fdFile(ReplyFDEnv, "dialmx-control-reply") }

func fdFile(env, name string) (*os.File, error) {
	raw := strings.TrimSpace(os.Getenv(env))
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("control: invalid %s=%q", env, raw)
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, fmt.Errorf("control: fd %d unavailable for %s", fd, env)
	}
	return f, nil
}

// readRequest reads one length-prefixed JSON request. A clean EOF at a frame
// boundary is returned as io.EOF so the caller can treat it as shutdown.
func readRequest(r io.Reader) (request, error) {
	payload, err := readFrame(r)
	if err != nil {
		return request{}, err
	}
	var req request
	if err := json.Unmarshal(payload, &req); err != nil {
		return request{}, fmt.Errorf("control: decode request: %w", err)
	}
	if req.Type == "" {
		return request{}, errors.New("control: request missing type")
	}
	return req, nil
}

func writeReply(w io.Writer, rep reply) error {
	payload, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("control: encode reply: %w", err)
	}
	return writeFrame(w, payload)
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("control: frame too large: %d bytes", n)
	}
	if n == 0 {
		return []byte{}, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("control: frame too large: %d bytes", len(payload))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// Serve runs the child side of the control loop until the command stream ends,
// a shutdown request arrives, or ctx is cancelled. It is the only reader of
// cmd and the only writer of reply, so it serialises every handler call.
func Serve(ctx context.Context, cmd io.Reader, reply io.Writer, h Handler, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := readRequest(cmd)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		rep := dispatch(ctx, h, req, log)
		if err := writeReply(reply, rep); err != nil {
			return err
		}
		if req.Type == TypeShutdown {
			return nil
		}
	}
}

func dispatch(ctx context.Context, h Handler, req request, log *slog.Logger) reply {
	switch req.Type {
	case TypeConfigure:
		if req.Settings == nil {
			return reply{Error: "configure requires settings"}
		}
		st, err := h.Configure(ctx, *req.Settings)
		if err != nil {
			log.Warn("included receiver configure failed", "error", err)
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Status: &st}
	case TypeDeactivate:
		st, err := h.Deactivate(ctx)
		if err != nil {
			log.Warn("included receiver deactivate failed", "error", err)
			return reply{Error: err.Error()}
		}
		return reply{OK: true, Status: &st}
	case TypeStatus:
		st := h.Status()
		return reply{OK: true, Status: &st}
	case TypeShutdown:
		if err := h.Close(); err != nil {
			return reply{Error: err.Error()}
		}
		st := h.Status()
		return reply{OK: true, Status: &st}
	default:
		return reply{Error: fmt.Sprintf("unknown request type %q", req.Type)}
	}
}

// Client is the core-side half of the control channel. It is safe for
// concurrent use: every round trip is serialised so a request and its reply are
// never interleaved with another caller's.
type Client struct {
	mu      sync.Mutex
	r       io.Reader
	w       io.Writer
	timeout time.Duration
	// poisoned is set once a round trip fails after bytes may have been written
	// or before its reply was fully read. The stream framing can no longer be
	// trusted, so every later call fails fast instead of reading a stale reply
	// and misattributing it to the wrong request.
	poisoned error
}

// NewClient wraps the core's end of the control pipes.
func NewClient(r io.Reader, w io.Writer) *Client {
	return &Client{r: r, w: w, timeout: DefaultTimeout}
}

// Configure sends settings and activates the receiver, returning the resulting
// status.
func (c *Client) Configure(ctx context.Context, s Settings) (Status, error) {
	rep, err := c.call(ctx, request{Type: TypeConfigure, Settings: &s})
	if err != nil {
		return Status{}, err
	}
	return statusOf(rep), nil
}

// Deactivate stops admission, drains bounded, stops sessions and returns the
// resulting status.
func (c *Client) Deactivate(ctx context.Context) (Status, error) {
	rep, err := c.call(ctx, request{Type: TypeDeactivate})
	if err != nil {
		return Status{}, err
	}
	return statusOf(rep), nil
}

// Status queries the receiver's current lifecycle state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	rep, err := c.call(ctx, request{Type: TypeStatus})
	if err != nil {
		return Status{}, err
	}
	return statusOf(rep), nil
}

// Shutdown asks the receiver to close and exit.
func (c *Client) Shutdown(ctx context.Context) (Status, error) {
	rep, err := c.call(ctx, request{Type: TypeShutdown})
	if err != nil {
		return Status{}, err
	}
	return statusOf(rep), nil
}

func (c *Client) call(ctx context.Context, req request) (reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.poisoned != nil {
		return reply{}, c.poisoned
	}

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	setWriteDeadline(c.w, deadline)
	payload, err := json.Marshal(req)
	if err != nil {
		return reply{}, fmt.Errorf("control: encode request: %w", err)
	}
	if err := writeFrame(c.w, payload); err != nil {
		return reply{}, c.poison(req.Type, err)
	}
	setReadDeadline(c.r, deadline)
	payload, err = readFrame(c.r)
	if err != nil {
		return reply{}, c.poison(req.Type, err)
	}
	var rep reply
	if err := json.Unmarshal(payload, &rep); err != nil {
		return reply{}, c.poison(req.Type, err)
	}
	if !rep.OK {
		// A well-formed error reply is a clean protocol exchange: the channel
		// remains usable.
		if rep.Error == "" {
			rep.Error = "control request failed"
		}
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// poison marks the channel unusable after a transport or framing failure and
// returns a wrapped error naming the request that lost its reply.
func (c *Client) poison(reqType string, err error) error {
	c.poisoned = fmt.Errorf("control: channel poisoned after %s: %w", reqType, err)
	return c.poisoned
}

func statusOf(rep reply) Status {
	if rep.Status == nil {
		return Status{}
	}
	return *rep.Status
}

type readDeadliner interface{ SetReadDeadline(time.Time) error }
type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

func setReadDeadline(r io.Reader, t time.Time) {
	if d, ok := r.(readDeadliner); ok {
		_ = d.SetReadDeadline(t)
	}
}

func setWriteDeadline(w io.Writer, t time.Time) {
	if d, ok := w.(writeDeadliner); ok {
		_ = d.SetWriteDeadline(t)
	}
}
