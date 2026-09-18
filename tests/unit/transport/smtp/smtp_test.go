package smtp_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

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
func TestPlainSMTPAndHostedSSRF(t *testing.T) {
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
		t.Fatalf("hosted private SMTP should reject: %v", err)
	}
	if err := smtp.Send(ctx, smtp.Config{Host: "localhost", Port: port, Security: "bogus"}, smtp.SendRequest{}, false); err == nil {
		t.Fatal("invalid security accepted")
	}
}

func TestOutboundAdapterHostedFlagAndRawMIME(t *testing.T) {
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
		t.Fatalf("hosted flag not applied: %v", err)
	}
}
