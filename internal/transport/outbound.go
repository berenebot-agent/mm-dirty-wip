package transport

import (
	"context"
	"encoding/json"
	"errors"
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

// SelfHostedOnlyProvider marks transports that must not be exposed by the
// hosted service. These transports generally require operator-owned network
// identity or reputation.
type SelfHostedOnlyProvider interface {
	SelfHostedOnly() bool
}

// EnvelopeRecipientLimitProvider lets a transport reject messages whose
// envelope cannot be delivered atomically by one provider transaction.
type EnvelopeRecipientLimitProvider interface {
	MaxEnvelopeRecipients() int
}

func OutboundAllowed(t OutboundTransport, hosted bool) bool {
	if t == nil {
		return false
	}
	if hosted {
		if restricted, ok := t.(SelfHostedOnlyProvider); ok && restricted.SelfHostedOnly() {
			return false
		}
	}
	return true
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
