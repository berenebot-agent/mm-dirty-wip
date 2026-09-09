// Package transport defines the provider-neutral inbound webhook boundary.
//
// Each inbound provider (Mailgun, Cloudflare Email Routing via Worker, ...)
// implements InboundTransport.Receive: it authenticates the webhook, stages
// the raw MIME to a bounded temp file, and returns both the normalized message
// and the explicit account/domain/credential binding it verified. The mailbox
// core downstream (recipient resolution, MIME parsing, CommitInbound, event
// publishing) is transport-agnostic.
package transport

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrUnknownProvider     = errors.New("unknown inbound provider")
	ErrInboundUnauthorized = errors.New("inbound webhook unauthorized")
)

// InboundMessage is a parsed provider webhook, normalized for the shared
// ingest pipeline. Raw MIME is staged at RawPath (0600 temp file, bounded by
// maxBytes) so large messages stream to disk instead of RAM.
//
// Provider auth material (e.g. Mailgun's timestamp/token/signature) stays
// internal to the adapter and is never exposed here.
type InboundMessage struct {
	Provider          string
	Recipient         string
	EnvelopeFrom      string
	RawPath           string
	Size              int64
	DeliveryID        string
	ProviderMessageID string
}

// InboundBinding is the account/domain/credential tuple a provider webhook
// authenticated against, plus the decrypted provider configuration. Recipient
// is the canonical original envelope recipient, which may differ from the
// inbox address when a catch-all is used.
type InboundBinding struct {
	AccountID    string
	DomainID     string
	CredentialID string
	Provider     string
	Recipient    string
	Config       map[string]any
}

// BindingResolver maps an envelope recipient to the domain and assigned
// receive connection for a provider. It is implemented by the service and
// handed to adapters so provider code never touches the store directly.
//
// Implementations return ErrInboundUnauthorized for unknown or unconfigured
// recipients so the HTTP layer can answer uniformly without leaking which
// domains exist.
type BindingResolver interface {
	ResolveInboundBinding(ctx context.Context, provider, recipient string) (InboundBinding, error)
}

// InboundTransport is implemented by every provider adapter. Receive owns
// authentication, because only the adapter knows when enough of its payload is
// available to verify. It must not read or persist MIME beyond staging until
// authentication succeeds, and it returns the binding it actually verified.
type InboundTransport interface {
	Name() string
	Description() string
	ConfigFields() []ConfigField
	Receive(ctx context.Context, r *http.Request, resolver BindingResolver, tmpPath string, maxBytes int64) (InboundMessage, InboundBinding, error)
}
