package mx_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/transport"
	"github.com/dellarb/mammothmail/internal/transport/mx"
)

type resolver struct {
	mx  []*net.MX
	mxe error
	ips map[string][]net.IP
}

func (r resolver) LookupMX(context.Context, string) ([]*net.MX, error) { return r.mx, r.mxe }
func (r resolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	return r.ips[host], nil
}

// fakeServerOpts controls the fake MX SMTP server's behaviour around the end of
// the DATA transaction, so tests can reproduce the failure modes seen in
// production.
type fakeServerOpts struct {
	// dropBeforeQuit closes the connection after accepting the message with a
	// 250, before (or instead of) answering QUIT.
	dropBeforeQuit bool
	// bodyDelay is slept after the terminating dot is seen and before the 250
	// verdict is sent, simulating a slow upload/large message.
	bodyDelay time.Duration
	// verdict replaces the post-DATA "250 ok" (an authoritative reply, e.g.
	// "550 no" or "451 try later").
	verdict string
	// dropBeforeVerdict closes the connection immediately after the
	// terminating dot, with no verdict sent: the remote's decision is lost.
	dropBeforeVerdict bool
	// dropMidBody closes the connection while the message body is still being
	// uploaded, before the terminating dot.
	dropMidBody bool
	// onConn is called with the connection count after each accepted
	// connection, so tests can observe how many addresses were tried.
	onConn func(int)
}

// fakeServer starts a single-connection SMTP server on loopback. It returns the
// host, port and the received message body channel, and a close func.
func fakeServer(t *testing.T, opts fakeServerOpts) (host string, port int, got <-chan string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan string, 1)
	cons := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			cons++
			if opts.onConn != nil {
				opts.onConn(cons)
			}
			go serveFakeConn(c, opts, ch)
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "localhost", a.Port, ch, func() { ln.Close() }
}

func serveFakeConn(c net.Conn, opts fakeServerOpts, ch chan<- string) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	fmt.Fprint(w, "220 test ESMTP\r\n")
	w.Flush()
	data := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if data {
			if line == ".\r\n" {
				if opts.bodyDelay > 0 {
					time.Sleep(opts.bodyDelay)
				}
				ch <- body.String()
				if opts.dropBeforeVerdict {
					return
				}
				verdict := opts.verdict
				if verdict == "" {
					verdict = "250 ok"
				}
				fmt.Fprint(w, verdict+"\r\n")
				w.Flush()
				if opts.dropBeforeQuit {
					return
				}
				data = false
				continue
			}
			body.WriteString(line)
			if opts.dropMidBody {
				return
			}
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			fmt.Fprint(w, "250-test\r\n250 OK\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			fmt.Fprint(w, "250 ok\r\n")
		case strings.HasPrefix(upper, "DATA"):
			fmt.Fprint(w, "354 end\r\n")
			data = true
		case strings.HasPrefix(upper, "QUIT"):
			fmt.Fprint(w, "221 bye\r\n")
			w.Flush()
			return
		default:
			fmt.Fprint(w, "250 ok\r\n")
		}
		w.Flush()
	}
}

// loopbackResolver resolves any host to loopback so a fake SMTP server can be
// targeted. The delivery port is overridden via SetPortForTest.
type loopbackResolver struct{}

func (loopbackResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Host: "localhost.", Pref: 10}}, nil
}
func (loopbackResolver) LookupIP(context.Context, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("127.0.0.1")}, nil
}

func TestDirectMXRequiresOneRecipientAndHELO(t *testing.T) {
	base := transport.OutboundMessage{FromAddress: "sender@example.com", To: []string{"a@example.net"}, RawMIME: []byte("Subject: hi\r\n\r\nbody\r\n")}
	for name, msg := range map[string]transport.OutboundMessage{
		"multiple": {FromAddress: base.FromAddress, To: []string{"a@example.net", "b@example.net"}, RawMIME: base.RawMIME},
		"none":     {FromAddress: base.FromAddress, RawMIME: base.RawMIME},
	} {
		t.Run(name, func(t *testing.T) {
			err := mx.Send(context.Background(), mx.OutboundConfig{HELO: "mail.example.com"}, msg, false, resolver{})
			if err == nil {
				t.Fatal("expected recipient validation error")
			}
		})
	}
	if err := mx.Send(context.Background(), mx.OutboundConfig{}, base, false, resolver{}); err == nil || !strings.Contains(err.Error(), "HELO") {
		t.Fatalf("HELO validation error = %v", err)
	}
}

func TestDirectMXTargetsPreferMXAndFallbackToA(t *testing.T) {
	ctx := context.Background()
	targets, err := mx.ResolveTargets(ctx, resolver{mx: []*net.MX{{Host: "low.example.", Pref: 20}, {Host: "high.example.", Pref: 10}}}, "Example.NET")
	if err != nil || strings.Join(targets, ",") != "high.example,low.example" {
		t.Fatalf("targets = %v, err = %v", targets, err)
	}
	targets, err = mx.ResolveTargets(ctx, resolver{mxe: &net.DNSError{IsNotFound: true}}, "example.net")
	if err != nil || strings.Join(targets, ",") != "example.net" {
		t.Fatalf("fallback targets = %v, err = %v", targets, err)
	}
}

// TestDirectMXAcceptsWhenQuitFails reproduces the production bug where the
// remote accepted the message (250 after the terminating dot) but the local
// call reported failure because QUIT did not complete. A delivered message must
// not be reported as failed (that would trigger a duplicate retry).
func TestDirectMXAcceptsWhenQuitFails(t *testing.T) {
	host, port, got, closeFn := fakeServer(t, fakeServerOpts{dropBeforeQuit: true})
	defer closeFn()
	mx.SetPortForTest(port)
	t.Cleanup(func() { mx.SetPortForTest(25) })

	raw := []byte("Subject: big\r\n\r\nbody\r\n")
	msg := transport.OutboundMessage{
		FromAddress: "sender@example.com",
		To:          []string{"dest@example.net"},
		RawMIME:     raw,
	}
	if err := mx.Send(context.Background(), mx.OutboundConfig{HELO: "mail.example.com"}, msg, false, loopbackResolver{}); err != nil {
		t.Fatalf("accepted message reported as failure: %v", err)
	}
	select {
	case b := <-got:
		if !strings.Contains(b, "Subject: big") {
			t.Fatalf("body %q", b)
		}
	case <-time.After(2 * time.Second):
		_ = host
		t.Fatal("server did not receive the message")
	}
}

// TestDirectMXSlowBodyNotCutByPhaseTimer proves the body upload is not bounded
// by the per-command SMTP deadline: a body that takes longer than the phase
// timeout still delivers, because the phase timer is cleared for the upload.
func TestDirectMXSlowBodyNotCutByPhaseTimer(t *testing.T) {
	host, port, got, closeFn := fakeServer(t, fakeServerOpts{bodyDelay: 300 * time.Millisecond})
	defer closeFn()
	mx.SetPortForTest(port)
	t.Cleanup(func() { mx.SetPortForTest(25) })
	// A phase timeout far shorter than the body delay. Before the fix a single
	// whole-transaction deadline of this length would kill the upload.
	mx.SetPhaseTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() { mx.SetPhaseTimeoutForTest(45 * time.Second) })

	raw := []byte("Subject: slow\r\n\r\nbody\r\n")
	msg := transport.OutboundMessage{
		FromAddress: "sender@example.com",
		To:          []string{"dest@example.net"},
		RawMIME:     raw,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mx.Send(ctx, mx.OutboundConfig{HELO: "mail.example.com"}, msg, false, loopbackResolver{}); err != nil {
		t.Fatalf("slow body send failed: %v", err)
	}
	select {
	case b := <-got:
		if !strings.Contains(b, "Subject: slow") {
			t.Fatalf("body %q", b)
		}
	case <-time.After(2 * time.Second):
		_ = host
		t.Fatal("server did not receive the message")
	}
}

// --- post-DATA outcome classification ---

// The post-DATA fixture tests below intentionally run serially because they
// mutate the package-global SMTP port override through SetPortForTest.
// ambiguousSend drives a Send attempt and asserts the outcome classification:
// an ambiguous result means the remote's decision was lost (no retry, possible
// duplicate); a definitive post-body reply (4xx/5xx) is authoritative.
func ambiguousSend(t *testing.T, opts fakeServerOpts, cfg mx.OutboundConfig, rl mx.MXResolver) error {
	t.Helper()
	_, port, _, closeFn := fakeServer(t, opts)
	defer closeFn()
	mx.SetPortForTest(port)
	t.Cleanup(func() { mx.SetPortForTest(25) })
	msg := transport.OutboundMessage{
		FromAddress: "sender@example.com",
		To:          []string{"dest@example.net"},
		RawMIME:     []byte("Subject: x\r\n\r\nbody\r\n"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return mx.Send(ctx, cfg, msg, false, rl)
}

// TestDirectMXLostVerdictIsAmbiguous covers the connection dying after the
// terminating dot but before any verdict: the remote may have accepted the
// message, so the outcome is unknown. The send must be reported as ambiguous,
// not retryable.
func TestDirectMXLostVerdictIsAmbiguous(t *testing.T) {
	err := ambiguousSend(t, fakeServerOpts{dropBeforeVerdict: true}, mx.OutboundConfig{HELO: "mail.example.com"}, loopbackResolver{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !transport.AsAmbiguous(err) {
		t.Fatalf("lost verdict = %v, want ambiguous", err)
	}
	if transport.IsPermanent(err) {
		t.Fatalf("lost verdict = %v, must not be permanent", err)
	}
}

// TestDirectMXMidBodyDropIsAmbiguous covers the connection dying while the
// body is still uploading. The mammothmail convention treats any failure from
// the start of the body upload as ambiguous so the two transports (Direct MX
// and SMTP) behave identically.
func TestDirectMXMidBodyDropIsAmbiguous(t *testing.T) {
	err := ambiguousSend(t, fakeServerOpts{dropMidBody: true}, mx.OutboundConfig{HELO: "mail.example.com"}, loopbackResolver{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !transport.AsAmbiguous(err) {
		t.Fatalf("mid-body drop = %v, want ambiguous", err)
	}
}

// TestDirectMXDefinitivePostBodyReplyIsNotAmbiguous covers the remote actually
// answering after the body: a definitive SMTP reply decides the message's
// fate, so it is not ambiguous. 5xx is permanent; 4xx stays retryable.
func TestDirectMXDefinitivePostBodyReplyIsNotAmbiguous(t *testing.T) {
	err := ambiguousSend(t, fakeServerOpts{verdict: "550 recipient rejected"}, mx.OutboundConfig{HELO: "mail.example.com"}, loopbackResolver{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if transport.AsAmbiguous(err) {
		t.Fatalf("definitive reply = %v, must not be ambiguous", err)
	}
	if !transport.IsPermanent(err) {
		t.Fatalf("550 reply = %v, want permanent", err)
	}
	err = ambiguousSend(t, fakeServerOpts{verdict: "451 try later"}, mx.OutboundConfig{HELO: "mail.example.com"}, loopbackResolver{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if transport.AsAmbiguous(err) || transport.IsPermanent(err) {
		t.Fatalf("451 reply = %v, want plain retryable", err)
	}
}

// TestDirectMXAmbiguousOutcomeIsNotRetriedOnAnotherIP proves the Send loop
// stops the whole attempt at the first ambiguous outcome: retrying the message
// on any other address could deliver a duplicate.
func TestDirectMXAmbiguousOutcomeIsNotRetriedOnAnotherIP(t *testing.T) {
	conns := make(chan int, 8)
	rl := resolver{
		mx: []*net.MX{{Host: "localhost.", Pref: 10}},
		ips: map[string][]net.IP{
			"localhost": {net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1")},
		},
	}
	err := ambiguousSend(t, fakeServerOpts{
		dropBeforeVerdict: true,
		onConn:            func(n int) { conns <- n },
	}, mx.OutboundConfig{HELO: "mail.example.com"}, rl)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !transport.AsAmbiguous(err) {
		t.Fatalf("lost verdict = %v, want ambiguous", err)
	}
	select {
	case n := <-conns:
		if n != 1 {
			t.Fatalf("connections tried = %d, want exactly 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server saw no connection")
	}
	select {
	case n := <-conns:
		t.Fatalf("unexpected retry connection #%d", n)
	default:
	}
}
