package mxagent

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// connWrapper is the net.Conn returned by the listener wrapper installed in
// ListenAndServe. Every accepted TCP connection is wrapped once, so the edge
// can attach a stable smtp_connection_id, observe connection lifecycle and
// transport byte counts, and record the outcome of the one reply write per DATA
// transaction. It deliberately never inspects or logs raw protocol bytes: Read
// and Write only record byte counts and whether the call failed, never its
// contents.
//
// The wrapper survives STARTTLS: go-smtp wraps it in a *tls.Conn, and
// unwrapConn peels that back so NewSession can recover the same identity.
type connWrapper struct {
	net.Conn
	srv     *Server
	id      string
	peer    string
	local   string
	started time.Time

	// bytesRead/bytesWritten count bytes moved in each direction. Under
	// STARTTLS these are ciphertext bytes (the *tls.Conn sits above), which is
	// why they are labelled transport bytes, never protocol bytes.
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64

	mu sync.Mutex
	// openedLogged and closedLogged ensure exactly one open and one close
	// event per TCP connection even though NewSession runs again after
	// STARTTLS and Close may race with a session.
	openedLogged bool
	closedLogged bool
	// sessionCount counts backend sessions created on this connection: one for
	// the plaintext greeting, and a second after a successful STARTTLS.
	sessionCount int
	// replyProbe, when non-nil, is the reply write the session expects next. It
	// is consumed by the first Write that follows.
	replyProbe *replyProbe
	// lastReadErr and closeReason record the most recent transport read failure
	// and why the connection ended, for the close event.
	lastReadErr string
	closeReason string
}

// replyProbe records the outcome of the one DATA reply write a transaction
// produces after Data returns. It carries only the transaction correlation id,
// never the protocol text.
type replyProbe struct {
	txID string
}

// trackingListener wraps a listener so every accepted connection is a
// connWrapper. srv may be nil in unit tests that exercise the wrapper without a
// full Server.
type trackingListener struct {
	net.Listener
	srv *Server
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	w := newConnWrapper(c, l.srv)
	// Log the open at accept time so every accepted TCP connection has a
	// lifecycle event even if it is dropped before a session is created.
	w.logOpened()
	return w, nil
}

func newConnWrapper(c net.Conn, srv *Server) *connWrapper {
	w := &connWrapper{
		Conn:    c,
		srv:     srv,
		id:      newCorrelationID(),
		started: time.Now(),
	}
	if c != nil {
		w.peer = addrString(c.RemoteAddr())
		w.local = addrString(c.LocalAddr())
	}
	return w
}

// addrString renders a network address, preferring the full host:port so the
// peer port and local endpoint are visible.
func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

// unwrapConn recovers the connWrapper from a go-smtp session's Conn(), peeling
// a *tls.Conn introduced by STARTTLS. It returns nil when the connection was
// not produced by the wrapping listener.
func unwrapConn(c net.Conn) *connWrapper {
	for i := 0; i < 4 && c != nil; i++ {
		switch v := c.(type) {
		case *connWrapper:
			return v
		case *tls.Conn:
			c = v.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// logOpened emits the connection-open lifecycle event exactly once, at accept
// time. The client greeting (HELO) and STARTTLS state are not known yet; they
// appear on the session events.
func (w *connWrapper) logOpened() {
	if w.srv == nil || w.srv.log == nil {
		return
	}
	w.mu.Lock()
	if w.openedLogged {
		w.mu.Unlock()
		return
	}
	w.openedLogged = true
	w.mu.Unlock()
	w.srv.log.Info("mx connection opened",
		AttrConnectionID, w.id,
		"peer", w.peer,
		"local", w.local,
	)
}

// noteSession records that a backend session was created on this connection and
// reports whether this is a post-STARTTLS session, so the caller can emit the
// STARTTLS-established event exactly once.
func (w *connWrapper) noteSession() (count int) {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	w.sessionCount++
	count = w.sessionCount
	w.mu.Unlock()
	return count
}

// armReply arms the reply write probe for one transaction. It is called by Data
// before it returns; go-smtp writes the DATA response immediately afterwards,
// so the next Write is that reply.
func (w *connWrapper) armReply(txID string) {
	if w == nil || txID == "" {
		return
	}
	w.mu.Lock()
	w.replyProbe = &replyProbe{txID: txID}
	w.mu.Unlock()
}

func (w *connWrapper) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	w.bytesRead.Add(int64(n))
	if err != nil {
		// Record every read outcome, including a clean EOF, so the close event
		// can report how the input side ended.
		w.mu.Lock()
		w.lastReadErr = readErrorClass(err)
		w.mu.Unlock()
	}
	return n, err
}

func (w *connWrapper) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	w.bytesWritten.Add(int64(n))
	if w.srv == nil || w.srv.log == nil {
		return n, err
	}
	w.mu.Lock()
	probe := w.replyProbe
	w.replyProbe = nil
	w.mu.Unlock()
	if probe == nil {
		return n, err
	}
	// This event records the outcome of the transport write that carries the
	// SMTP reply. It is NOT a claim that the full DATA response was delivered:
	// under STARTTLS the wrapper only observes the (possibly partial) encrypted
	// first fragment, and even in plaintext the edge must not dump protocol
	// text. ok is a true transport success (all bytes accepted, no error);
	// otherwise error carries a bounded classifier.
	ok := err == nil && n == len(p)
	args := []any{
		AttrConnectionID, w.id,
		AttrTransactionID, probe.txID,
		"ok", ok,
		"short", err == nil && n < len(p),
		"bytes", n,
	}
	if err != nil {
		args = append(args, "error", writeErrorClass(err))
	}
	w.srv.log.Info("mx smtp reply transport write", args...)
	return n, err
}

func (w *connWrapper) Close() error {
	err := w.Conn.Close()
	if w.srv == nil || w.srv.log == nil {
		return err
	}
	w.mu.Lock()
	if w.closedLogged {
		w.mu.Unlock()
		return err
	}
	w.closedLogged = true
	if w.closeReason == "" {
		switch {
		case err != nil:
			w.closeReason = "close_error"
		case w.lastReadErr != "":
			// Derive the close reason from how the input side ended: a clean
			// EOF is a normal peer close, anything else is a transport failure.
			w.closeReason = w.lastReadErr
		default:
			w.closeReason = "closed"
		}
	}
	reason := w.closeReason
	lastRead := w.lastReadErr
	txns := w.sessionCount
	w.mu.Unlock()
	// go-smtp's graceful QUIT closes the connection before it reaches here; the
	// reason is still recorded for a transport failure on close.
	args := []any{
		AttrConnectionID, w.id,
		"peer", w.peer,
		"local", w.local,
		"duration_ms", durationMs(time.Since(w.started)),
		"bytes_read", w.bytesRead.Load(),
		"bytes_written", w.bytesWritten.Load(),
		"sessions", txns,
		"close_reason", reason,
	}
	if lastRead != "" {
		args = append(args, "last_read_error", lastRead)
	}
	w.srv.log.Info("mx connection closed", args...)
	return err
}

// readErrorClass classifies a transport read error into a bounded token.
func readErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	return "read_error"
}

// writeErrorClass classifies a transport write error into a bounded token.
func writeErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.ErrShortWrite):
		return "short_write"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return "timeout"
	}
	return "write_error"
}

// durationMs renders a duration as whole milliseconds for structured logs.
// It is defined here because both the wrapper and the session use it.
func durationMs(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

var _ net.Conn = (*connWrapper)(nil)
