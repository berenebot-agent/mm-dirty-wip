package imap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
)

func TestDialPlainRequiresOptIn(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	host, port := fs.HostPort()

	_, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host:     host,
		Port:     port,
		Username: "user@example.com",
		Password: "secret",
		Security: imapadapter.SecurityPlain,
		// AllowPlain deliberately false.
	})
	if err == nil {
		t.Fatal("expected plaintext without opt-in to be rejected")
	}
}

func TestDialDefaultSecurityIsTLS(t *testing.T) {
	cfg := imapadapter.Config{Host: "imap.example.com", Username: "u", Password: "p"}
	n := cfg.Normalize()
	if n.Security != imapadapter.SecurityTLS {
		t.Fatalf("default security = %q, want tls", n.Security)
	}
	if n.Port != imapadapter.DefaultPortTLS {
		t.Fatalf("default port = %d, want %d", n.Port, imapadapter.DefaultPortTLS)
	}
}

func TestDialBadCredentials(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	host, port := fs.HostPort()
	_, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host: host, Port: port, Username: "user@example.com", Password: "wrong",
		Security: imapadapter.SecurityPlain, AllowPlain: true,
	})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindAuth {
		t.Fatalf("error = %v, want kind auth", err)
	}
}

func TestDialTLSVerifiesCertificate(t *testing.T) {
	fs := newFakeTLSServer(t, serverConfig{})
	host, port := fs.HostPort()

	// Without the self-signed CA the handshake must fail.
	_, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host: host, Port: port, Username: "user@example.com", Password: "secret",
		Security: imapadapter.SecurityTLS, ServerName: "127.0.0.1",
	})
	if err == nil {
		t.Fatal("expected TLS verification failure against an untrusted certificate")
	}
}

func TestDialTLSSucceedsWithTrustedCA(t *testing.T) {
	fs := newFakeTLSServer(t, serverConfig{})
	adapter := dialFakeTLS(t, fs, "user@example.com", "secret")
	if adapter == nil {
		t.Fatal("expected TLS dial to succeed")
	}
}

func TestDialNeverLogsPassword(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	host, port := fs.HostPort()
	const secret = "super-secret-password"
	_, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host: host, Port: port, Username: "user@example.com", Password: secret + "-bad",
		Security: imapadapter.SecurityPlain, AllowPlain: true,
	})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the password: %v", err)
	}
}

func TestCapabilitiesFromFakeServer(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	adapter := dialFake(t, fs, "user@example.com", "secret")

	caps := adapter.Capabilities()
	if !caps.Namespace {
		t.Error("expected NAMESPACE capability")
	}
	if !caps.Move {
		t.Error("expected MOVE capability")
	}
	if !caps.UIDPlus {
		t.Error("expected UIDPLUS capability")
	}
	if !caps.Idle {
		t.Error("expected IDLE capability")
	}
	if !adapter.IdleCapable() {
		t.Error("IdleCapable should be true")
	}
	if !caps.SpecialUse {
		t.Error("expected SPECIAL-USE capability")
	}
}

func TestDialContextCancellation(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	host, port := fs.HostPort()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := imapadapter.Dial(ctx, imapadapter.Config{
		Host: host, Port: port, Username: "user@example.com", Password: "secret",
		Security: imapadapter.SecurityPlain, AllowPlain: true,
	})
	if err == nil {
		t.Fatal("expected cancelled dial to fail")
	}
}

func TestConfigValidateRejectsEmptyCredentials(t *testing.T) {
	cases := []imapadapter.Config{
		{Host: "", Username: "u", Password: "p"},
		{Host: "h", Username: "", Password: "p"},
		{Host: "h", Username: "u", Password: ""},
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

var _ = time.Second
