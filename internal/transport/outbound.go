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

// ProviderError builds a safe diagnostic from a provider's non-2xx response.
// Arbitrary response bodies and reason phrases are never retained.
func ProviderError(provider, status string, _ []byte) error {
	// Provider bodies can echo credentials or private infrastructure details.
	// Never persist arbitrary text, including an apparently harmless prefix.
	diagnostic := "provider rejected the request; check the provider dashboard"
	switch {
	case strings.HasPrefix(status, "401"), strings.HasPrefix(status, "403"):
		diagnostic = "check the connector credentials and sender permissions"
	case strings.HasPrefix(status, "429"):
		diagnostic = "provider rate limit reached; try again later"
	case strings.HasPrefix(status, "400"), strings.HasPrefix(status, "422"):
		diagnostic = "check recipients, sender verification and message requirements"
	case strings.HasPrefix(status, "5"):
		diagnostic = "provider is temporarily unavailable"
	}
	// Keep only the numeric status; even the HTTP reason phrase is untrusted.
	code := "unknown status"
	if len(status) >= 3 && status[0] >= '1' && status[0] <= '5' && status[1] >= '0' && status[1] <= '9' && status[2] >= '0' && status[2] <= '9' {
		code = status[:3]
	}
	return fmt.Errorf("%s returned HTTP %s: %s", provider, code, diagnostic)
}
