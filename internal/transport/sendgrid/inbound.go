// Package sendgrid adapts Twilio SendGrid Inbound Parse webhooks to the
// generic inbound boundary.
//
// SendGrid POSTs a multipart/form-data payload. With "POST the raw, full MIME
// message" enabled it includes the original message as an `email` part and the
// SMTP envelope as a JSON `envelope` field. When a webhook security policy with
// signature verification is attached, every request carries an ECDSA signature
// over sha256(timestamp || raw request body). Verifying it requires the public
// key stored on the recipient's domain, and the key is selected by the envelope
// recipient, so the body is staged to disk first and read in two passes: the
// first reads the small envelope field to resolve the key, the second extracts
// the raw MIME only after the signature is verified.
package sendgrid

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
)

// Transport adapts SendGrid Inbound Parse to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return "sendgrid" }
func (Transport) Description() string { return "SendGrid" }
func (Transport) IngestPath() string  { return "/internal/ingest/sendgrid" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "public_key", Label: "Webhook verification public key", Type: "text", Required: true, Placeholder: "Base64 public key from the Inbound Parse security policy"},
	}
}

// Webhook signature headers sent with every Inbound Parse request that has a
// signature security policy attached.
const (
	headerSignature = "X-Twilio-Email-Event-Webhook-Signature"
	headerTimestamp = "X-Twilio-Email-Event-Webhook-Timestamp"
)

const (
	maxMultipartParts = 64
	maxFieldBytes     = 1 << 20
)

// signatureMaxAge bounds how old a signed request may be. SendGrid may retry
// delivery, and the raw MIME Message-ID already deduplicates replays, so a
// generous window avoids dropping legitimately delayed deliveries.
const signatureMaxAge = 24 * time.Hour

// envelope is the SMTP envelope subset SendGrid includes as a JSON field.
type envelope struct {
	From string   `json:"from"`
	To   []string `json:"to"`
}

// Receive authenticates the SendGrid webhook, stages the raw MIME to tmpPath and
// returns the binding it verified. The raw message is not persisted until the
// signature is verified.
func (Transport) Receive(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (transport.InboundMessage, transport.InboundBinding, error) {
	var msg transport.InboundMessage
	var binding transport.InboundBinding

	media, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := strings.TrimSpace(params["boundary"])
	if media != "multipart/form-data" || boundary == "" {
		return msg, binding, fmt.Errorf("unsupported content type %s", media)
	}
	maxParts := resolverMultipartLimit(resolver)
	if maxParts <= 0 {
		maxParts = maxMultipartParts
	}

	// Stage the whole body to disk so the signature can be verified over the
	// exact received bytes without buffering a large message in memory.
	bodyCap := maxBytes*3 + maxFieldBytes
	r.Body = http.MaxBytesReader(nil, r.Body, bodyCap)
	bodyPath := tmpPath + ".body"
	defer os.Remove(bodyPath)
	if err := stageBody(r.Body, bodyPath, bodyCap); err != nil {
		return msg, binding, err
	}

	// First pass: read only the small envelope/to fields; skip the raw MIME.
	env, err := scanEnvelope(bodyPath, boundary, maxParts)
	if err != nil {
		return msg, binding, err
	}
	recipient := primaryRecipient(env)
	if recipient == "" {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	binding, err = resolver.ResolveInboundBinding(ctx, "sendgrid", recipient)
	if err != nil {
		return msg, binding, err
	}
	publicKey, _ := binding.Config["public_key"].(string)
	if !verifySignature(publicKey, r.Header, bodyPath) {
		return msg, binding, transport.ErrInboundUnauthorized
	}

	// Second pass: the signature is verified, so extract the raw MIME.
	size, err := extractRawEmail(bodyPath, boundary, tmpPath, maxBytes, maxParts)
	if err != nil {
		return msg, binding, err
	}
	deliveryID := messageID(tmpPath)
	providerMessageID := deliveryID
	if deliveryID == "" {
		deliveryID = "sg-" + hex.EncodeToString(sha256File(tmpPath))
		providerMessageID = ""
	}
	return transport.InboundMessage{
		Provider:          "sendgrid",
		Recipient:         binding.Recipient,
		EnvelopeFrom:      strings.TrimSpace(env.From),
		RawPath:           tmpPath,
		Size:              size,
		DeliveryID:        deliveryID,
		ProviderMessageID: providerMessageID,
	}, binding, nil
}

// stageBody streams r to path, enforcing the byte cap.
func stageBody(r io.Reader, path string, cap int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, cap+1))
	if err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if n > cap {
		return fmt.Errorf("message too large")
	}
	if n == 0 {
		return fmt.Errorf("empty message")
	}
	return nil
}

// scanEnvelope reads the multipart body once and returns the SMTP envelope. The
// raw `email` part is discarded, not retained.
func scanEnvelope(bodyPath, boundary string, maxParts int) (envelope, error) {
	var env envelope
	f, err := os.Open(bodyPath)
	if err != nil {
		return env, err
	}
	defer f.Close()
	mr := multipart.NewReader(f, boundary)
	var (
		seenEnvelope bool
		parts        int
	)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return env, err
		}
		parts++
		if parts > maxParts {
			p.Close()
			return env, fmt.Errorf("too many multipart parts")
		}
		if p.FormName() != "envelope" {
			io.Copy(io.Discard, p)
			p.Close()
			continue
		}
		if seenEnvelope {
			p.Close()
			return env, fmt.Errorf("duplicate field envelope")
		}
		seenEnvelope = true
		b, err := io.ReadAll(io.LimitReader(p, maxFieldBytes+1))
		p.Close()
		if err != nil {
			return env, err
		}
		if int64(len(b)) > maxFieldBytes {
			return env, fmt.Errorf("form field too large")
		}
		if err := json.Unmarshal(b, &env); err != nil {
			return env, fmt.Errorf("invalid envelope json")
		}
	}
	return env, nil
}

// extractRawEmail performs a second pass over the staged body, writing the raw
// `email` part to tmpPath bounded by maxBytes, and returns its size.
func extractRawEmail(bodyPath, boundary, tmpPath string, maxBytes int64, maxParts int) (int64, error) {
	f, err := os.Open(bodyPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	mr := multipart.NewReader(f, boundary)
	var (
		wrote bool
		parts int
		size  int64
	)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			dst.Close()
			return 0, err
		}
		parts++
		if parts > maxParts {
			p.Close()
			dst.Close()
			return 0, fmt.Errorf("too many multipart parts")
		}
		if p.FormName() != "email" {
			io.Copy(io.Discard, p)
			p.Close()
			continue
		}
		if wrote {
			p.Close()
			dst.Close()
			return 0, fmt.Errorf("multiple raw email parts")
		}
		wrote = true
		size, err = io.Copy(dst, io.LimitReader(p, maxBytes+1))
		p.Close()
		if err != nil {
			dst.Close()
			return 0, err
		}
	}
	if err := dst.Close(); err != nil {
		return 0, err
	}
	if !wrote {
		return 0, fmt.Errorf("raw email missing")
	}
	if size > maxBytes {
		return 0, fmt.Errorf("message too large")
	}
	if size == 0 {
		return 0, fmt.Errorf("empty message")
	}
	return size, nil
}

// primaryRecipient returns the canonical first SMTP envelope recipient.
func primaryRecipient(env envelope) string {
	for _, to := range env.To {
		if c := canonicalRecipient(to); c != "" {
			return c
		}
	}
	return ""
}

// verifySignature validates the SendGrid ECDSA signature over
// sha256(timestamp || raw body), streaming the staged body. The public key is
// the base64 (or PEM) SubjectPublicKeyInfo issued by the Inbound Parse security
// policy.
func verifySignature(publicKey string, h http.Header, bodyPath string) bool {
	sig := strings.TrimSpace(h.Get(headerSignature))
	ts := strings.TrimSpace(h.Get(headerTimestamp))
	if strings.TrimSpace(publicKey) == "" || sig == "" || ts == "" {
		return false
	}
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if delta := time.Since(time.Unix(seconds, 0)); delta < -signatureMaxAge || delta > signatureMaxAge {
		return false
	}
	pub, err := parsePublicKey(publicKey)
	if err != nil {
		return false
	}
	signature, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	f, err := os.Open(bodyPath)
	if err != nil {
		return false
	}
	defer f.Close()
	hasher := sha256.New()
	hasher.Write([]byte(ts))
	if _, err := io.Copy(hasher, f); err != nil {
		return false
	}
	return ecdsa.VerifyASN1(pub, hasher.Sum(nil), signature)
}

// parsePublicKey accepts the SendGrid public key as a PEM block or a base64
// SubjectPublicKeyInfo and returns the ECDSA public key.
func parsePublicKey(raw string) (*ecdsa.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	var der []byte
	if block, _ := pem.Decode([]byte(raw)); block != nil {
		der = block.Bytes
	} else if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		der = b
	} else if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		der = b
	} else {
		return nil, fmt.Errorf("invalid public key encoding")
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is not ECDSA")
	}
	return pub, nil
}

// sha256File returns the SHA-256 digest of a file, used for the delivery-id
// fallback when the MIME carries no Message-ID.
func sha256File(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil
	}
	return h.Sum(nil)
}

// messageID returns the Message-ID header of the staged raw MIME, or "" when it
// is absent.
func messageID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	m, err := mail.ReadMessage(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(m.Header.Get("Message-Id"))
}

// canonicalRecipient strips any display name and lowercases the address.
func canonicalRecipient(v string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// resolverMultipartLimit returns the operator-configured multipart part cap
// when the resolver exposes one, or 0 to use the adapter default.
func resolverMultipartLimit(resolver transport.BindingResolver) int {
	if p, ok := resolver.(transport.MultipartLimitProvider); ok {
		return p.MaxMultipartParts()
	}
	return 0
}
