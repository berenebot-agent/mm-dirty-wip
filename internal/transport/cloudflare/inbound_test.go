package cloudflare

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gatehouse-mail/internal/transport"
)

var _ transport.InboundTransport = Transport{}

func workerRequest(t *testing.T, secret string, mutate func(*WorkerPayload)) *http.Request {
	t.Helper()
	p := WorkerPayload{
		Recipient:    "hermes@example.com",
		EnvelopeFrom: "sender@outside.test",
		RawMIMEB64:   base64.StdEncoding.EncodeToString([]byte("From: sender@outside.test\r\nTo: hermes@example.com\r\nSubject: hi\r\n\r\nhello")),
		DeliveryID:   "cf-delivery-1",
	}
	if mutate != nil {
		mutate(&p)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	return r
}

func TestParseAndVerify(t *testing.T) {
	var tr Transport
	r := workerRequest(t, "s3cret", nil)
	msg, err := tr.Parse(r, t.TempDir()+"/m.eml", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Provider != "cloudflare" || msg.Recipient != "hermes@example.com" || msg.DeliveryID != "cf-delivery-1" {
		t.Fatalf("%+v", msg)
	}
	raw, _ := os.ReadFile(msg.RawPath)
	if !bytes.Contains(raw, []byte("hello")) {
		t.Fatal("raw mime missing")
	}
	// Rebuild the request for Verify since Parse consumed the body.
	r = workerRequest(t, "s3cret", nil)
	if err = tr.Verify(r, msg, "s3cret"); err != nil {
		t.Fatalf("valid bearer rejected: %v", err)
	}
	r = workerRequest(t, "wrong", nil)
	if err = tr.Verify(r, msg, "s3cret"); err == nil {
		t.Fatal("wrong bearer accepted")
	}
	r = workerRequest(t, "", nil)
	if err = tr.Verify(r, msg, "s3cret"); err == nil {
		t.Fatal("missing bearer accepted")
	}
}

func TestDeliveryIDFallsBackToMIMEHash(t *testing.T) {
	var tr Transport
	r := workerRequest(t, "s3cret", func(p *WorkerPayload) { p.DeliveryID = "" })
	msg, err := tr.Parse(r, t.TempDir()+"/m.eml", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg.DeliveryID, "cf-") || len(msg.DeliveryID) < 10 {
		t.Fatalf("delivery id %q", msg.DeliveryID)
	}
	r2 := workerRequest(t, "s3cret", func(p *WorkerPayload) { p.DeliveryID = "" })
	msg2, err := tr.Parse(r2, t.TempDir()+"/m2.eml", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if msg.DeliveryID != msg2.DeliveryID {
		t.Fatal("same MIME must hash to the same delivery id")
	}
}

func TestRejectsBadPayload(t *testing.T) {
	var tr Transport
	for _, mutate := range []func(*WorkerPayload){
		func(p *WorkerPayload) { p.Recipient = "" },
		func(p *WorkerPayload) { p.RawMIMEB64 = "" },
		func(p *WorkerPayload) { p.RawMIMEB64 = "!!not-base64!!" },
	} {
		r := workerRequest(t, "s3cret", mutate)
		if _, err := tr.Parse(r, t.TempDir()+"/m.eml", 1024); err == nil {
			t.Fatal("bad payload accepted")
		}
	}
	r := workerRequest(t, "s3cret", nil)
	if _, err := tr.Parse(r, t.TempDir()+"/m.eml", 8); err == nil {
		t.Fatal("oversize accepted")
	}
}
