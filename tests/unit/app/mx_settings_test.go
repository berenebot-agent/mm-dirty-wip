package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
)

var sysadmin = model.Principal{SystemAdmin: true}

// testCertPEM returns a syntactically valid self-signed certificate PEM so CA
// validation can succeed without shipping a fixture.
func testCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

// TestMXReceiverSettingsSaveAndRedact covers the system-administrator save
// contract: permission, validation, CAS, key generation/retention and
// redaction.
func TestMXReceiverSettingsSaveAndRedact(t *testing.T) {
	svc, _, _, _ := testService(t)
	ctx := context.Background()
	caPEM := testCertPEM(t)

	// Non system administrators cannot read or write installation settings.
	if _, err := svc.SaveMXReceiverSettings(ctx, model.Principal{Admin: true}, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k"}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("non-admin save = %v", err)
	}
	if err := svc.ClearMXReceiverSettings(ctx, model.Principal{}, 0); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("non-admin clear = %v", err)
	}

	// Unconfigured reads are empty and unlocked.
	empty, err := svc.GetMXReceiverSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Mode != "" || empty.Revision != 0 || empty.KeyConfigured {
		t.Fatalf("empty settings = %+v", empty)
	}

	// Invalid inputs are rejected with the sentinel error.
	for _, in := range []app.MXReceiverInput{
		{Mode: "bogus"},
		{Mode: app.MXModeRemote}, // missing URL
		{Mode: app.MXModeRemote, URL: "ftp://r.example", BearerKey: "k"}, // bad scheme
		{Mode: app.MXModeRemote, URL: "https://r.example/path", BearerKey: "k"},
		{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", Hostname: "mx.test"},        // included-only field
		{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", MaxRecipients: 5},           // included-only field
		{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", VerifyDKIM: boolPtr(false)}, // included-only field
		{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", CA: "not a certificate"},    // bad CA
		{Mode: app.MXModeIncluded, URL: "https://r.example"},                                           // included takes no URL
		{Mode: app.MXModeIncluded, MaxMessageBytes: 1024},                                              // below the 1 MiB floor
		{Mode: app.MXModeIncluded, MaxRecipients: -1},                                                  // negative limit
		{Mode: app.MXModeIncluded, Hostname: "http://mx.test"},                                         // not a bare hostname
		{Mode: app.MXModeIncluded, Hostname: "bad_host.example"},                                       // invalid character
	} {
		if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, in); !errors.Is(err, app.ErrMXInvalidInput) {
			t.Fatalf("input %+v err = %v", in, err)
		}
	}

	// Included mode generates a key on create and retains it on update, even if
	// a caller tries to supply one. Verification defaults on.
	included, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeIncluded, BearerKey: "ignored", Hostname: "mx.test", MaxRecipients: 50})
	if err != nil {
		t.Fatal(err)
	}
	if included.Mode != app.MXModeIncluded || included.Revision != 1 || !included.KeyConfigured {
		t.Fatalf("included save = %+v", included)
	}
	if !included.VerifySPFEnabled() || !included.VerifyDKIMEnabled() || !included.VerifyDMARCEnabled() {
		t.Fatalf("verification must default on: %+v", included)
	}
	if included.BearerKey != "" {
		t.Fatal("read must redact the bearer key")
	}
	firstKey, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil || firstKey.BearerKey == "" {
		t.Fatalf("runtime settings key missing: %v", err)
	}
	included2, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeIncluded, Hostname: "mx2.test", VerifyDKIM: boolPtr(false), Revision: included.Revision})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retained.BearerKey != firstKey.BearerKey {
		t.Fatal("included-to-included save rotated the generated key")
	}
	if retained.Hostname != "mx2.test" || retained.VerifyDKIMEnabled() || !retained.VerifySPFEnabled() || included2.Revision != 2 {
		t.Fatalf("included update = %+v", retained)
	}

	// A stale revision conflicts.
	if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeIncluded, Revision: 1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale save = %v", err)
	}

	// Switching included -> remote without a key is rejected: the generated
	// included key is not a valid remote credential.
	if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example", Revision: included2.Revision}); !errors.Is(err, app.ErrMXInvalidInput) {
		t.Fatalf("included->remote blank key = %v", err)
	}

	// Switch to remote with a supplied key: it replaces the credential and the
	// included-only blob is dropped.
	remote, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example/", BearerKey: "remote-secret", CA: caPEM, Revision: included2.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if remote.URL != "https://r.example" || remote.CA != caPEM || !remote.KeyConfigured {
		t.Fatalf("remote save = %+v", remote)
	}
	if remote.Hostname != "" || remote.MaxRecipients != 0 {
		t.Fatalf("remote save kept included-only fields: %+v", remote)
	}
	rt, _ := svc.MXReceiverSettingsForRuntime(ctx)
	if rt.BearerKey != "remote-secret" {
		t.Fatalf("remote key = %q", rt.BearerKey)
	}

	// A blank remote key retains the stored credential.
	remote2, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r2.example", Revision: remote.Revision})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if rt.BearerKey != "remote-secret" {
		t.Fatalf("blank key did not retain: %q", rt.BearerKey)
	}

	// Switching remote -> included generates a fresh included key; it never
	// reuses the operator's remote key.
	included3, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeIncluded, Revision: remote2.Revision})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if rt.BearerKey == "remote-secret" || rt.BearerKey == "" {
		t.Fatalf("remote->included reused the remote key: %q", rt.BearerKey)
	}
	if !rt.VerifySPFEnabled() || !rt.VerifyDKIMEnabled() || !rt.VerifyDMARCEnabled() {
		t.Fatalf("verification default lost: %+v", rt)
	}

	// The runtime status reflects the persisted configuration.
	status, err := svc.MXReceiverStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Mode != app.MXModeIncluded || !status.Configured || status.Revision != included3.Revision {
		t.Fatalf("status = %+v", status)
	}

	// Clearing leaves an initialized, unconfigured state.
	if err := svc.ClearMXReceiverSettings(ctx, sysadmin, included3.Revision); err != nil {
		t.Fatal(err)
	}
	cleared, err := svc.GetMXReceiverSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Mode != "" || cleared.KeyConfigured {
		t.Fatalf("cleared = %+v", cleared)
	}
	status, _ = svc.MXReceiverStatus(ctx)
	if status.Configured || status.State != "disabled" {
		t.Fatalf("cleared status = %+v", status)
	}
}

// tlsTestKeyPair returns a valid self-signed certificate and its private key in
// PEM form, suitable for STARTTLS validation.
func tlsTestKeyPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyPEM := strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	return certPEM, keyPEM
}

// TestMXReceiverIncludedAdvancedSettings covers the full included SMTP surface:
// every field round-trips, the private TLS key is never returned publicly but is
// available to the runtime, a blank key retains the stored one, and malformed or
// partial TLS pairs are rejected.
func TestMXReceiverIncludedAdvancedSettings(t *testing.T) {
	svc, _, _, _ := testService(t)
	ctx := context.Background()
	certPEM, keyPEM := tlsTestKeyPair(t)
	requireTLS := true

	saved, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{
		Mode:                app.MXModeIncluded,
		Hostname:            "mx.test",
		MaxMessageBytes:     2 << 20,
		MaxStagingBytes:     4 << 20,
		MaxRecipients:       50,
		MaxConnections:      80,
		RequireTLS:          &requireTLS,
		VerifyDKIM:          boolPtr(false),
		DNSResolver:         "127.0.0.1:53",
		DNSTimeoutSeconds:   7,
		ReadTimeoutSeconds:  60,
		WriteTimeoutSeconds: 70,
		DataTimeoutSeconds:  300,
		SMTPTLSCert:         certPEM,
		SMTPTLSKey:          keyPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.SMTPTLSCert != certPEM || !saved.SMTPTLSKeyConfigured {
		t.Fatalf("saved TLS state = %+v", saved)
	}
	if saved.SMTPTLSKey != "" {
		t.Fatal("public read must not echo the TLS private key")
	}
	if !saved.RequireTLSEnabled() || saved.VerifyDKIMEnabled() || !saved.VerifySPFEnabled() {
		t.Fatalf("toggle resolution = %+v", saved)
	}

	// The runtime accessor sees the private key and all tunables.
	rt, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rt.SMTPTLSKey != keyPEM || rt.MaxStagingBytes != 4<<20 || rt.DNSResolver != "127.0.0.1:53" ||
		rt.DNSTimeoutSeconds != 7 || rt.ReadTimeoutSeconds != 60 || rt.WriteTimeoutSeconds != 70 || rt.DataTimeoutSeconds != 300 {
		t.Fatalf("runtime fields = %+v", rt)
	}

	// A blank key retains the stored one.
	retained, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{
		Mode: app.MXModeIncluded, SMTPTLSCert: certPEM, RequireTLS: &requireTLS, Revision: saved.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if rt.SMTPTLSKey != keyPEM {
		t.Fatal("blank TLS key did not retain the stored key")
	}
	// Clearing the certificate clears the retained key too.
	cleared, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeIncluded, Revision: retained.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.SMTPTLSCert != "" || cleared.SMTPTLSKeyConfigured {
		t.Fatalf("certificate not cleared: %+v", cleared)
	}

	// Malformed and partial pairs are rejected.
	for _, in := range []app.MXReceiverInput{
		{Mode: app.MXModeIncluded, SMTPTLSCert: certPEM},                            // missing key
		{Mode: app.MXModeIncluded, SMTPTLSKey: keyPEM},                              // missing cert
		{Mode: app.MXModeIncluded, SMTPTLSCert: "not pem", SMTPTLSKey: "not pem"},   // malformed
		{Mode: app.MXModeIncluded, RequireTLS: boolPtr(true)},                       // require without cert
		{Mode: app.MXModeIncluded, MaxStagingBytes: 1024, MaxMessageBytes: 2 << 20}, // staging < message
		{Mode: app.MXModeIncluded, DNSTimeoutSeconds: -1},                           // negative timeout
		// Above the core ingest cap (testService sets 5 MiB).
		{Mode: app.MXModeIncluded, MaxMessageBytes: 6 << 20},
		// Cert+key larger than the control frame can deliver.
		{Mode: app.MXModeIncluded, SMTPTLSCert: certPEM + strings.Repeat("A", 61<<10), SMTPTLSKey: keyPEM},
	} {
		if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, in); !errors.Is(err, app.ErrMXInvalidInput) {
			t.Fatalf("input %+v err = %v", in, err)
		}
	}

	// Every included-only field is rejected in remote mode.
	if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", DNSResolver: "8.8.8.8:53"}); !errors.Is(err, app.ErrMXInvalidInput) {
		t.Fatal("remote save accepted an included-only DNS resolver")
	}
	if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", SMTPTLSCert: certPEM, SMTPTLSKey: keyPEM}); !errors.Is(err, app.ErrMXInvalidInput) {
		t.Fatal("remote save accepted an included-only TLS pair")
	}
}

// TestMXReceiverIngestGate verifies the persisted configuration, not MX_ENABLE,
// decides whether the mx-routed ingest path accepts work.
func TestMXReceiverIngestGate(t *testing.T) {
	svc, _, _, box := mxService(t)
	ctx := context.Background()
	cleared, err := svc.GetMXReceiverSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearMXReceiverSettings(ctx, sysadmin, cleared.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := svc.GetMXReceiverSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})); !errors.Is(err, app.ErrMXDisabled) {
		t.Fatalf("unconfigured ingest = %v", err)
	}
	if _, err := svc.SaveMXReceiverSettings(ctx, sysadmin, app.MXReceiverInput{Mode: app.MXModeRemote, URL: "https://r.example", BearerKey: "k", Revision: after.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})); err != nil {
		t.Fatalf("configured ingest = %v", err)
	}
}

func boolPtr(v bool) *bool { return &v }

// stubRuntime reports a fixed status, standing in for cmd/server's controller.
type stubRuntime struct {
	status app.MXReceiverStatus
}

func (s stubRuntime) Wake()                                       {}
func (s stubRuntime) Status(context.Context) app.MXReceiverStatus { return s.status }

// TestMXReceiverStatusQueriesRuntimeWithoutSettings verifies a fresh install
// still reports the included shape as supported, so the UI does not hide it.
func TestMXReceiverStatusQueriesRuntimeWithoutSettings(t *testing.T) {
	svc, _, _, _ := testService(t)
	ctx := context.Background()
	svc.MXRuntime = stubRuntime{status: app.MXReceiverStatus{State: "disabled", IncludedSupported: true}}

	st, err := svc.MXReceiverStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.IncludedSupported {
		t.Fatalf("fresh install must report included support: %+v", st)
	}
	if st.Configured || st.State != "disabled" {
		t.Fatalf("fresh install status = %+v", st)
	}
}
