package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	smtpstd "net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type Config struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Security   string `json:"security,omitempty"`
	FromDomain string `json:"from_domain,omitempty"`
}

type SendRequest struct {
	From string
	To   []string
	Raw  []byte
}

type outboundTransport struct{}

func init() { transport.RegisterOutbound(outboundTransport{}) }

func SetRequirePublic(v bool) { netutil.SetRequirePublic(v) }

func (outboundTransport) Name() string        { return "smtp" }
func (outboundTransport) Description() string { return "SMTP" }
func (outboundTransport) PreferRawMIME() bool { return true }
func (outboundTransport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "host", Label: "SMTP host", Type: "text", Required: true, Placeholder: "smtp.example.com"},
		{Name: "port", Label: "Port", Type: "number", Default: "587"},
		{Name: "username", Label: "Username", Type: "text"},
		{Name: "password", Label: "Password", Type: "password", Secret: true},
		{Name: "security", Label: "Security", Type: "select", Default: "starttls", Options: []transport.ConfigOption{{Value: "starttls", Label: "STARTTLS"}, {Value: "tls", Label: "TLS"}, {Value: "plain", Label: "Plain"}}},
		{Name: "from_domain", Label: "From domain", Type: "text", Placeholder: "example.com"},
	}
}
func (outboundTransport) Send(ctx context.Context, cfg map[string]any, m transport.OutboundMessage) (transport.OutboundResult, error) {
	var c Config
	if err := transport.DecodeOutboundConfig(cfg, &c); err != nil {
		return transport.OutboundResult{}, err
	}
	all := append(append(append([]string{}, m.To...), m.CC...), m.BCC...)
	if err := Send(ctx, c, SendRequest{From: m.FromAddress, To: all, Raw: m.RawMIME}, netutil.RequirePublic()); err != nil {
		return transport.OutboundResult{}, err
	}
	return transport.OutboundResult{ProviderMessageID: "smtp"}, nil
}

func Send(ctx context.Context, c Config, m SendRequest, requirePublic bool) error {
	if c.Host == "" {
		return fmt.Errorf("smtp host required")
	}
	if c.Port == 0 {
		c.Port = 587
	}
	sec := strings.ToLower(strings.TrimSpace(c.Security))
	if sec == "" {
		sec = "starttls"
	}
	if sec != "starttls" && sec != "tls" && sec != "plain" {
		return fmt.Errorf("smtp security must be starttls, tls, or plain")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", c.Host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("smtp host resolved no addresses")
	}
	var last error
	for _, ip := range ips {
		if requirePublic && !netutil.PublicIP(ip) {
			last = fmt.Errorf("smtp destination is not public-routable")
			continue
		}
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(c.Port))
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			last = err
			continue
		}
		// DialContext only governs establishment; cancellation must also unblock
		// DATA writes and the final reply on the established socket.
		rawConn := conn
		stopCancellation := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
		// A per-command deadline, refreshed before each phase. It is cleared
		// before the body upload so a large message is not killed by a
		// total-transaction timer (which could report failure after the remote
		// already accepted the message, causing a duplicate on retry).
		setPhaseDeadline := func() { _ = conn.SetDeadline(time.Now().Add(30 * time.Second)) }
		clearDeadline := func() { _ = conn.SetDeadline(time.Time{}) }
		setPhaseDeadline()
		if sec == "tls" {
			conn = tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		}
		cl, err := smtpstd.NewClient(conn, c.Host)
		if err != nil {
			stopCancellation()
			conn.Close()
			last = classifySMTPError(err)
			if transport.IsPermanent(last) {
				return last
			}
			continue
		}
		ok := false
		// dataStarted records that the message body upload has begun. Once it has,
		// any transport failure is ambiguous: the server may already have accepted
		// the message, so retrying on another address could deliver a duplicate. It
		// is reported as an AmbiguousError (never a plain retryable failure).
		dataStarted := false
		func() {
			defer stopCancellation()
			defer cl.Close()
			if sec == "starttls" {
				if okExt, _ := cl.Extension("STARTTLS"); !okExt {
					last = fmt.Errorf("smtp server does not offer STARTTLS")
					return
				}
				setPhaseDeadline()
				if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
					last = classifySMTPError(err)
					return
				}
			}
			setPhaseDeadline()
			if c.Username != "" {
				auth := smtpAuth(sec, c)
				if err := cl.Auth(auth); err != nil {
					last = classifySMTPError(err)
					return
				}
			}
			setPhaseDeadline()
			if err := cl.Mail(m.From); err != nil {
				last = classifySMTPError(err)
				return
			}
			for _, rcpt := range m.To {
				setPhaseDeadline()
				if err := cl.Rcpt(rcpt); err != nil {
					last = classifySMTPError(err)
					return
				}
			}
			setPhaseDeadline()
			wc, err := cl.Data()
			if err != nil {
				last = classifySMTPError(err)
				return
			}
			// The body upload is bounded only by the caller's context. From this
			// point the message may be accepted, so a failure is ambiguous.
			clearDeadline()
			dataStarted = true
			bw := bufio.NewWriter(wc)
			if _, err = bw.Write(m.Raw); err == nil {
				err = bw.Flush()
			}
			cerr := wc.Close()
			if err != nil {
				last = ambiguousDataError(err)
				return
			}
			// wc.Close() sends the terminating dot and reads the server's
			// verdict: a 2xx means the remote accepted the message, so a
			// subsequent QUIT failure must not be reported as a delivery
			// failure (it would trigger a duplicate).
			if cerr != nil {
				if isSMTPReply(cerr) {
					// The server returned a definitive reply after the body: it
					// either rejected the message (4xx transient or 5xx permanent)
					// or is reporting the success path's own post-DATA state. That
					// reply is authoritative and the message was NOT accepted, so
					// this is a clean, non-ambiguous outcome: a 5xx is permanent, a
					// 4xx is retryable. Only a lost reply (connection dropped or
					// timed out before any code arrived) is genuinely ambiguous.
					last = classifySMTPError(cerr)
					dataStarted = false
					return
				}
				last = ambiguousDataError(cerr)
				return
			}
			setPhaseDeadline()
			_ = cl.Quit()
			ok = true
		}()
		if ok {
			return nil
		}
		// A failure after the body upload began is ambiguous: do not retry on
		// another address or IP, because the message may already be accepted.
		if dataStarted {
			return last
		}
		// A permanent (5xx) rejection will never succeed on another address:
		// stop trying the remaining IPs and let the worker fail the message.
		if transport.IsPermanent(last) {
			return last
		}
	}
	if last == nil {
		last = fmt.Errorf("no usable smtp destination")
	}
	return last
}

// Verify validates that an SMTP binding can be reached and authenticated without
// sending any mail: it dials, negotiates TLS/STARTTLS, and — when a username is
// set — performs AUTH, then QUIT. It never issues MAIL FROM, RCPT TO or DATA, so
// no test message is ever delivered. It applies the same destination policy as
// Send (requirePublic) and the same explicit-plaintext rules.
func Verify(ctx context.Context, c Config, requirePublic bool) error {
	if c.Host == "" {
		return fmt.Errorf("smtp host required")
	}
	if c.Port == 0 {
		c.Port = 587
	}
	sec := strings.ToLower(strings.TrimSpace(c.Security))
	if sec == "" {
		sec = "starttls"
	}
	if sec != "starttls" && sec != "tls" && sec != "plain" {
		return fmt.Errorf("smtp security must be starttls, tls, or plain")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", c.Host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("smtp host resolved no addresses")
	}
	var last error
	for _, ip := range ips {
		if requirePublic && !netutil.PublicIP(ip) {
			last = fmt.Errorf("smtp destination is not public-routable")
			continue
		}
		addr := net.JoinHostPort(ip.String(), strconv.Itoa(c.Port))
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			last = err
			continue
		}
		rawConn := conn
		stopCancellation := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
		setPhaseDeadline := func() { _ = conn.SetDeadline(time.Now().Add(30 * time.Second)) }
		setPhaseDeadline()
		if sec == "tls" {
			conn = tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		}
		cl, err := smtpstd.NewClient(conn, c.Host)
		if err != nil {
			stopCancellation()
			conn.Close()
			last = classifySMTPError(err)
			if transport.IsPermanent(last) {
				return last
			}
			continue
		}
		ok := false
		func() {
			defer stopCancellation()
			defer cl.Close()
			if sec == "starttls" {
				if okExt, _ := cl.Extension("STARTTLS"); !okExt {
					last = fmt.Errorf("smtp server does not offer STARTTLS")
					return
				}
				setPhaseDeadline()
				if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
					last = classifySMTPError(err)
					return
				}
			}
			setPhaseDeadline()
			if c.Username != "" {
				if okExt, _ := cl.Extension("AUTH"); !okExt {
					last = fmt.Errorf("smtp server does not offer AUTH")
					return
				}
				if err := cl.Auth(smtpAuth(sec, c)); err != nil {
					last = classifySMTPError(err)
					return
				}
			}
			setPhaseDeadline()
			_ = cl.Quit()
			ok = true
		}()
		if ok {
			return nil
		}
		if transport.IsPermanent(last) {
			return last
		}
	}
	if last == nil {
		last = fmt.Errorf("no usable smtp destination")
	}
	return last
}

// classifySMTPError wraps a 5xx SMTP reply as a permanent error so the outbox
// fails it immediately instead of retrying with backoff. It mirrors the MX
// transport's classification.
func classifySMTPError(err error) error {
	if e, ok := err.(*textproto.Error); ok && e.Code >= 500 && e.Code < 600 {
		return &transport.PermanentError{Err: err}
	}
	return err
}

// isSMTPReply reports whether err is a definitive SMTP protocol reply (a
// *textproto.Error carries a numeric code). A reply is authoritative: the
// server decided the message's fate, so the outcome is not ambiguous. A raw
// transport error (EOF, timeout, connection reset) has no code and is the
// genuinely unknown case.
func isSMTPReply(err error) bool {
	var e *textproto.Error
	return errors.As(err, &e)
}

// smtpAuth selects the authentication mechanism for a connection. TLS and
// STARTTLS use the standard library's PLAIN auth, which refuses to send
// credentials over an unencrypted link. The explicit "plain" security mode is a
// deliberate operator opt-in to an unencrypted transport (for example a
// self-hosted relay on a private network): only then is an explicit PLAIN auth
// used, so credentials are never silently sent in clear on a connection the
// operator did not explicitly mark as plaintext. There is no downgrade path:
// a "tls"/"starttls" connection never falls back to clear auth.
func smtpAuth(sec string, c Config) smtpstd.Auth {
	if sec == "plain" {
		return &plainAuth{username: c.Username, password: c.Password}
	}
	return smtpstd.PlainAuth("", c.Username, c.Password, c.Host)
}

// plainAuth is an explicit SMTP AUTH PLAIN implementation for the operator-chosen
// plaintext security mode. Unlike net/smtp.PlainAuth it does not itself refuse a
// non-TLS link, because the operator explicitly selected plain.
type plainAuth struct {
	username string
	password string
}

func (a *plainAuth) Start(*smtpstd.ServerInfo) (string, []byte, error) {
	resp := []byte("\x00" + a.username + "\x00" + a.password)
	return "PLAIN", resp, nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("unexpected server challenge")
	}
	return nil, nil
}

// ambiguousDataError wraps a failure that occurred at or after the message body
// upload began (a DATA write/close timeout or a dropped connection while awaiting
// the server's verdict). The message may already have been accepted, so it is
// reported as an ambiguous outcome: the outbox records the ambiguity rather than
// blindly retrying, which could deliver a duplicate.
func ambiguousDataError(err error) error {
	return &transport.AmbiguousError{Err: fmt.Errorf("smtp: outcome unknown after the message body was sent: %w", err)}
}
