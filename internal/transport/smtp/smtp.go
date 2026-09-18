package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	smtpstd "net/smtp"
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
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if sec == "tls" {
			conn = tls.Client(conn, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		}
		cl, err := smtpstd.NewClient(conn, c.Host)
		if err != nil {
			conn.Close()
			last = err
			continue
		}
		ok := false
		func() {
			defer cl.Close()
			if sec == "starttls" {
				if okExt, _ := cl.Extension("STARTTLS"); !okExt {
					last = fmt.Errorf("smtp server does not offer STARTTLS")
					return
				}
				if err := cl.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
					last = err
					return
				}
			}
			if c.Username != "" {
				auth := smtpstd.PlainAuth("", c.Username, c.Password, c.Host)
				if err := cl.Auth(auth); err != nil {
					last = err
					return
				}
			}
			if err := cl.Mail(m.From); err != nil {
				last = err
				return
			}
			for _, rcpt := range m.To {
				if err := cl.Rcpt(rcpt); err != nil {
					last = err
					return
				}
			}
			wc, err := cl.Data()
			if err != nil {
				last = err
				return
			}
			bw := bufio.NewWriter(wc)
			if _, err = bw.Write(m.Raw); err == nil {
				err = bw.Flush()
			}
			cerr := wc.Close()
			if err != nil {
				last = err
				return
			}
			if cerr != nil {
				last = cerr
				return
			}
			_ = cl.Quit()
			ok = true
		}()
		if ok {
			return nil
		}
	}
	if last == nil {
		last = fmt.Errorf("no usable smtp destination")
	}
	return last
}
