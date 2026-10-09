package mailgun

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
)

// InboundForm holds the Mailgun webhook fields the adapter consumes. Auth
// material (Timestamp/Token/Signature) stays internal to this package.
type InboundForm struct {
	Timestamp         string
	Token             string
	Signature         string
	Sender            string
	Recipient         string
	ProviderMessageID string
	RawPath           string
	Size              int64
}

// Transport adapts Mailgun HTTPS webhooks to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return "mailgun" }
func (Transport) Description() string { return "Mailgun" }
func (Transport) IngestPath() string  { return "/internal/ingest/mailgun/raw-mime" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "signing_key", Label: "Webhook signing key", Type: "password", Required: true, Secret: true, Placeholder: "Mailgun HTTP webhook signing key"},
	}
}

// Receive authenticates the Mailgun webhook, stages the raw MIME to tmpPath and
// returns the binding it verified. The canonical envelope recipient selects the
// domain and its assigned signing key.
func (Transport) Receive(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (transport.InboundMessage, transport.InboundBinding, error) {
	ct := r.Header.Get("Content-Type")
	media, params, _ := mime.ParseMediaType(ct)
	var (
		form    InboundForm
		binding transport.InboundBinding
		err     error
	)
	switch {
	case media == "multipart/form-data":
		form, binding, err = receiveMultipart(ctx, r, resolver, params["boundary"], tmpPath, maxBytes, resolverMultipartLimit(resolver))
	case media == "application/x-www-form-urlencoded" || media == "":
		form, binding, err = receiveURLEncoded(ctx, r, resolver, tmpPath, maxBytes)
	default:
		return transport.InboundMessage{}, transport.InboundBinding{}, fmt.Errorf("unsupported content type %s", media)
	}
	if err != nil {
		return transport.InboundMessage{}, transport.InboundBinding{}, err
	}
	return transport.InboundMessage{
		Provider:          "mailgun",
		Recipient:         binding.Recipient,
		EnvelopeFrom:      form.Sender,
		RawPath:           form.RawPath,
		Size:              form.Size,
		DeliveryID:        form.Token,
		ProviderMessageID: form.ProviderMessageID,
	}, binding, nil
}

// verifyBinding resolves the recipient's receive connection and checks the
// Mailgun HMAC. The signature covers timestamp+token, not the recipient or MIME.
func verifyBinding(ctx context.Context, resolver transport.BindingResolver, form InboundForm) (transport.InboundBinding, error) {
	if strings.TrimSpace(form.Recipient) == "" {
		return transport.InboundBinding{}, fmt.Errorf("recipient missing")
	}
	recipient := canonicalRecipient(form.Recipient)
	binding, err := resolver.ResolveInboundBinding(ctx, "mailgun", recipient)
	if err != nil {
		return transport.InboundBinding{}, err
	}
	key, _ := binding.Config["signing_key"].(string)
	if !VerifySignature(key, form.Timestamp, form.Token, form.Signature) {
		return transport.InboundBinding{}, transport.ErrInboundUnauthorized
	}
	return binding, nil
}

// canonicalRecipient strips any display name and lowercases the address.
func canonicalRecipient(v string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// authReady reports whether the fields needed to resolve and verify are present.
func authReady(form InboundForm) bool {
	return strings.TrimSpace(form.Timestamp) != "" && strings.TrimSpace(form.Token) != "" &&
		strings.TrimSpace(form.Signature) != "" && strings.TrimSpace(form.Recipient) != ""
}

// receiveMultipart streams the Mailgun multipart webhook. It verifies as soon
// as the auth and recipient fields are available; if the MIME part arrives
// first it is staged to disk but not parsed or committed until verification.
func receiveMultipart(ctx context.Context, r *http.Request, resolver transport.BindingResolver, boundary, tmpPath string, maxBytes int64, maxParts int) (InboundForm, transport.InboundBinding, error) {
	var form InboundForm
	if boundary == "" {
		return form, transport.InboundBinding{}, fmt.Errorf("invalid multipart boundary")
	}
	if maxParts <= 0 {
		maxParts = maxMultipartParts
	}
	mr := multipart.NewReader(r.Body, boundary)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return form, transport.InboundBinding{}, err
	}
	defer f.Close()
	var (
		wroteMIME bool
		verified  bool
		binding   transport.InboundBinding
		seen      = map[string]bool{}
		parts     int
	)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return form, transport.InboundBinding{}, err
		}
		parts++
		if parts > maxParts {
			p.Close()
			return form, transport.InboundBinding{}, fmt.Errorf("too many multipart parts")
		}
		name := p.FormName()
		if name == "body-mime" {
			if wroteMIME {
				p.Close()
				return form, transport.InboundBinding{}, fmt.Errorf("multiple body-mime parts")
			}
			n, err := io.Copy(f, io.LimitReader(p, maxBytes+1))
			p.Close()
			if err != nil {
				return form, transport.InboundBinding{}, err
			}
			if n > maxBytes {
				return form, transport.InboundBinding{}, fmt.Errorf("message too large")
			}
			form.Size = n
			form.RawPath = tmpPath
			wroteMIME = true
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p, maxFieldBytes+1))
		p.Close()
		if err != nil {
			return form, transport.InboundBinding{}, err
		}
		if int64(len(b)) > maxFieldBytes {
			return form, transport.InboundBinding{}, fmt.Errorf("form field too large")
		}
		if err = setField(&form, name, string(b), seen); err != nil {
			return form, transport.InboundBinding{}, err
		}
		if !verified && authReady(form) {
			binding, err = verifyBinding(ctx, resolver, form)
			if err != nil {
				return form, transport.InboundBinding{}, err
			}
			verified = true
		}
	}
	if !wroteMIME {
		return form, transport.InboundBinding{}, fmt.Errorf("body-mime missing")
	}
	if !verified {
		if !authReady(form) {
			return form, transport.InboundBinding{}, transport.ErrInboundUnauthorized
		}
		if binding, err = verifyBinding(ctx, resolver, form); err != nil {
			return form, transport.InboundBinding{}, err
		}
	}
	return form, binding, nil
}

// receiveURLEncoded handles application/x-www-form-urlencoded webhooks with
// explicit caps on the encoded body, field size and field count. MIME streams
// to disk rather than being buffered in memory. The encoded cap allows the worst-case
// percent-encoding expansion (3x) of a full-size body plus one non-MIME field,
// so no legitimate message that fits the MIME limit is rejected.
func receiveURLEncoded(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (InboundForm, transport.InboundBinding, error) {
	var form InboundForm
	encodedCap := maxBytes*3 + maxFieldBytes
	r.Body = http.MaxBytesReader(nil, r.Body, encodedCap)
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return form, transport.InboundBinding{}, err
	}
	defer f.Close()
	reader := bufio.NewReader(r.Body)
	mimeWriter := bufio.NewWriter(f)
	seen := map[string]bool{}
	wroteMIME := false
	for fields := 0; ; fields++ {
		if _, err := reader.Peek(1); err == io.EOF {
			break
		} else if err != nil {
			return form, transport.InboundBinding{}, err
		}
		if fields >= maxFormFields {
			return form, transport.InboundBinding{}, fmt.Errorf("too many form fields")
		}
		var name strings.Builder
		_, delimiter, err := decodeFormPart(reader, &name, 1024, true)
		if err != nil {
			return form, transport.InboundBinding{}, err
		}
		if delimiter != '=' {
			continue
		}
		if name.String() == "body-mime" {
			if wroteMIME {
				return form, transport.InboundBinding{}, fmt.Errorf("multiple body-mime parts")
			}
			form.Size, _, err = decodeFormPart(reader, mimeWriter, maxBytes, false)
			wroteMIME = true
		} else {
			var value strings.Builder
			_, _, err = decodeFormPart(reader, &value, maxFieldBytes, false)
			if err == nil {
				err = setField(&form, name.String(), value.String(), seen)
			}
		}
		if err != nil {
			return form, transport.InboundBinding{}, err
		}
	}
	if !wroteMIME || form.Size == 0 {
		return form, transport.InboundBinding{}, fmt.Errorf("body-mime missing")
	}
	if !authReady(form) {
		return form, transport.InboundBinding{}, transport.ErrInboundUnauthorized
	}
	binding, err := verifyBinding(ctx, resolver, form)
	if err != nil {
		return form, transport.InboundBinding{}, err
	}
	if err = mimeWriter.Flush(); err != nil {
		return form, transport.InboundBinding{}, err
	}
	form.RawPath = tmpPath
	return form, binding, nil
}

// decodeFormPart decodes one query component with bounded memory. MIME bytes
// stream to disk; each field occurrence (including repeated unknown keys) counts.
func decodeFormPart(r *bufio.Reader, w io.ByteWriter, limit int64, name bool) (int64, byte, error) {
	var n int64
	for {
		c, err := r.ReadByte()
		if err == io.EOF {
			return n, 0, nil
		}
		if err != nil {
			return n, 0, err
		}
		if c == '&' || (name && c == '=') {
			return n, c, nil
		}
		if c == ';' {
			return n, 0, fmt.Errorf("invalid urlencoded form")
		}
		if c == '+' {
			c = ' '
		} else if c == '%' {
			var pair [2]byte
			if _, err := io.ReadFull(r, pair[:]); err != nil {
				return n, 0, fmt.Errorf("invalid urlencoded form")
			}
			decoded, err := url.QueryUnescape("%" + string(pair[:]))
			if err != nil {
				return n, 0, fmt.Errorf("invalid urlencoded form")
			}
			c = decoded[0]
		}
		n++
		if n > limit {
			return n, 0, fmt.Errorf("form field too large")
		}
		if err := w.WriteByte(c); err != nil {
			return n, 0, err
		}
	}
}

// bounds for Mailgun webhook parsing. The multipart part count is capped so a
// malicious webhook cannot force unbounded iteration; individual non-MIME
// fields are capped separately from the MIME body.
const (
	maxMultipartParts = 64
	maxFormFields     = 64
	maxFieldBytes     = 1 << 20
)

// resolverMultipartLimit returns the operator-configured multipart part cap
// when the resolver exposes one, or 0 to use the adapter default.
func resolverMultipartLimit(resolver transport.BindingResolver) int {
	if p, ok := resolver.(transport.MultipartLimitProvider); ok {
		return p.MaxMultipartParts()
	}
	return 0
}

func isSingletonField(name string) bool {
	switch name {
	case "timestamp", "token", "signature", "sender", "recipient", "Message-Id", "message-id", "message_id":
		return true
	}
	return false
}

// setField records a known Mailgun field, rejecting a repeated singleton so a
// later value cannot change the recipient or auth material after verification.
func setField(out *InboundForm, name, value string, seen map[string]bool) error {
	value = strings.TrimSpace(value)
	switch name {
	case "timestamp", "token", "signature", "sender", "recipient", "Message-Id", "message-id", "message_id":
		if seen[name] {
			return fmt.Errorf("duplicate field %s", name)
		}
		seen[name] = true
	}
	switch name {
	case "timestamp":
		out.Timestamp = value
	case "token":
		out.Token = value
	case "signature":
		out.Signature = value
	case "sender":
		out.Sender = value
	case "recipient":
		out.Recipient = value
	case "Message-Id", "message-id", "message_id":
		out.ProviderMessageID = value
	}
	return nil
}

// signatureMaxAge bounds how old a Mailgun timestamp may be. Mailgun warns that
// webhook processing can be delayed well beyond 15 minutes, and token-based
// deduplication already prevents replays, so a generous window avoids dropping
// legitimately delayed deliveries.
const signatureMaxAge = 24 * time.Hour

func VerifySignature(signingKey, timestamp, token, signature string) bool {
	if signingKey == "" || timestamp == "" || token == "" || signature == "" {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	delta := time.Since(time.Unix(ts, 0))
	if delta < -signatureMaxAge || delta > signatureMaxAge {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte(timestamp + token))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(strings.TrimSpace(signature))
	return err == nil && hmac.Equal(want, got)
}
