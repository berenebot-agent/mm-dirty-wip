package mx

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type OutboundConfig struct {
	HELO string `json:"helo"`
}

type MXResolver interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
	LookupIP(context.Context, string) ([]net.IP, error)
}

type systemResolver struct{}

func (systemResolver) LookupMX(ctx context.Context, domain string) ([]*net.MX, error) {
	return net.DefaultResolver.LookupMX(ctx, domain)
}
func (systemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

type outboundTransport struct{}

func init() { transport.RegisterOutbound(outboundTransport{}) }

func (outboundTransport) Name() string               { return Provider }
func (outboundTransport) Description() string        { return "Direct MX" }
func (outboundTransport) PreferRawMIME() bool        { return true }
func (outboundTransport) MaxEnvelopeRecipients() int { return 1 }
func (outboundTransport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{{Name: "helo", Label: "HELO hostname", Type: "text", Required: true, Placeholder: "mail.example.com"}}
}

func (outboundTransport) Send(ctx context.Context, cfg map[string]any, m transport.OutboundMessage) (transport.OutboundResult, error) {
	var c OutboundConfig
	if err := transport.DecodeOutboundConfig(cfg, &c); err != nil {
		return transport.OutboundResult{}, err
	}
	if err := Send(ctx, c, m, netutil.RequirePublic(), systemResolver{}); err != nil {
		return transport.OutboundResult{}, err
	}
	return transport.OutboundResult{ProviderMessageID: "mx"}, nil
}

func Send(ctx context.Context, c OutboundConfig, m transport.OutboundMessage, requirePublic bool, resolver MXResolver) error {
	if err := validate(c, m); err != nil {
		return err
	}
	from, err := mail.ParseAddress(m.FromAddress)
	if err != nil || from.Address != m.FromAddress {
		return fmt.Errorf("mx sender address is invalid")
	}
	recipient := envelopeRecipients(m)[0]
	rcpt, err := mail.ParseAddress(recipient)
	if err != nil || rcpt.Address != recipient {
		return fmt.Errorf("mx recipient address is invalid")
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(strings.SplitN(rcpt.Address, "@", 2)[1]), "."))
	targets, err := mxTargets(ctx, resolver, domain)
	if err != nil {
		return err
	}
	var last error
	for _, target := range targets {
		ips, err := resolver.LookupIP(ctx, target)
		if err != nil {
			last = err
			continue
		}
		for _, ip := range ips {
			if requirePublic && !netutil.PublicIP(ip) {
				last = fmt.Errorf("mx destination is not public-routable")
				continue
			}
			if err := smtpTransaction(ctx, c.HELO, target, ip, from.Address, recipient, m.RawMIME); err == nil {
				return nil
			} else {
				last = err
				if transport.IsPermanent(err) {
					return err
				}
			}
		}
	}
	if last == nil {
		last = fmt.Errorf("mx destination has no usable addresses")
	}
	return last
}

func validate(c OutboundConfig, m transport.OutboundMessage) error {
	if strings.TrimSpace(c.HELO) == "" || strings.ContainsAny(c.HELO, "\r\n \t") {
		return fmt.Errorf("mx HELO hostname is required and must not contain whitespace")
	}
	if len(envelopeRecipients(m)) != 1 {
		return &transport.PermanentError{Err: fmt.Errorf("direct MX requires exactly one envelope recipient")}
	}
	if len(m.RawMIME) == 0 {
		return fmt.Errorf("mx raw MIME is required")
	}
	return nil
}

func envelopeRecipients(m transport.OutboundMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, group := range [][]string{m.To, m.CC, m.BCC} {
		for _, value := range group {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "" && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	return out
}

func mxTargets(ctx context.Context, resolver MXResolver, domain string) ([]string, error) {
	mxs, err := resolver.LookupMX(ctx, domain)
	if err == nil && len(mxs) > 0 {
		sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
		out := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			host := strings.TrimSuffix(strings.TrimSpace(mx.Host), ".")
			if host == "" {
				return nil, &transport.PermanentError{Err: fmt.Errorf("recipient domain has a null MX record")}
			}
			out = append(out, host)
		}
		return out, nil
	}
	if err != nil && !isNoSuchDomain(err) {
		return nil, err
	}
	return []string{domain}, nil
}

// ResolveTargets resolves the SMTP hosts for a recipient domain. It is exposed
// for diagnostics and deterministic transport tests; delivery still performs a
// fresh address lookup before every connection attempt.
func ResolveTargets(ctx context.Context, resolver MXResolver, domain string) ([]string, error) {
	return mxTargets(ctx, resolver, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), ".")))
}

func isNoSuchDomain(err error) bool {
	if dns, ok := err.(*net.DNSError); ok {
		return dns.IsNotFound
	}
	return false
}

func smtpTransaction(ctx context.Context, helo, host string, ip net.IP, from, recipient string, raw []byte) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "25"))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Hello(helo); err != nil {
		return err
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return classifySMTPError(err)
	}
	if err := client.Rcpt(recipient); err != nil {
		return classifySMTPError(err)
	}
	w, err := client.Data()
	if err != nil {
		return classifySMTPError(err)
	}
	bw := bufio.NewWriter(w)
	if _, err = bw.Write(raw); err == nil {
		err = bw.Flush()
	}
	closeErr := w.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return classifySMTPError(closeErr)
	}
	return client.Quit()
}

func classifySMTPError(err error) error {
	if e, ok := err.(*textproto.Error); ok && e.Code >= 500 && e.Code < 600 {
		return &transport.PermanentError{Err: err}
	}
	return err
}
