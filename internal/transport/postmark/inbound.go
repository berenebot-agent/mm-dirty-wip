// Package postmark adapts Postmark inbound webhooks to the generic inbound
// boundary.
//
// Postmark POSTs a JSON document describing the message and, when "Include raw
// email content in JSON payload" is enabled, the original MIME source under
// RawEmail. Postmark does not sign inbound webhooks, so the endpoint is protected
// with HTTP Basic authentication: MailMoose generates a per-domain username and
// password, stores them encrypted, and the operator embeds them in the webhook
// URL registered with Postmark.
//
// RawEmail may carry bytes that are not valid UTF-8 (binary message parts), so
// it is extracted from the JSON without conversion to a Go string: the raw JSON
// value is unescaped byte-for-byte to preserve the original MIME.
package postmark

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dellarb/mailmoose/internal/transport"
)

// Transport adapts Postmark inbound webhooks to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return "postmark" }
func (Transport) Description() string { return "Postmark" }
func (Transport) IngestPath() string  { return "/internal/ingest/postmark" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "username", Label: "Webhook username", Type: "password", Required: true, Secret: true, Generated: true, Placeholder: "Generated automatically"},
		{Name: "password", Label: "Webhook password", Type: "password", Required: true, Secret: true, Generated: true, Placeholder: "Generated automatically"},
	}
}

// maxWebhookBytes bounds the JSON webhook. The RawEmail field expands under
// JSON escaping, so the cap allows a full-size message plus escaping overhead.
const maxWebhookBytes = 4 * (30 << 20)

// payload is the subset of the Postmark inbound document the adapter consumes.
// RawEmail is kept as raw JSON so binary MIME bytes survive extraction.
type payload struct {
	From              string          `json:"From"`
	OriginalRecipient string          `json:"OriginalRecipient"`
	MessageID         string          `json:"MessageID"`
	RawEmail          json.RawMessage `json:"RawEmail"`
}

// Receive authenticates the Basic-auth credentials, stages the raw MIME to
// tmpPath and returns the binding it verified. The OriginalRecipient selects the
// domain and its generated credentials.
func (Transport) Receive(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (transport.InboundMessage, transport.InboundBinding, error) {
	var msg transport.InboundMessage
	var binding transport.InboundBinding

	media, _, _ := parseMediaType(r.Header.Get("Content-Type"))
	if media != "" && media != "application/json" {
		return msg, binding, fmt.Errorf("unsupported content type %s", media)
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxWebhookBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return msg, binding, fmt.Errorf("read webhook: %w", err)
	}
	if len(body) == 0 {
		return msg, binding, fmt.Errorf("empty webhook")
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return msg, binding, fmt.Errorf("invalid webhook json: %w", err)
	}
	recipient := canonicalRecipient(p.OriginalRecipient)
	if recipient == "" {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	binding, err = resolver.ResolveInboundBinding(ctx, "postmark", recipient)
	if err != nil {
		return msg, binding, err
	}
	if !validBasicAuth(r.Header.Get("Authorization"), binding.Config) {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	rawEmail := trimSpace(p.RawEmail)
	if len(rawEmail) == 0 || string(rawEmail) == `""` {
		return msg, binding, fmt.Errorf("raw email missing; enable \"Include raw email content in JSON payload\" in Postmark")
	}
	size, err := writeRawEmail(rawEmail, tmpPath, maxBytes)
	if err != nil {
		return msg, binding, err
	}
	delivery := strings.TrimSpace(p.MessageID)
	return transport.InboundMessage{
		Provider:          "postmark",
		Recipient:         binding.Recipient,
		EnvelopeFrom:      strings.TrimSpace(p.From),
		RawPath:           tmpPath,
		Size:              size,
		DeliveryID:        delivery,
		ProviderMessageID: delivery,
	}, binding, nil
}

// writeRawEmail unescapes the raw JSON string value and streams it to tmpPath,
// bounded by maxBytes. Bytes that are not valid UTF-8 are preserved.
func writeRawEmail(raw json.RawMessage, tmpPath string, maxBytes int64) (int64, error) {
	raw = trimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, fmt.Errorf("invalid raw email field")
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	n, err := unescapeJSONString(raw[1:len(raw)-1], &limitedWriter{w: bw, n: maxBytes + 1})
	if err == nil {
		err = bw.Flush()
	}
	closeErr := f.Close()
	if err != nil {
		return n, err
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n > maxBytes {
		return n, fmt.Errorf("message too large")
	}
	if n == 0 {
		return n, fmt.Errorf("empty message")
	}
	return n, nil
}

// limitedWriter writes at most n bytes and then reports an error so the caller
// can distinguish "exactly at the cap" from "over the cap".
type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("message too large")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	written, err := l.w.Write(p)
	l.n -= int64(written)
	return written, err
}

// unescapeJSONString decodes a JSON string body (without the surrounding
// quotes) and writes the decoded bytes. Standard escapes are decoded; a raw
// byte that is not part of an escape is copied verbatim, which preserves
// non-UTF-8 MIME content.
func unescapeJSONString(src []byte, w io.Writer) (int64, error) {
	var n int64
	writeByte := func(b byte) error {
		if err := writeFull(w, []byte{b}); err != nil {
			return err
		}
		n++
		return nil
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c != '\\' {
			if err := writeByte(c); err != nil {
				return n, err
			}
			continue
		}
		i++
		if i >= len(src) {
			return n, fmt.Errorf("invalid raw email escape")
		}
		switch src[i] {
		case '"', '\\', '/':
			if err := writeByte(src[i]); err != nil {
				return n, err
			}
		case 'b':
			if err := writeByte('\b'); err != nil {
				return n, err
			}
		case 'f':
			if err := writeByte('\f'); err != nil {
				return n, err
			}
		case 'n':
			if err := writeByte('\n'); err != nil {
				return n, err
			}
		case 'r':
			if err := writeByte('\r'); err != nil {
				return n, err
			}
		case 't':
			if err := writeByte('\t'); err != nil {
				return n, err
			}
		case 'u':
			r, adv, err := decodeUnicodeEscape(src[i+1:])
			if err != nil {
				return n, err
			}
			i += adv
			buf := make([]byte, 4)
			m := utf8.EncodeRune(buf, r)
			if err := writeFull(w, buf[:m]); err != nil {
				return n, err
			}
			n += int64(m)
		default:
			return n, fmt.Errorf("invalid raw email escape")
		}
	}
	return n, nil
}

// decodeUnicodeEscape reads the four hex digits following a \u escape (src
// starts just after the 'u') plus an optional low surrogate, returning the rune
// and how many bytes were consumed beyond the four hex digits.
func decodeUnicodeEscape(src []byte) (rune, int, error) {
	r1, err := hex4(src)
	if err != nil {
		return 0, 0, err
	}
	consumed := 4
	if utf16.IsSurrogate(rune(r1)) && len(src) >= 6 && src[4] == '\\' && src[5] == 'u' {
		r2, err := hex4(src[6:])
		if err != nil {
			return 0, 0, err
		}
		if combined := utf16.DecodeRune(rune(r1), rune(r2)); combined != utf8.RuneError {
			return combined, 10, nil
		}
	}
	return rune(r1), consumed, nil
}

func hex4(b []byte) (int, error) {
	if len(b) < 4 {
		return 0, fmt.Errorf("invalid unicode escape")
	}
	var v int
	for _, c := range b[:4] {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("invalid unicode escape")
		}
		v = v<<4 | d
	}
	return v, nil
}

// writeFull writes all of p to w.
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// validBasicAuth compares the Authorization: Basic credentials to the domain's
// generated username/password in constant time. An empty configured value never
// authenticates.
func validBasicAuth(header string, cfg map[string]any) bool {
	user, _ := cfg["username"].(string)
	pass, _ := cfg["password"].(string)
	if strings.TrimSpace(user) == "" || strings.TrimSpace(pass) == "" {
		return false
	}
	const prefix = "Basic "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return false
	}
	gotUser, gotPass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1 &&
		subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
}

// parseMediaType returns the lowercased media type of a Content-Type header,
// ignoring parameters.
func parseMediaType(ct string) (string, string, error) {
	ct = strings.TrimSpace(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		return strings.ToLower(strings.TrimSpace(ct[:i])), strings.TrimSpace(ct[i+1:]), nil
	}
	return strings.ToLower(ct), "", nil
}

func trimSpace(b []byte) []byte {
	return bytes.TrimSpace(b)
}

// canonicalRecipient strips any display name and lowercases the address.
func canonicalRecipient(v string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(strings.TrimSpace(v))
}
