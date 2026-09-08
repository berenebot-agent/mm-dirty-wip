package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gatehouse-mail/internal/transport"
	_ "gatehouse-mail/internal/transport/cloudflare"
)

func cfRequest(t *testing.T, secret, deliveryID, recipient, raw string) *http.Request {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"recipient":     recipient,
		"envelope_from": "sender@outside.test",
		"raw_mime_b64":  base64.StdEncoding.EncodeToString([]byte(raw)),
		"delivery_id":   deliveryID,
		"received_at":   time.Now().UTC().Format(time.RFC3339Nano),
	})
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

func TestCloudflareIngestAndDedup(t *testing.T) {
	svc, _, _, box := testService(t)
	svc.Config.CloudflareSecret = "cf-secret"
	ctx := context.Background()
	raw := "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: via cloudflare\r\nMessage-ID: <cf-inbound@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\ncloudflare body"
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, "cf-secret", "cf-1", box.Address, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	if m.Provider != "cloudflare" || m.Subject != "via cloudflare" {
		t.Fatalf("%+v", m)
	}
	m2, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, "cf-secret", "cf-1", box.Address, raw))
	if err != nil || !dup || m2.ID != m.ID {
		t.Fatalf("dedup %v dup=%v", err, dup)
	}
}

func TestInboundUnknownProviderAndUnauthorized(t *testing.T) {
	svc, _, _, box := testService(t)
	svc.Config.CloudflareSecret = "cf-secret"
	ctx := context.Background()
	raw := "From: x@y.test\r\nTo: hermes@example.com\r\nSubject: nope\r\n\r\nhi"
	if _, _, err := svc.IngestInbound(ctx, "nope", cfRequest(t, "cf-secret", "x", box.Address, raw)); !errors.Is(err, transport.ErrUnknownProvider) {
		t.Fatalf("unknown provider err=%v", err)
	}
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, "wrong", "x", box.Address, raw)); !errors.Is(err, transport.ErrInboundUnauthorized) {
		t.Fatalf("unauthorized err=%v", err)
	}
}

func TestMailgunCompatRouteStillIngests(t *testing.T) {
	svc, _, _, box := testService(t)
	ctx := context.Background()
	raw := "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: Hello\r\nMessage-ID: <inbound-compat@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nPlease reply"
	m, dup, err := svc.IngestMailgun(ctx, mgRequest(t, svc.Config.MailgunSigningKey, "compat-1", box.Address, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	if m.Provider != "mailgun" {
		t.Fatalf("provider %q", m.Provider)
	}
}
