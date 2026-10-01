package receiver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// buildReceiverBinary compiles the real dialmx/cmd/receiver binary once. It is
// skipped when Go is not on PATH (e.g. a host-invoked run outside the Docker
// wrapper), because the binary smoke test is only meaningful with a toolchain.
func buildReceiverBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; binary smoke runs under the Docker wrapper only")
	}
	bin := filepath.Join(t.TempDir(), "dialmx-receiver")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", bin, "./dialmx/cmd/receiver")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build receiver binary: %v\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

// selfSignedCA writes a certificate and key valid for 127.0.0.1 and returns
// their paths plus a pool trusting the certificate.
func selfSignedCA(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dialmx-smoke"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(parsed)
	return certFile, keyFile, pool
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestBinaryHTTP2AndSMTP is the process-level smoke test: it runs the real
// receiver binary with a self-signed certificate on two free ports, confirms
// the session listener negotiates HTTP/2 (not HTTP/1.1) for /readyz, confirms
// the SMTP edge greets and answers a well-formed RCPT with the documented 451
// temporary failure for an unregistered domain, then SIGTERMs the process and
// requires it to exit within the bounded shutdown window.
func TestBinaryHTTP2AndSMTP(t *testing.T) {
	bin := buildReceiverBinary(t)
	certFile, keyFile, pool := selfSignedCA(t)
	sessionAddr := freePort(t)
	smtpAddr := freePort(t)

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"DIALMX_MODE=shared",
		"DIALMX_LISTEN_ADDR="+sessionAddr,
		"DIALMX_TLS_CERT="+certFile,
		"DIALMX_TLS_KEY="+keyFile,
		"MX_LISTEN_ADDR="+smtpAddr,
		"MX_HOSTNAME=mx.smoke.test",
		"MX_MAX_MESSAGE_BYTES=1048576",
		"MX_STAGING_BYTES=2097152",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
		}
	})

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
	readyURL := "https://" + sessionAddr + "/readyz"
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(readyURL)
		if err == nil {
			if resp.ProtoMajor != 2 {
				_ = resp.Body.Close()
				t.Fatalf("readyz negotiated HTTP/%d, want HTTP/2", resp.ProtoMajor)
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("readyz status %d, want 200", resp.StatusCode)
			}
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver never became ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	smtpDeadline := time.Now().Add(5 * time.Second)
	var conn *smtp.Client
	for {
		var err error
		conn, err = smtp.Dial(smtpAddr)
		if err == nil {
			break
		}
		if time.Now().After(smtpDeadline) {
			t.Fatalf("SMTP edge never accepted a connection: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer conn.Close()
	if err := conn.Hello("smoke.test"); err != nil {
		t.Fatalf("SMTP HELO failed: %v", err)
	}
	if err := conn.Mail("sender@outside.test", nil); err != nil {
		t.Fatalf("SMTP MAIL failed: %v", err)
	}
	err := conn.Rcpt("ghost@unregistered.test", nil)
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 {
		t.Fatalf("expected 451 for unregistered domain, got %v", err)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal receiver: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		waited = true
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("receiver did not exit within bounded shutdown window")
	}
}
