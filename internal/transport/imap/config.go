// Package imap implements the optional remote IMAP protocol adapter for
// standalone mailboxes. It speaks IMAP4rev2/IMAP4rev1 through
// github.com/emersion/go-imap/v2 and exposes a small, provider-neutral surface
// the mailbox service consumes: root discovery, folder listing with special
// roles, header/body/attachment fetch with streaming, search, folder
// management, flags, moves, UID-targeted expunge/append and IDLE capability.
//
// The adapter is deliberately independent of the store and the app layer: it
// operates on plain values (an injected net.Conn, a Config, an explicit root)
// and never imports internal/store or internal/app. Credentials are held only
// in Config, are never logged, and are never placed in an error string.
package imap

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// SecurityMode names the transport security of an IMAP connection. It mirrors
// model.RemoteSecurity* so the adapter and the model cannot drift.
type SecurityMode string

const (
	// SecurityTLS is implicit TLS (the default). It never downgrades.
	SecurityTLS SecurityMode = SecurityMode(model.RemoteSecurityTLS)
	// SecurityStartTLS upgrades a cleartext connection with STARTTLS and
	// refuses to continue if the server advertises LOGINDISABLED or the
	// upgrade fails. It never falls back to cleartext.
	SecurityStartTLS SecurityMode = SecurityMode(model.RemoteSecurityStartTLS)
	// SecurityPlain is cleartext with no transport security. It is an explicit
	// operator choice and is only permitted when AllowPlain is set on the
	// Config; a deployment policy layer decides whether to set it at all.
	SecurityPlain SecurityMode = SecurityMode(model.RemoteSecurityPlain)
)

// Default ports for the security modes.
const (
	DefaultPortTLS      = model.RemoteDefaultIMAPPort
	DefaultPortStartTLS = model.RemoteDefaultIMAPStartPort
	DefaultPortPlain    = model.RemoteDefaultIMAPPlainPort
)

// Config is the non-secret connection description plus the two secrets needed
// to authenticate. The secrets (Password) never leave the process, are never
// logged, and are never embedded in an error.
//
// Host/Port/Username/Security mirror model.RemoteConnection; RequirePublic and
// DialTimeouts control destination policy and bounded I/O.
type Config struct {
	// Host is the IMAP server hostname.
	Host string
	// Port is the TCP port. Zero selects the conventional port for the
	// security mode.
	Port int
	// Username is the login identity. It is never logged.
	Username string
	// Password is the login secret. It is never logged and never appears in an
	// error string.
	Password string
	// Security is the transport security mode. The zero value means TLS; TLS
	// is the default and a connection never downgrades to a weaker mode.
	Security SecurityMode
	// AllowPlain must be true for SecurityPlain to be accepted. It is an
	// explicit, per-connection opt-in so a caller can never select plaintext
	// by accident.
	AllowPlain bool
	// RequirePublic, when true, restricts the connection to public-routable
	// destinations and pins the resolved address at dial time so DNS cannot
	// rebind between validation and connection.
	RequirePublic bool
	// DialTimeout bounds a single dial (TCP handshake plus, for implicit TLS,
	// the TLS handshake). Zero uses the package default.
	DialTimeout time.Duration
	// ServerName overrides the TLS server name used for certificate
	// verification. Empty means Host is used, which is the correct behaviour
	// for a normal provider.
	ServerName string
	// TLSCertPEM, when non-empty, is a PEM bundle of additional trusted roots
	// (for a self-hosted server with a private CA). The system roots are always
	// used in addition.
	TLSCertPEM []byte
}

// Normalize fills in the port default and normalizes the security mode. It does
// not mutate the receiver.
func (c Config) Normalize() Config {
	out := c
	out.Host = strings.TrimSpace(out.Host)
	out.Username = strings.TrimSpace(out.Username)
	if out.Security == "" {
		out.Security = SecurityTLS
	}
	out.Security = SecurityMode(strings.ToLower(strings.TrimSpace(string(out.Security))))
	if out.Port == 0 {
		out.Port = defaultPort(out.Security)
	}
	return out
}

// Validate checks a normalized config for internal consistency. It never
// includes the password in the returned error.
func (c Config) Validate() error {
	n := c.Normalize()
	if n.Host == "" {
		return fmt.Errorf("imap: host is required")
	}
	if strings.ContainsAny(n.Host, " /\\\t\r\n") {
		return fmt.Errorf("imap: invalid host")
	}
	if n.Username == "" {
		return fmt.Errorf("imap: username is required")
	}
	if n.Password == "" {
		return fmt.Errorf("imap: password is required")
	}
	if n.Port < 1 || n.Port > 65535 {
		return fmt.Errorf("imap: invalid port")
	}
	switch n.Security {
	case SecurityTLS, SecurityStartTLS:
	case SecurityPlain:
		if !n.AllowPlain {
			return fmt.Errorf("imap: plaintext is not permitted without an explicit opt-in")
		}
	default:
		return fmt.Errorf("imap: security must be %q, %q or %q", SecurityTLS, SecurityStartTLS, SecurityPlain)
	}
	return nil
}

// Address returns the host:port dial target.
func (c Config) Address() string {
	n := c.Normalize()
	return net.JoinHostPort(n.Host, fmt.Sprintf("%d", n.Port))
}

// SecurityFromModel maps a model security string to the adapter mode, applying
// the model default (TLS) for an empty value.
func SecurityFromModel(raw string) SecurityMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case model.RemoteSecurityStartTLS:
		return SecurityStartTLS
	case model.RemoteSecurityPlain:
		return SecurityPlain
	default:
		return SecurityTLS
	}
}

func defaultPort(mode SecurityMode) int {
	switch mode {
	case SecurityStartTLS, SecurityPlain:
		return DefaultPortStartTLS
	default:
		return DefaultPortTLS
	}
}
