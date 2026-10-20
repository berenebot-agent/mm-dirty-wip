package imap_test

import (
	"context"
	"testing"

	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

func TestDialRejectsNonPublicDestination(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	host, port := fs.HostPort()

	netutil.SetRequirePublic(true)
	defer netutil.SetRequirePublic(false)

	_, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host: host, Port: port, Username: "user@example.com", Password: "secret",
		Security:      imapadapter.SecurityPlain,
		AllowPlain:    true,
		RequirePublic: true,
	})
	if err == nil {
		t.Fatal("expected a loopback destination to be rejected when RequirePublic is set")
	}
}

func TestDialAllowsLoopbackWhenPublicNotRequired(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	adapter := dialFake(t, fs, "user@example.com", "secret")
	if adapter == nil {
		t.Fatal("expected the loopback dial to succeed without the public requirement")
	}
}
