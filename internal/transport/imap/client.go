package imap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/netutil"
	"github.com/emersion/go-imap/v2/imapclient"
)

// defaultDialTimeout bounds a single connection attempt, including the TLS
// handshake for implicit TLS.
const defaultDialTimeout = 20 * time.Second

// connect establishes and authenticates an IMAP session. It applies the
// destination policy, certificate verification, the security-mode gate and a
// bounded dial context. On any failure the underlying connection is closed and
// no credential material is included in the error.
func connect(ctx context.Context, cfg Config, sink *notifySink) (*imapclient.Client, error) {
	n := cfg.Normalize()
	if err := n.Validate(); err != nil {
		return nil, err
	}

	dialTimeout := n.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = defaultDialTimeout
	}

	tlsConfig, err := buildTLSConfig(n)
	if err != nil {
		return nil, err
	}

	address := n.Address()
	options := &imapclient.Options{TLSConfig: tlsConfig}
	if sink != nil {
		options.UnilateralDataHandler = &imapclient.UnilateralDataHandler{
			Mailbox: sink.mailbox,
		}
	}

	var client *imapclient.Client
	switch n.Security {
	case SecurityTLS:
		client, err = dialTLSContext(ctx, tlsConfig, address, n, dialTimeout, options)
	case SecurityStartTLS:
		client, err = dialStartTLSContext(ctx, tlsConfig, address, n, dialTimeout, options)
	case SecurityPlain:
		var conn net.Conn
		conn, err = dialPolicy(ctx, address, n.RequirePublic, dialTimeout)
		if err == nil {
			client = imapclient.New(conn, options)
		}
	default:
		return nil, Unsupported("unsupported security mode")
	}
	if err != nil {
		return nil, wrapErr(err)
	}

	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	if err := authenticate(client, n.Username, n.Password); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// dialTLSContext dials addr and performs an implicit-TLS handshake within ctx,
// verifying the server certificate against the system roots (plus any
// operator-supplied roots).
func dialTLSContext(ctx context.Context, tlsConfig *tls.Config, addr string, n Config, timeout time.Duration, options *imapclient.Options) (*imapclient.Client, error) {
	raw, err := dialPolicy(ctx, addr, n.RequirePublic, timeout)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig.Clone()
	if cfg.NextProtos == nil {
		cfg.NextProtos = []string{"imap"}
	}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	opts := *options
	opts.TLSConfig = cfg
	return imapclient.New(conn, &opts), nil
}

// dialStartTLSContext dials cleartext, issues STARTTLS and upgrades, verifying
// the certificate. It never proceeds in cleartext: if the server does not
// advertise STARTTLS or the handshake fails, the connection is dropped.
func dialStartTLSContext(ctx context.Context, tlsConfig *tls.Config, addr string, n Config, timeout time.Duration, options *imapclient.Options) (*imapclient.Client, error) {
	raw, err := dialPolicy(ctx, addr, n.RequirePublic, timeout)
	if err != nil {
		return nil, err
	}
	cfg := tlsConfig.Clone()
	if cfg.ServerName == "" {
		if n.ServerName != "" {
			cfg.ServerName = n.ServerName
		} else {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr == nil {
				cfg.ServerName = host
			}
		}
	}
	opts := *options
	opts.TLSConfig = cfg
	client, err := imapclient.NewStartTLS(raw, &opts)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return client, nil
}

// dialPolicy opens a TCP connection under ctx, enforcing the public-IP policy
// at connect time when enabled. When the policy is disabled it delegates to a
// plain dialer. It is a package variable so a future test seam can substitute a
// deterministic dialer; production always uses the default implementation.
var dialPolicy = func(ctx context.Context, addr string, publicOnly bool, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if !publicOnly {
		return dialer.DialContext(ctx, "tcp", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		if !netutil.PublicIP(ip) {
			last = fmt.Errorf("imap: destination is not public-routable")
			continue
		}
		conn, derr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if derr != nil {
			last = derr
			continue
		}
		return conn, nil
	}
	if last == nil {
		last = fmt.Errorf("imap: destination is not public-routable")
	}
	return nil, last
}

// buildTLSConfig builds a verifying TLS configuration. Certificate
// verification is always on; an operator may add private roots, but may never
// disable verification.
func buildTLSConfig(n Config) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: n.ServerName,
	}
	if cfg.ServerName == "" {
		cfg.ServerName = n.Host
	}
	if len(n.TLSCertPEM) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(n.TLSCertPEM) {
			return nil, fmt.Errorf("imap: no certificates found in the supplied PEM bundle")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// authenticate logs in and validates the outcome. The credentials are only ever
// passed to the library's LOGIN command; they are not logged here.
func authenticate(client *imapclient.Client, username, password string) error {
	if err := client.WaitGreeting(); err != nil {
		return wrapErr(err)
	}
	if err := client.Login(username, password).Wait(); err != nil {
		return wrapErr(err)
	}
	return nil
}
