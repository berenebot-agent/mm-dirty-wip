package imap_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// testCert is a self-signed certificate for 127.0.0.1 plus its PEM encodings.
type testCert struct {
	tls     tls.Certificate
	certPEM []byte
	keyPEM  []byte
}

// generateSelfSigned builds a self-signed ECDSA certificate valid for 127.0.0.1
// and ::1. It is generated once per call; tests that need a stable pair should
// reuse the result.
func generateSelfSigned(t *testing.T) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return testCert{tls: pair, certPEM: certPEM, keyPEM: keyPEM}
}

var cachedCert testCert

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	if cachedCert.tls.Certificate == nil {
		cachedCert = generateSelfSigned(t)
	}
	return cachedCert.tls
}

// selfSignedPEM returns the PEM certificate for trusting the fake TLS server.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	if cachedCert.certPEM == nil {
		cachedCert = generateSelfSigned(t)
	}
	return cachedCert.certPEM
}
