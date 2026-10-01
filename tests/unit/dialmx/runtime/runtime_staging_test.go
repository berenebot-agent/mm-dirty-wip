package runtime_test

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	receiverruntime "github.com/dellarb/mailmoose/dialmx/runtime"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// rawSMTP is a minimal SMTP client used to hold a DATA transaction open so the
// edge's in-memory staging reservation stays held.
type rawSMTP struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dialSMTP(t *testing.T, addr string) *rawSMTP {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial smtp: %v", err)
	}
	s := &rawSMTP{t: t, conn: c, r: bufio.NewReader(c)}
	s.expect("220")
	s.send("EHLO test")
	for {
		line, err := s.readLine(2 * time.Second)
		if err != nil {
			t.Fatalf("EHLO read: %v", err)
		}
		if strings.HasPrefix(line, "250 ") {
			break
		}
	}
	return s
}

func (s *rawSMTP) send(line string) {
	s.t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := s.conn.Write([]byte(line + "\r\n")); err != nil {
		s.t.Fatalf("send %q: %v", line, err)
	}
}

func (s *rawSMTP) readLine(timeout time.Duration) (string, error) {
	_ = s.conn.SetReadDeadline(time.Now().Add(timeout))
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (s *rawSMTP) expect(prefix string) string {
	s.t.Helper()
	line, err := s.readLine(2 * time.Second)
	if err != nil {
		s.t.Fatalf("expect %q: %v", prefix, err)
	}
	if !strings.HasPrefix(line, prefix) {
		s.t.Fatalf("expect %q, got %q", prefix, line)
	}
	return line
}

func (s *rawSMTP) close() { _ = s.conn.Close() }

// beginData drives MAIL/RCPT/DATA and returns the 354 reply, leaving the client
// responsible for the body.
func (s *rawSMTP) beginData(recipient string) {
	s.t.Helper()
	s.send("MAIL FROM:<sender@outside.test>")
	s.expect("250")
	s.send("RCPT TO:<" + recipient + ">")
	s.expect("250")
	s.send("DATA")
	s.expect("354")
}

// TestIncludedStagingDefaultAdmitsConcurrentMessages proves a zero
// MaxStagingBytes does not regress to a single-message staging budget: with two
// DATA transactions holding a reservation at once, the second is not refused
// with a 451 staging-busy reply. With the one-message floor it would be.
//
// It runs a real core session (a manager with an always-accepting backend) so
// RCPT succeeds; the body is held open on both connections so the staging
// reservation is live on both.
func TestIncludedStagingDefaultAdmitsConcurrentMessages(t *testing.T) {
	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	cfg := receiverruntime.Config{
		Hostname:     "mx.test",
		Secret:       "core-key",
		SessionAddr:  "127.0.0.1:0",
		DrainTimeout: time.Second,
		SMTP: mxagent.Config{
			Hostname:        "mx.test",
			ListenAddr:      "127.0.0.1:0",
			MaxMessageBytes: 1 << 20,
			MaxRecipients:   10,
			MaxConnections:  8,
			DataTimeout:     3 * time.Second,
			// MaxStagingBytes deliberately zero: the runtime must default it to
			// the documented aggregate budget, not a single-message floor.
		},
	}
	st, err := rt.Activate(context.Background(), cfg)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// A real core session makes RCPT resolve so DATA staging is reached.
	be := &drainBackend{}
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

	first := dialSMTP(t, st.SMTPAddr)
	defer first.close()
	first.beginData("a@example.test")
	// Send a partial body and hold the connection open so the reservation is
	// held; no terminator yet.
	if _, err := first.conn.Write([]byte("From: sender@outside.test\r\n\r\npartial body")); err != nil {
		t.Fatalf("partial data: %v", err)
	}
	// Give the server a moment to enter the staging read.
	time.Sleep(200 * time.Millisecond)

	second := dialSMTP(t, st.SMTPAddr)
	defer second.close()
	second.beginData("b@example.test")
	if _, err := second.conn.Write([]byte("From: sender@outside.test\r\n\r\nsecond body")); err != nil {
		t.Fatalf("second partial data: %v", err)
	}
	// The bug surfaced as a 451 staging-busy reply after the 354. With the
	// aggregate default, no reply arrives while the body is held: the read
	// times out.
	line, err := second.readLine(500 * time.Millisecond)
	if err == nil && strings.HasPrefix(line, "451") {
		t.Fatalf("second concurrent DATA was refused with staging-busy: %q", line)
	}
}
