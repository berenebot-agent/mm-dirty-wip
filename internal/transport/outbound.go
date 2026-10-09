package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type OutboundMessage struct {
	FromName, FromAddress string
	To, CC, BCC           []string
	Subject, Text, HTML   string
	MessageID             string
	InReplyTo             string
	References            []string
	RawMIME               []byte
	Attachments           []OutboundAttachment
	// IdempotencyKey, when set, is a stable key for the logical send (the local
	// message id). Adapters whose provider supports idempotency keys pass it so
	// a retry after a lost success response does not deliver a duplicate. It is
	// empty when unknown; adapters without provider-side support ignore it.
	IdempotencyKey string
}

type OutboundAttachment struct {
	Filename, ContentType string
	Content               []byte
}

type OutboundResult struct{ ProviderMessageID string }

type OutboundTransport interface {
	Name() string
	Description() string
	Send(ctx context.Context, cfg map[string]any, m OutboundMessage) (OutboundResult, error)
}

// RawMIMEProvider is implemented by transports that build their request from
// the raw MIME (e.g. SMTP) and therefore do not need structured attachments
// reconstructed from it.
type RawMIMEProvider interface {
	PreferRawMIME() bool
}

type ConfigOption struct{ Value, Label string }

type ConfigField struct {
	Name, Label, Type    string
	Required, Secret     bool
	Generated            bool
	Placeholder, Default string
	Options              []ConfigOption
}

type ConfigSchemaProvider interface {
	ConfigFields() []ConfigField
}

// EnvelopeRecipientLimitProvider lets a transport reject messages whose
// envelope cannot be delivered atomically by one provider transaction.
type EnvelopeRecipientLimitProvider interface {
	MaxEnvelopeRecipients() int
}

func MaxEnvelopeRecipients(t OutboundTransport) int {
	if limited, ok := t.(EnvelopeRecipientLimitProvider); ok {
		return limited.MaxEnvelopeRecipients()
	}
	return 0
}

func DecodeOutboundConfig(in map[string]any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// PermanentError marks a provider error that will never succeed on retry (e.g.
// a 4xx client rejection). The outbox worker fails such messages immediately
// instead of retrying them with backoff.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is a permanent (non-retryable) provider error.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// providerErrorBodyLimit bounds how much of a provider's error response is
// retained in an error message. Provider bodies are persisted to the message's
// last_error and shown to mailbox readers, so the raw response is truncated and
// stripped of control characters rather than stored in full.
const providerErrorBodyLimit = 512

// ProviderError builds an error from a provider's non-2xx response. The body is
// truncated to a short, single-line snippet so a large or secret-bearing
// response cannot be persisted to the delivery log or surfaced verbatim.
func ProviderError(provider, status string, body []byte) error {
	return fmt.Errorf("%s returned %s: %s", provider, status, snippet(body))
}

// snippet returns at most providerErrorBodyLimit bytes of body as a single
// control-character-free line.
func snippet(body []byte) string {
	if len(body) > providerErrorBodyLimit {
		body = body[:providerErrorBodyLimit]
	}
	var b strings.Builder
	for _, c := range body {
		if c == '\r' || c == '\n' || c == '\t' {
			b.WriteByte(' ')
			continue
		}
		if c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}
