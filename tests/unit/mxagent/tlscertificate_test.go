package mxagent_test

import (
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

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/emersion/go-smtp"
)

// inMemoryCert builds a self-signed localhost certificate in memory and returns
// the tls.Certificate and the parsed leaf for client trust.
func inMemoryCert(t *testing.T) (cert tls.Certificate, leaf *x509.Certificate) {
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
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, leaf
}

// TestEdgeUsesInMemoryCertificate proves ListenAndServe prefers the in-memory
// TLSCertificate over file paths: with a deliberately invalid file pair set,
// STARTTLS still succeeds and presents the in-memory certificate.
func TestEdgeUsesInMemoryCertificate(t *testing.T) {
	cert, leaf := inMemoryCert(t)

	cfg := mxagent.Config{
		Hostname:        "localhost",
		MaxMessageBytes: 1 << 20,
		MaxStagingBytes: 2 << 20,
		MaxRecipients:   10,
		MaxConnections:  8,
		DataTimeout:     2 * time.Second,
		DNSTimeout:      2 * time.Second,
		// A bogus file pair must be ignored when the in-memory cert is set.
		TLSCertFile:    "/nonexistent/cert.pem",
		TLSKeyFile:     "/nonexistent/key.pem",
		TLSCertificate: &cert,
	}
	addr, stop := startEdgeWithConfig(t, slog.Default(), &ctxDelivery{}, cfg)
	defer stop()

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	c, err := smtp.DialStartTLS(addr, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("starttls: %v", err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		t.Fatalf("handshake with in-memory cert: %v", err)
	}
	state, ok := c.TLSConnectionState()
	if !ok || len(state.PeerCertificates) == 0 {
		t.Fatal("no TLS state/peer certificate after STARTTLS")
	}
	if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatalf("verify in-memory certificate: %v", err)
	}
}
