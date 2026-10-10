package imap_test

import (
	"context"
	"testing"
	"time"

	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
)

// dialFake connects an adapter to the fake server over plaintext with
// RequirePublic disabled (the fake server is on loopback).
func dialFake(t *testing.T, fs *fakeServer, username, password string) *imapadapter.Adapter {
	t.Helper()
	host, port := fs.HostPort()
	adapter, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host:       host,
		Port:       port,
		Username:   username,
		Password:   password,
		Security:   imapadapter.SecurityPlain,
		AllowPlain: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter
}

// dialFakeTLS connects over implicit TLS, trusting the fake server's
// self-signed certificate through TLSCertPEM.
func dialFakeTLS(t *testing.T, fs *fakeServer, username, password string) *imapadapter.Adapter {
	t.Helper()
	host, port := fs.HostPort()
	adapter, err := imapadapter.Dial(context.Background(), imapadapter.Config{
		Host:       host,
		Port:       port,
		Username:   username,
		Password:   password,
		Security:   imapadapter.SecurityTLS,
		ServerName: "127.0.0.1",
		TLSCertPEM: selfSignedPEM(t),
	})
	if err != nil {
		t.Fatalf("Dial TLS: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}
