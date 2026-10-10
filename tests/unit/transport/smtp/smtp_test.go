package smtp_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/smtp"
)

func fakeSMTP(t *testing.T) (host string, port int, got <-chan string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
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
					ch <- body.String()
					fmt.Fprint(w, "250 ok\r\n")
					w.Flush()
					data = false
					continue
				}
				body.WriteString(line)
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
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "localhost", a.Port, ch, func() { ln.Close() }
}
func TestPlainSMTPAndPublicRoutableSSRF(t *testing.T) {
	host, port, got, closeFn := fakeSMTP(t)
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw := []byte("From: sender@example.com\r\nTo: dest@example.net\r\nSubject: hi\r\n\r\nbody\r\n")
	if err := smtp.Send(ctx, smtp.Config{Host: host, Port: port, Security: "plain"}, smtp.SendRequest{From: "sender@example.com", To: []string{"dest@example.net"}, Raw: raw}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if !strings.Contains(b, "Subject: hi") {
			t.Fatalf("body %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	if err := smtp.Send(ctx, smtp.Config{Host: "127.0.0.1", Port: port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, true); err == nil || !strings.Contains(err.Error(), "public-routable") {
		t.Fatalf("private SMTP should reject when public is required: %v", err)
	}
	if err := smtp.Send(ctx, smtp.Config{Host: "localhost", Port: port, Security: "bogus"}, smtp.SendRequest{}, false); err == nil {
		t.Fatal("invalid security accepted")
	}
}

// fakeSMTPReject is a fake server that rejects RCPT with the given reply.
func fakeSMTPReject(t *testing.T, rcptReply string) (host string, port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := bufio.NewWriter(c)
		fmt.Fprint(w, "220 test ESMTP\r\n")
		w.Flush()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch upper := strings.ToUpper(line); {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				fmt.Fprint(w, "250-test\r\n250 OK\r\n")
			case strings.HasPrefix(upper, "MAIL FROM"):
				fmt.Fprint(w, "250 ok\r\n")
			case strings.HasPrefix(upper, "RCPT TO"):
				fmt.Fprint(w, rcptReply)
			case strings.HasPrefix(upper, "QUIT"):
				fmt.Fprint(w, "221 bye\r\n")
				w.Flush()
				return
			default:
				fmt.Fprint(w, "250 ok\r\n")
			}
			w.Flush()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "localhost", a.Port, func() { ln.Close() }
}

// TestSMTPFiveHundredIsPermanent proves a 5xx reply is classified as a
// permanent error (so the outbox fails it instead of retrying for hours).
func TestSMTPFiveHundredIsPermanent(t *testing.T) {
	host, port, closeFn := fakeSMTPReject(t, "550 5.1.1 user unknown\r\n")
	defer closeFn()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	err := smtp.Send(context.Background(), smtp.Config{Host: host, Port: port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false)
	if err == nil {
		t.Fatal("5xx rejection was not an error")
	}
	if !transport.IsPermanent(err) {
		t.Fatalf("5xx rejection not classified permanent: %v", err)
	}
}

// TestSMTPFourHundredIsRetryable proves a 4xx (transient) reply is not
// classified as permanent.
func TestSMTPFourHundredIsRetryable(t *testing.T) {
	host, port, closeFn := fakeSMTPReject(t, "451 4.3.0 try later\r\n")
	defer closeFn()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	err := smtp.Send(context.Background(), smtp.Config{Host: host, Port: port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false)
	if err == nil {
		t.Fatal("4xx rejection was not an error")
	}
	if transport.IsPermanent(err) {
		t.Fatalf("4xx rejection wrongly classified permanent: %v", err)
	}
}

func TestOutboundAdapterRequirePublicAndRawMIME(t *testing.T) {
	host, port, got, closeFn := fakeSMTP(t)
	defer closeFn()
	smtp.SetRequirePublic(false)
	defer smtp.SetRequirePublic(false)
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\n-- attachment body --\r\n")
	if err := smtp.Send(context.Background(), smtp.Config{Host: host, Port: port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-got:
		if !strings.Contains(body, "attachment body") {
			t.Fatalf("raw mime not delivered: %q", body)
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
	smtp.SetRequirePublic(true)
	err := smtp.Send(context.Background(), smtp.Config{Host: "127.0.0.1", Port: 25, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, true)
	if err == nil || !strings.Contains(err.Error(), "public-routable") {
		t.Fatalf("require-public not applied: %v", err)
	}
}

// TestSMTPAmbiguousAfterData proves a failure at or after the message body was
// uploaded (here the server drops the connection after receiving the terminating
// dot but before replying) is classified as an ambiguous send outcome, never a
// plain retryable failure, so the outbox does not blindly retry a send the server
// may already have accepted.
func TestSMTPAmbiguousAfterData(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		fmt.Fprint(c, "220 test ESMTP\r\n")
		data := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if data {
				if line == ".\r\n" {
					// Drop the connection without sending the verdict: the
					// message body was received, so the outcome is unknown.
					return
				}
				continue
			}
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "DATA") {
				fmt.Fprint(c, "354 go\r\n")
				data = true
			} else {
				fmt.Fprint(c, "250 ok\r\n")
			}
		}
	}()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	err = smtp.Send(context.Background(), smtp.Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false)
	if err == nil {
		t.Fatal("dropped verdict was not an error")
	}
	if !transport.AsAmbiguous(err) {
		t.Fatalf("post-DATA failure not classified ambiguous: %v", err)
	}
	if transport.IsPermanent(err) {
		t.Fatalf("ambiguous error wrongly classified permanent: %v", err)
	}
	<-done
}

// fakeSMTPAuth is a fake server advertising AUTH PLAIN and expecting the given
// credentials. It records the negotiation so a test can prove no mail was sent.
func fakeSMTPAuth(t *testing.T, user, pass string, sent *bool, authSeen *bool) (host string, port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := bufio.NewWriter(c)
		fmt.Fprint(w, "220 test ESMTP\r\n")
		w.Flush()
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					fmt.Fprint(w, "250 ok\r\n")
					w.Flush()
					inData = false
				}
				continue
			}
			upper := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				fmt.Fprint(w, "250-test\r\n250-AUTH PLAIN\r\n250 OK\r\n")
			case strings.HasPrefix(upper, "AUTH PLAIN"):
				// The initial-response form: AUTH PLAIN <base64>.
				fields := strings.Fields(strings.TrimSpace(line))
				if len(fields) < 3 {
					// No initial response: prompt for it.
					fmt.Fprint(w, "334 \r\n")
					w.Flush()
					line, err = r.ReadString('\n')
					if err != nil {
						return
					}
					fields = append(fields, strings.TrimSpace(line))
				}
				raw, derr := base64.StdEncoding.DecodeString(fields[len(fields)-1])
				if derr != nil || string(raw) != "\x00"+user+"\x00"+pass {
					fmt.Fprint(w, "535 5.7.8 bad credentials\r\n")
					w.Flush()
					continue
				}
				*authSeen = true
				fmt.Fprint(w, "235 2.7.0 ok\r\n")
			case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
				*sent = true
				fmt.Fprint(w, "250 ok\r\n")
			case strings.HasPrefix(upper, "DATA"):
				*sent = true
				fmt.Fprint(w, "354 end\r\n")
				inData = true
			case strings.HasPrefix(upper, "QUIT"):
				fmt.Fprint(w, "221 bye\r\n")
				w.Flush()
				return
			default:
				fmt.Fprint(w, "250 ok\r\n")
			}
			w.Flush()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "localhost", a.Port, func() { ln.Close() }
}

// TestSMTPPlainAuthExplicit proves the operator-selected plaintext security mode
// can authenticate with an explicit PLAIN implementation (net/smtp.PlainAuth
// would refuse a non-TLS link), while Verify validates the same binding without
// sending any mail.
func TestSMTPPlainAuthExplicit(t *testing.T) {
	var sent, authSeen bool
	host, port, closeFn := fakeSMTPAuth(t, "user@test", "secret", &sent, &authSeen)
	defer closeFn()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	cfg := smtp.Config{Host: host, Port: port, Security: "plain", Username: "user@test", Password: "secret"}
	if err := smtp.Send(context.Background(), cfg, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false); err != nil {
		t.Fatalf("plain auth send: %v", err)
	}
	if !authSeen || !sent {
		t.Fatalf("plain auth not exercised: authSeen=%v sent=%v", authSeen, sent)
	}

	// Verify authenticates and never issues MAIL FROM/RCPT/DATA.
	var sent2, auth2 bool
	host2, port2, closeFn2 := fakeSMTPAuth(t, "user@test", "secret", &sent2, &auth2)
	defer closeFn2()
	cfg2 := smtp.Config{Host: host2, Port: port2, Security: "plain", Username: "user@test", Password: "secret"}
	if err := smtp.Verify(context.Background(), cfg2, false); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !auth2 {
		t.Fatal("verify did not authenticate")
	}
	if sent2 {
		t.Fatal("verify sent mail (MAIL FROM/RCPT/DATA issued)")
	}
}

// TestSMTPStartTLSRefusesClearAuth proves the stronger security modes never fall
// back to sending credentials in clear: a server that does not offer STARTTLS is
// refused rather than authenticated in the clear.
func TestSMTPStartTLSRefusesClearAuth(t *testing.T) {
	var sent, authSeen bool
	host, port, closeFn := fakeSMTPAuth(t, "user@test", "secret", &sent, &authSeen)
	defer closeFn()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	err := smtp.Send(context.Background(), smtp.Config{Host: host, Port: port, Security: "starttls", Username: "user@test", Password: "secret"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false)
	if err == nil {
		t.Fatal("starttls without server support unexpectedly succeeded")
	}
	if authSeen {
		t.Fatal("credentials sent in clear because STARTTLS was unavailable")
	}
}

func TestCancellationUnblocksFinalDATAReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		fmt.Fprint(c, "220 test ESMTP\r\n")
		data := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if data {
				// Deliberately withhold the final DATA verdict.
				continue
			}
			if strings.HasPrefix(line, "DATA") {
				fmt.Fprint(c, "354 go\r\n")
				data = true
			} else {
				fmt.Fprint(c, "250 ok\r\n")
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- smtp.Send(ctx, smtp.Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: []byte("Subject: hi\r\n\r\nbody\r\n")}, false)
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled DATA unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not unblock DATA")
	}
	<-done
}

// TestSMTPFourHundredAfterDataIsRetryable proves a definitive 4xx reply to the
// terminating dot is a clean, retryable failure, NOT an ambiguous outcome. Only a
// lost reply (no numeric code) is ambiguous; a delivered 4xx means the server
// explicitly rejected the message, so the outbox may safely retry with backoff.
func TestSMTPFourHundredAfterDataIsRetryable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := bufio.NewWriter(c)
		fmt.Fprint(w, "220 test ESMTP\r\n")
		w.Flush()
		data := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if data {
				if line == ".\r\n" {
					// A definitive transient rejection after the body.
					fmt.Fprint(w, "451 4.7.1 greylisted, try later\r\n")
					w.Flush()
					data = false
					continue
				}
				continue
			}
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "DATA") {
				fmt.Fprint(w, "354 go\r\n")
				data = true
			} else if strings.HasPrefix(upper, "QUIT") {
				fmt.Fprint(w, "221 bye\r\n")
				w.Flush()
				return
			} else {
				fmt.Fprint(w, "250 ok\r\n")
			}
			w.Flush()
		}
	}()
	raw := []byte("From: a@b.test\r\nTo: c@d.test\r\nSubject: hi\r\n\r\nbody\r\n")
	err = smtp.Send(context.Background(), smtp.Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "plain"}, smtp.SendRequest{From: "a@b.test", To: []string{"c@d.test"}, Raw: raw}, false)
	if err == nil {
		t.Fatal("4xx after DATA unexpectedly succeeded")
	}
	if transport.AsAmbiguous(err) {
		t.Fatalf("definitive 4xx after DATA wrongly classified ambiguous: %v", err)
	}
	if transport.IsPermanent(err) {
		t.Fatalf("4xx after DATA wrongly classified permanent: %v", err)
	}
	<-done
}
