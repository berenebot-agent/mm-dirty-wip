package cloudflare_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/cloudflare"
)

var _ transport.InboundTransport = cloudflare.Transport{}

type fakeResolver struct {
	binding transport.InboundBinding
	err     error
}

func (f fakeResolver) ResolveInboundBinding(_ context.Context, provider, recipient string) (transport.InboundBinding, error) {
	if f.err != nil {
		return transport.InboundBinding{}, f.err
	}
	b := f.binding
	b.Provider = provider
	b.Recipient = recipient
	return b, nil
}

func cfResolver() fakeResolver {
	return fakeResolver{binding: transport.InboundBinding{
		AccountID: "acc", DomainID: "dom", CredentialID: "cred",
		Config: map[string]any{"webhook_secret": "s3cret"},
	}}
}

func workerRequest(t *testing.T, secret, deliveryID, raw string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader(raw))
	r.Header.Set("Content-Type", "message/rfc822")
	r.Header.Set(cloudflare.HeaderRecipient, "hermes@example.com")
	r.Header.Set(cloudflare.HeaderEnvelopeTo, "sender@outside.test")
	if deliveryID != "" {
		r.Header.Set(cloudflare.HeaderDeliveryID, deliveryID)
	}
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	return r
}

func TestReceiveAndVerify(t *testing.T) {
	var tr cloudflare.Transport
	raw := "From: sender@outside.test\r\nTo: hermes@example.com\r\nSubject: hi\r\n\r\nhello"
	path := t.TempDir() + "/m.eml"
	msg, binding, err := tr.Receive(context.Background(), workerRequest(t, "s3cret", "cf-delivery-1", raw), cfResolver(), path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Provider != "cloudflare" || msg.Recipient != "hermes@example.com" || msg.DeliveryID != "cf-delivery-1" {
		t.Fatalf("%+v", msg)
	}
	if binding.AccountID != "acc" || binding.CredentialID != "cred" {
		t.Fatalf("binding %+v", binding)
	}
	staged, _ := os.ReadFile(msg.RawPath)
	if !strings.Contains(string(staged), "hello") {
		t.Fatal("raw mime missing")
	}
}

func TestWrongBearerRejectedBeforeBody(t *testing.T) {
	var tr cloudflare.Transport
	var read bool
	body := &trackingReader{read: &read, r: strings.NewReader("From: x\r\n\r\nbody")}
	r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", body)
	r.Header.Set("Content-Type", "message/rfc822")
	r.Header.Set(cloudflare.HeaderRecipient, "hermes@example.com")
	r.Header.Set("Authorization", "Bearer wrong")
	if _, _, err := tr.Receive(context.Background(), r, cfResolver(), t.TempDir()+"/m.eml", 1024); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
	if read {
		t.Fatal("body must not be read before authentication")
	}
}

type trackingReader struct {
	read *bool
	r    *strings.Reader
}

func (t *trackingReader) Read(p []byte) (int, error) { *t.read = true; return t.r.Read(p) }

func TestMissingRecipientRejected(t *testing.T) {
	var tr cloudflare.Transport
	r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader("body"))
	r.Header.Set("Authorization", "Bearer s3cret")
	if _, _, err := tr.Receive(context.Background(), r, cfResolver(), t.TempDir()+"/m.eml", 1024); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestDeliveryIDFallsBackToMIMEHash(t *testing.T) {
	var tr cloudflare.Transport
	raw := "From: sender@outside.test\r\nTo: hermes@example.com\r\n\r\nhello"
	msg, _, err := tr.Receive(context.Background(), workerRequest(t, "s3cret", "", raw), cfResolver(), t.TempDir()+"/m.eml", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg.DeliveryID, "cf-") || len(msg.DeliveryID) < 10 {
		t.Fatalf("delivery id %q", msg.DeliveryID)
	}
	msg2, _, err := tr.Receive(context.Background(), workerRequest(t, "s3cret", "", raw), cfResolver(), t.TempDir()+"/m2.eml", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if msg.DeliveryID != msg2.DeliveryID {
		t.Fatal("same MIME must hash to the same delivery id")
	}
}

func TestRejectsBadPayload(t *testing.T) {
	var tr cloudflare.Transport
	raw := "From: sender@outside.test\r\n\r\nhello"
	if _, _, err := tr.Receive(context.Background(), workerRequest(t, "s3cret", "", ""), cfResolver(), t.TempDir()+"/m.eml", 1024); err == nil {
		t.Fatal("empty body accepted")
	}
	r := workerRequest(t, "s3cret", "", raw)
	r.Header.Set("Content-Type", "application/json")
	if _, _, err := tr.Receive(context.Background(), r, cfResolver(), t.TempDir()+"/m.eml", 1024); err == nil {
		t.Fatal("bad content type accepted")
	}
	if _, _, err := tr.Receive(context.Background(), workerRequest(t, "s3cret", "", raw), cfResolver(), t.TempDir()+"/m.eml", 4); err == nil {
		t.Fatal("oversize accepted")
	}
}
