// Package cloudflare adapts Cloudflare Email Routing webhooks to the
// generic inbound boundary.
//
// Email Routing cannot POST directly, so a Cloudflare Worker forwards each
// message: it holds the raw MIME from the EmailMessage, wraps it in JSON,
// and POSTs to /internal/ingest/cloudflare with a shared Bearer secret.
package cloudflare

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"gatehouse-mail/internal/transport"
)

// WorkerPayload is the JSON contract the Cloudflare Worker posts.
type WorkerPayload struct {
	Recipient    string `json:"recipient"`
	EnvelopeFrom string `json:"envelope_from"`
	RawMIMEB64   string `json:"raw_mime_b64"`
	ReceivedAt   string `json:"received_at"`
	DeliveryID   string `json:"delivery_id"`
}

// Transport adapts the Worker JSON webhook to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return "cloudflare" }

func (Transport) Parse(r *http.Request, tmpPath string, maxBytes int64) (transport.InboundMessage, error) {
	var out transport.InboundMessage
	ct := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if ct != "" && ct != "application/json" {
		return out, fmt.Errorf("unsupported content type %s", ct)
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes+1024)
	var p WorkerPayload
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return out, fmt.Errorf("invalid worker payload: %w", err)
	}
	if strings.TrimSpace(p.Recipient) == "" {
		return out, fmt.Errorf("recipient required")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.RawMIMEB64))
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(strings.TrimSpace(p.RawMIMEB64))
		if err != nil {
			return out, fmt.Errorf("invalid raw_mime_b64: %w", err)
		}
	}
	if int64(len(raw)) > maxBytes {
		return out, fmt.Errorf("message too large")
	}
	if len(raw) == 0 {
		return out, fmt.Errorf("raw_mime_b64 required")
	}
	if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
		return out, err
	}
	received := strings.TrimSpace(p.ReceivedAt)
	if received == "" {
		received = time.Now().UTC().Format(time.RFC3339Nano)
	}
	delivery := strings.TrimSpace(p.DeliveryID)
	if delivery == "" {
		sum := sha256.Sum256(raw)
		delivery = "cf-" + hex.EncodeToString(sum[:])
	}
	return transport.InboundMessage{
		Provider:     "cloudflare",
		Recipient:    strings.TrimSpace(p.Recipient),
		EnvelopeFrom: strings.TrimSpace(p.EnvelopeFrom),
		RawPath:      tmpPath,
		Size:         int64(len(raw)),
		DeliveryID:   delivery,
		Timestamp:    received,
	}, nil
}

func (Transport) Verify(r *http.Request, _ transport.InboundMessage, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("CLOUDFLARE_WEBHOOK_SECRET is not configured")
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return transport.ErrInboundUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(prefix):])), []byte(secret)) != 1 {
		return transport.ErrInboundUnauthorized
	}
	return nil
}
