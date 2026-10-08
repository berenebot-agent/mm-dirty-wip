package receiver

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/logging"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// This file is the receiver's complete structured observability contract. Every
// record is compact plain text (the same [MX]-tagged format the core uses, so
// the included edge and the core interleave readably on one container stream)
// at INFO through one *slog.Logger, so a shared SMTP edge line and a receiver
// lifecycle line carry the same format. Events deliberately carry only bounded,
// non-secret facts: ids, outcomes, classifiers, counters and durations. Raw
// MIME, TXT proof records, signatures and key material are never logged.
//
// Verbosity: process lifecycle, mail receipt, mail transfer and failures are
// INFO. The per-connection and per-session transport chatter (transport
// accepted/closed, session opened/hello/closed; and the shared edge's
// connection opened/closed, session started, STARTTLS and reply-write records)
// is DEBUG, hidden unless DIALMX_LOG_LEVEL=debug. This is deliberate for the
// managed one-core receiver, where the connect chatter is pure noise; the
// event names below are unchanged, only their level differs.
//
// Event grammar (all messages are lower-case, space-separated, stable):
//
//	dialmx receiver starting          once, at boot
//	dialmx receiver ready             once, after both listeners bind
//	dialmx receiver stopping          once, on shutdown
//	dialmx receiver stopped           once, after bounded drain
//	dialmx listener bound             per listener
//	dialmx listener failed            per listener
//	dialmx settings                   once, effective configuration
//
//	dialmx transport accepted         per accepted TCP connection      (DEBUG)
//	dialmx transport tls failure      per pre-handshake TLS failure    (DEBUG)
//	dialmx transport closed           per closed TCP connection        (DEBUG)
//	dialmx session rejected           per rejected session (never admitted) (INFO)
//	dialmx session opened             per admitted session             (DEBUG)
//	dialmx session hello              per decoded Hello                (DEBUG)
//	dialmx session closed             per session teardown             (DEBUG)
//
//	dialmx domain proof               per DNS proof attempt (lookup/parse/key/signature/renew/lifecycle)
//
//	dialmx resolve                    per recipient routing decision
//	dialmx handoff                    per connection handoff lifecycle
//	dialmx handoff result             per recipient durable outcome
//
// The shared SMTP edge's mx * records use the same [MX] text format.
//
// Registration metadata: a core enrolled through a named shared service (Antler
// MX) supplies contact_email and setup_id on DomainAuth. Both are operational
// metadata, never credentials — domain authority remains the DNS-anchored
// Ed25519 proof. They are logged on the domain proof registration/renewal and
// on resolve/handoff records so usage can be aggregated by operator contact;
// they are omitted entirely for a custom receiver. A malformed value is dropped
// and never rejects the proof.

// Event names. They are used both as the slog message and as the event
// attribute value, so the message and the attribute never drift. The process
// lifecycle events are exported so the command entrypoint emits the exact same
// strings through the same envelope.
const (
	EventReceiverStarting = "dialmx receiver starting"
	EventReceiverReady    = "dialmx receiver ready"
	EventReceiverStopping = "dialmx receiver stopping"
	EventReceiverStopped  = "dialmx receiver stopped"
	EventListenerBound    = "dialmx listener bound"
	EventListenerFailed   = "dialmx listener failed"
	EventSettings         = "dialmx settings"

	eventTransportAccepted   = "dialmx transport accepted"
	eventTransportTLSFailure = "dialmx transport tls failure"
	eventTransportClosed     = "dialmx transport closed"
	eventSessionRejected     = "dialmx session rejected"
	eventSessionOpened       = "dialmx session opened"
	eventSessionHello        = "dialmx session hello"
	eventSessionClosed       = "dialmx session closed"
	eventDomainProof         = "dialmx domain proof"
	eventResolve             = "dialmx resolve"
	eventHandoff             = "dialmx handoff"
	eventHandoffResult       = "dialmx handoff result"

	// eventConnEstablished and eventConnClosed are the INFO lifecycle records.
	// The DEBUG transport/session events above are per-step chatter; these two
	// are the operator-facing view of "a core connected" and "a core
	// disconnected", so a working receiver is visible at the default level
	// instead of only producing lines when something fails.
	eventConnEstablished = "dialmx core connected"
	eventConnClosed      = "dialmx core disconnected"
	// eventSummary is the periodic INFO heartbeat.
	eventSummary = "dialmx summary"
)

// NewLogger returns the receiver's one logger: the shared compact text handler
// tagged [MX], so a receiver line and a shared SMTP edge line carry the same
// format as the core's [Core] lines. level is the minimum level (Info by
// default, Debug for the connect/session transport chatter). The process
// entrypoint calls this once and passes the result to both the receiver and the
// SMTP edge.
func NewLogger(level slog.Level, w io.Writer) *slog.Logger {
	return logging.New(w, level, logging.PrefixMX)
}

// txnLog appends the edge-supplied correlation attributes (smtp_connection_id,
// message_transaction_id, peer_ip, helo) when present. It is the only place the
// receiver reads mxagent.TransactionAttrsFromContext, so the public helper
// contract is exercised exactly as consumed.
func txnLog(ctx context.Context, args ...any) []any {
	attrs, ok := mxagent.TransactionAttrsFromContext(ctx)
	if !ok {
		return args
	}
	return append(attrs.SlogArgs(), args...)
}

// handshakeCompleter is satisfied by *tls.Conn. The session listener serves TLS
// through ServeTLS, so http.Server hands the ConnState callback the *tls.Conn
// directly; its ConnectionState reports whether the handshake ever completed.
type handshakeCompleter interface {
	ConnectionState() tls.ConnectionState
}

// transportState is the per-TCP-connection identity. It is created in
// ConnContext (which http.Server calls with the same net.Conn ConnState later
// observes) and consumed in ConnState, so a transport gets a unique id, a
// numeric duration and a peer host:port on both accept and close.
type transportState struct {
	id      string
	started time.Time
	peer    string
	port    string
}

// TransportTracker correlates the TCP-level lifecycle of the HTTPS session
// listener. It never inspects protocol bytes.
type TransportTracker struct {
	log *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]*transportState
}

// NewTransportTracker returns a tracker that logs through log.
func NewTransportTracker(log *slog.Logger) *TransportTracker {
	return &TransportTracker{log: log, conns: map[net.Conn]*transportState{}}
}

// ConnContext assigns each accepted connection a transport id and records the
// start time, attaching both to the request context so the session handler can
// report the same transport_id.
func (tr *TransportTracker) ConnContext(ctx context.Context, c net.Conn) context.Context {
	peer, port := peerHostPort(c.RemoteAddr())
	st := &transportState{
		id:      newID(),
		started: time.Now(),
		peer:    peer,
		port:    port,
	}
	tr.mu.Lock()
	tr.conns[c] = st
	tr.mu.Unlock()
	return withTransportContext(ctx, st)
}

// ConnState records the HTTPS connection lifecycle: accepted at StateNew, and
// at close either a pre-session TLS failure (the handshake never completed) or
// a normal close, each with the transport id, peer host:port and numeric
// duration. A TLS failure is logged AND followed by the close record with an
// aborted reason, so a reader sees both the cause and the terminal close.
func (tr *TransportTracker) ConnState(c net.Conn, s http.ConnState) {
	if tr.log == nil {
		return
	}
	transport := "http"
	if _, ok := c.(handshakeCompleter); ok {
		transport = "https"
	}
	switch s {
	case http.StateNew:
		tr.mu.Lock()
		st := tr.conns[c]
		tr.mu.Unlock()
		if st == nil {
			return
		}
		tr.log.Debug(eventTransportAccepted,
			"transport_id", st.id,
			"transport", transport,
			"peer", st.peer,
			"peer_port", st.port,
		)
	case http.StateClosed, http.StateHijacked:
		tr.mu.Lock()
		st := tr.conns[c]
		delete(tr.conns, c)
		tr.mu.Unlock()
		if st == nil {
			return
		}
		ms := time.Since(st.started).Milliseconds()
		reason := "closed"
		if hc, ok := c.(handshakeCompleter); ok && !hc.ConnectionState().HandshakeComplete {
			reason = "tls_handshake_failed"
			tr.log.Debug(eventTransportTLSFailure,
				"transport_id", st.id,
				"transport", "https",
				"peer", st.peer,
				"peer_port", st.port,
				"duration_ms", ms,
				"reason", reason,
			)
		}
		tr.log.Debug(eventTransportClosed,
			"transport_id", st.id,
			"transport", transport,
			"peer", st.peer,
			"peer_port", st.port,
			"duration_ms", ms,
			"reason", reason,
		)
	}
}

type transportCtxKey struct{}

func withTransportContext(ctx context.Context, st *transportState) context.Context {
	return context.WithValue(ctx, transportCtxKey{}, st)
}

func transportFromContext(ctx context.Context) (*transportState, bool) {
	st, ok := ctx.Value(transportCtxKey{}).(*transportState)
	return st, ok
}

// newID returns a 128-bit random hex id. It is not a credential.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}

// peerHostPort splits a remote address into host and port.
func peerHostPort(addr net.Addr) (host, port string) {
	if addr == nil {
		return "", ""
	}
	h, p, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String(), ""
	}
	return h, p
}

// resultWord renders an ok/fail result token for a boolean check.
func resultWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "fail"
}

// boundedReason maps an error to a short, bounded, non-secret classifier so log
// records never embed an unbounded or provider-supplied error string in a
// stable field. The raw error is only ever attached as a separate "error" attr
// by callers that need it.
func boundedReason(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.ToLower(err.Error())
	switch {
	case strings.Contains(reason, "timeout") || strings.Contains(reason, "deadline"):
		return "timeout"
	case strings.Contains(reason, "context canceled"):
		return "canceled"
	case strings.Contains(reason, "no such host") || strings.Contains(reason, "unavailable"):
		return "lookup_unavailable"
	}
	if len(reason) > 64 {
		return reason[:64]
	}
	return reason
}
