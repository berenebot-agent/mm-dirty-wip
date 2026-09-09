// Package cloudflare adapts Cloudflare Email Routing webhooks to the generic
// inbound boundary.
//
// Email Routing cannot POST directly, so a Cloudflare Worker forwards each
// message: it streams the raw MIME from the EmailMessage to
// /internal/ingest/cloudflare with a shared Bearer secret and envelope headers.
package cloudflare

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gatehouse-mail/internal/transport"
)

// Transport adapts the Worker webhook to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return "cloudflare" }
func (Transport) Description() string { return "Cloudflare Worker" }
func (Transport) IngestPath() string  { return "/internal/ingest/cloudflare" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "webhook_secret", Label: "Worker shared secret", Type: "password", Required: true, Secret: true, Generated: true, Placeholder: "Generated automatically"},
	}
}

// Header names of the Worker webhook contract.
const (
	HeaderRecipient  = "X-Gatehouse-Recipient"
	HeaderEnvelopeTo = "X-Gatehouse-Envelope-From"
	HeaderDeliveryID = "X-Gatehouse-Delivery-ID"
	HeaderReceivedAt = "X-Gatehouse-Received-At"

	maxHeaderValue = 512
)

// Receive authenticates the Worker request before touching the MIME body. The
// envelope recipient header selects the domain and its assigned shared secret;
// it is a routing hint only and grants no authority until the bearer matches.
func (Transport) Receive(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (transport.InboundMessage, transport.InboundBinding, error) {
	var msg transport.InboundMessage
	var binding transport.InboundBinding

	ct := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if ct != "" && ct != "message/rfc822" && ct != "application/octet-stream" {
		return msg, binding, fmt.Errorf("unsupported content type %s", ct)
	}
	recipient := strings.TrimSpace(r.Header.Get(HeaderRecipient))
	if recipient == "" || len(recipient) > maxHeaderValue {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	binding, err := resolver.ResolveInboundBinding(ctx, "cloudflare", canonicalRecipient(recipient))
	if err != nil {
		return msg, binding, err
	}
	secret, _ := binding.Config["webhook_secret"].(string)
	if !validBearer(r, secret) {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	delivery := strings.TrimSpace(r.Header.Get(HeaderDeliveryID))
	if len(delivery) > maxHeaderValue {
		return msg, binding, fmt.Errorf("delivery id too long")
	}
	envelopeFrom := strings.TrimSpace(r.Header.Get(HeaderEnvelopeTo))
	if len(envelopeFrom) > maxHeaderValue {
		envelopeFrom = ""
	}

	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes+1)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return msg, binding, err
	}
	hasher := sha256.New()
	n, err := io.Copy(f, io.TeeReader(r.Body, hasher))
	closeErr := f.Close()
	if err != nil {
		return msg, binding, err
	}
	if closeErr != nil {
		return msg, binding, closeErr
	}
	if n > maxBytes {
		return msg, binding, fmt.Errorf("message too large")
	}
	if n == 0 {
		return msg, binding, fmt.Errorf("empty message")
	}
	if delivery == "" {
		delivery = "cf-" + hex.EncodeToString(hasher.Sum(nil))
	}
	msg = transport.InboundMessage{
		Provider:     "cloudflare",
		Recipient:    binding.Recipient,
		EnvelopeFrom: envelopeFrom,
		RawPath:      tmpPath,
		Size:         n,
		DeliveryID:   delivery,
	}
	return msg, binding, nil
}

// validBearer compares the Authorization bearer to the configured secret in
// constant time. An empty configured secret never authenticates.
func validBearer(r *http.Request, secret string) bool {
	if strings.TrimSpace(secret) == "" {
		return false
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[len(prefix):])), []byte(secret)) == 1
}

func canonicalRecipient(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
