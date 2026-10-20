package runtime_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	receiverruntime "github.com/dellarb/mailmoose/dialmx/runtime"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/emersion/go-smtp"
)

// localhostCert generates an ephemeral self-signed certificate valid for
// "localhost" and 127.0.0.1, returning the PEM pair and the parsed certificate
// for client-side trust.
func localhostCert(t *testing.T) (certPEM, keyPEM string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM, cert
}

// TestIncludedStartTLSInMemoryCertificate proves the included receiver accepts
// a STARTTLS certificate delivered as PEM over the control channel (not from
// disk) and completes a verified TLS handshake against it.
func TestIncludedStartTLSInMemoryCertificate(t *testing.T) {
	skipIfFixedPortsBusy(t)
	certPEM, keyPEM, cert := localhostCert(t)

	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	st, err := rt.Configure(context.Background(), control.Settings{
		Hostname:          "localhost",
		Secret:            "core-key",
		TLSCertificatePEM: certPEM,
		TLSPrivateKeyPEM:  keyPEM,
		SMTP: mxagent.Config{
			Hostname:        "localhost",
			MaxMessageBytes: 1 << 20,
			MaxRecipients:   10,
			MaxConnections:  8,
			DataTimeout:     2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	// Configure forces the fixed endpoints; reach the ephemeral test SMTP addr
	// through a direct Activate is avoided, so use the status addr (tests that
	// need fixed ports are separate). st.SMTPAddr is the bound :2525 endpoint.
	if st.State != control.StateActive {
		t.Fatalf("state %q", st.State)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	c, err := smtp.DialStartTLS(st.SMTPAddr, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("starttls command: %v", err)
	}
	defer c.Close()
	// The handshake is lazy: a command after STARTTLS drives it, so a trust
	// failure surfaces here rather than on DialStartTLS.
	if err := c.Hello("localhost"); err != nil {
		t.Fatalf("post-STARTTLS handshake (verify against the in-memory cert): %v", err)
	}
	state, ok := c.TLSConnectionState()
	if !ok {
		t.Fatal("connection is not TLS after STARTTLS")
	}
	if len(state.PeerCertificates) == 0 {
		t.Fatal("no peer certificate presented")
	}
	if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: pool, DNSName: "localhost",
	}); err != nil {
		t.Fatalf("verify presented certificate: %v", err)
	}
}

// skipIfFixedPortsBusy skips the test when the included receiver's fixed
// endpoints are occupied, so Configure-based tests never flake.
func skipIfFixedPortsBusy(t *testing.T) {
	t.Helper()
	for _, addr := range []string{receiverruntime.DefaultSMTPAddr, receiverruntime.DefaultSessionAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("fixed endpoint %s unavailable: %v", addr, err)
		}
		_ = ln.Close()
	}
}

// TestIncludedStartTLSRejectsPlaintextWhenRequired proves RequireTLS refuses a
// plaintext MAIL when an in-memory certificate is configured.
func TestIncludedStartTLSRejectsPlaintextWhenRequired(t *testing.T) {
	skipIfFixedPortsBusy(t)
	certPEM, keyPEM, _ := localhostCert(t)

	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	st, err := rt.Configure(context.Background(), control.Settings{
		Hostname:          "localhost",
		Secret:            "core-key",
		TLSCertificatePEM: certPEM,
		TLSPrivateKeyPEM:  keyPEM,
		SMTP: mxagent.Config{
			Hostname:        "localhost",
			RequireTLS:      true,
			MaxMessageBytes: 1 << 20,
			MaxRecipients:   10,
			MaxConnections:  8,
			DataTimeout:     2 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	c, err := smtp.Dial(st.SMTPAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Hello("plain.test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := c.Mail("sender@outside.test", nil); err == nil {
		t.Fatal("plaintext MAIL accepted with RequireTLS")
	}
}

// TestConfigureInvalidPEMKeepsActive proves a repeated Configure with a broken
// certificate is rejected before deactivation: the running receiver stays
// active and serving.
func TestConfigureInvalidPEMKeepsActive(t *testing.T) {
	skipIfFixedPortsBusy(t)
	certPEM, keyPEM, _ := localhostCert(t)

	rt := receiverruntime.New(slog.New(slog.NewTextHandler(discard{}, nil)))
	defer rt.Close()

	good := control.Settings{
		Hostname:          "localhost",
		Secret:            "core-key",
		TLSCertificatePEM: certPEM,
		TLSPrivateKeyPEM:  keyPEM,
		SMTP:              mxagent.Config{Hostname: "localhost", MaxMessageBytes: 1 << 20},
	}
	if _, err := rt.Configure(context.Background(), good); err != nil {
		t.Fatalf("initial configure: %v", err)
	}
	bad := good
	bad.TLSCertificatePEM = "-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"
	if _, err := rt.Configure(context.Background(), bad); err == nil {
		t.Fatal("expected invalid PEM to be rejected")
	}
	if got := rt.Status().State; got != control.StateActive {
		t.Fatalf("active receiver was taken down by an invalid configure: %q", got)
	}
}
