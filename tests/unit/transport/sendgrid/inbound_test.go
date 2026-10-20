package sendgrid_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/sendgrid"
)

var _ transport.InboundTransport = sendgrid.Transport{}

type fakeResolver struct {
	publicKey string
	err       error
}

func (f fakeResolver) ResolveInboundBinding(_ context.Context, provider, recipient string) (transport.InboundBinding, error) {
	if f.err != nil {
		return transport.InboundBinding{}, f.err
	}
	return transport.InboundBinding{
		AccountID: "acc", DomainID: "dom", CredentialID: "cred",
		Provider: provider, Recipient: recipient,
		Config: map[string]any{"public_key": f.publicKey},
	}, nil
}

// keypair returns a fresh ECDSA private key and its base64 SubjectPublicKeyInfo.
func keypair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(der)
}

// signedRequest builds a SendGrid multipart webhook with a valid signature over
// sha256(timestamp||body).
func signedRequest(t *testing.T, priv *ecdsa.PrivateKey, recipient, raw string, ts string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("envelope", fmt.Sprintf(`{"to":[%q],"from":"b@test"}`, recipient))
	p, _ := w.CreateFormField("email")
	p.Write([]byte(raw))
	w.Close()
	body := b.Bytes()
	r := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	r.Header.Set("Content-Type", w.FormDataContentType())
	if ts == "" {
		ts = fmt.Sprintf("%d", time.Now().Unix())
	}
	r.Header.Set("X-Twilio-Email-Event-Webhook-Timestamp", ts)
	h := sha256.New()
	h.Write([]byte(ts))
	h.Write(body)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, h.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Twilio-Email-Event-Webhook-Signature", base64.StdEncoding.EncodeToString(sig))
	return r
}

func TestReceiveValidSignature(t *testing.T) {
	priv, pub := keypair(t)
	var tr sendgrid.Transport
	raw := "From: b@test\r\nTo: a@example.com\r\nMessage-ID: <x@test>\r\nSubject: hi\r\n\r\nhello"
	path := t.TempDir() + "/m.eml"
	msg, binding, err := tr.Receive(context.Background(), signedRequest(t, priv, "a@example.com", raw, ""), fakeResolver{publicKey: pub}, path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Provider != "sendgrid" || msg.Recipient != "a@example.com" {
		t.Fatalf("msg %+v", msg)
	}
	if msg.DeliveryID != "<x@test>" || msg.ProviderMessageID != "<x@test>" {
		t.Fatalf("delivery id %q provider id %q", msg.DeliveryID, msg.ProviderMessageID)
	}
	if msg.EnvelopeFrom != "b@test" {
		t.Fatalf("envelope from %q", msg.EnvelopeFrom)
	}
	if binding.AccountID != "acc" || binding.CredentialID != "cred" {
		t.Fatalf("binding %+v", binding)
	}
	staged, _ := os.ReadFile(msg.RawPath)
	if !strings.Contains(string(staged), "hello") {
		t.Fatal("raw mime missing or truncated")
	}
}

func TestReceiveBadSignatureRejected(t *testing.T) {
	_, pub := keypair(t)
	other, _ := keypair(t)
	var tr sendgrid.Transport
	raw := "From: b@test\r\n\r\nhello"
	req := signedRequest(t, other, "a@example.com", raw, "")
	if _, _, err := tr.Receive(context.Background(), req, fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveStaleTimestampRejected(t *testing.T) {
	priv, pub := keypair(t)
	var tr sendgrid.Transport
	ts := fmt.Sprintf("%d", time.Now().Add(-48*time.Hour).Unix())
	raw := "From: b@test\r\n\r\nhello"
	req := signedRequest(t, priv, "a@example.com", raw, ts)
	if _, _, err := tr.Receive(context.Background(), req, fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveMissingRecipientRejected(t *testing.T) {
	priv, pub := keypair(t)
	var tr sendgrid.Transport
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("envelope", `{"to":[],"from":"b@test"}`)
	p, _ := w.CreateFormField("email")
	p.Write([]byte("From: b@test\r\n\r\nhi"))
	w.Close()
	r := httptest.NewRequest("POST", "/", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	r.Header.Set("X-Twilio-Email-Event-Webhook-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	_ = priv
	if _, _, err := tr.Receive(context.Background(), r, fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 4096); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveDeliveryIDFallsBackToBodyHash(t *testing.T) {
	priv, pub := keypair(t)
	var tr sendgrid.Transport
	raw := "From: b@test\r\nTo: a@example.com\r\n\r\nno message id"
	msg, _, err := tr.Receive(context.Background(), signedRequest(t, priv, "a@example.com", raw, ""), fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg.DeliveryID, "sg-") || len(msg.DeliveryID) < 10 {
		t.Fatalf("delivery id %q", msg.DeliveryID)
	}
}

func TestReceiveRejectsBadContentTypeAndOversize(t *testing.T) {
	priv, pub := keypair(t)
	var tr sendgrid.Transport
	r := httptest.NewRequest("POST", "/", strings.NewReader("x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, _, err := tr.Receive(context.Background(), r, fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 4096); err == nil {
		t.Fatal("bad content type accepted")
	}
	raw := strings.Repeat("a", 8192)
	if _, _, err := tr.Receive(context.Background(), signedRequest(t, priv, "a@example.com", raw, ""), fakeResolver{publicKey: pub}, t.TempDir()+"/m.eml", 1024); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize err=%v", err)
	}
}
