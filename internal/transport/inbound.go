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

	"github.com/dellarb/mailmoose/internal/mxwire"
)

var (
	ErrUnknownProvider     = errors.New("unknown inbound provider")
	ErrInboundUnauthorized = errors.New("inbound webhook unauthorized")
	// ErrInboundIgnored lets an adapter acknowledge a webhook that needs no
	// ingest (for example a provider event type other than inbound mail). The
	// HTTP layer answers 200 so the provider stops retrying; no message is
	// persisted and no provider content is fetched.
	ErrInboundIgnored = errors.New("inbound webhook ignored")
)

// InboundMessage is a parsed provider webhook, normalized for the shared
// ingest pipeline. Raw MIME is staged at RawPath (0600 temp file, bounded by
// maxBytes) so large messages stream to disk instead of RAM.
//
// Provider auth material (e.g. Mailgun's timestamp/token/signature) stays
// internal to the adapter and is never exposed here.
type InboundMessage struct {
	Provider string
	// Recipient is the canonical primary envelope recipient, used for the
	// authenticated binding and, for single-recipient providers, as the only
	// delivery target.
	Recipient string
	// Recipients lists every MailMoose-controlled recipient the provider
	// addressed. It is used by providers (Resend) whose events carry all
	// recipients and must fan a single delivery out to each distinct inbox.
	// When empty, Recipient is the sole target.
	Recipients        []string
	EnvelopeFrom      string
	RawPath           string
	Size              int64
	DeliveryID        string
	ProviderMessageID string
	// EnvelopeFingerprint, when set, is the versioned MX retry identity over
	// canonical envelope sender, canonical recipient and the SHA-256 of the
	// original MIME. It is recorded as a durable delivery receipt so a retry
	// (including one after the message row is deleted) deduplicates. Empty for
	// webhook providers, whose adapter supplies DeliveryID instead.
	EnvelopeFingerprint string
	// AuthResults is bounded, normalized authentication evidence supplied by an
	// authenticated adapter (the MX edge). The policy engine consumes only this;
	// incoming Authentication-Results/Received-SPF headers are never trusted as
	// a substitute. It is zero for webhook providers.
	AuthResults mxwire.AuthResults
	// TrustedAuth marks AuthResults as computed by an authenticated adapter.
	// Without it, the core must not apply authentication-based Spam policy.
	TrustedAuth bool
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
	// ConfigDomainID is the domain the config was stored under (an ancestor when
	// receiving is inherited); it is the AAD scope for the stored credential.
	ConfigDomainID string
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

// MultipartLimitProvider is optionally implemented by a BindingResolver to
// expose an operator-configured cap on multipart webhook parts. Adapters that
// parse multipart bodies consult it so MAX_MULTIPART_PARTS is honoured rather
// than a package constant; when the resolver does not implement it, the
// adapter's own default applies.
type MultipartLimitProvider interface {
	MaxMultipartParts() int
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

// IngestPathProvider is implemented by inbound adapters whose public webhook
// URL is fixed. The Admin UI uses it to show operators the exact URL to
// register with the provider, so provider-specific path knowledge stays in the
// adapter rather than the UI.
type IngestPathProvider interface {
	IngestPath() string
}

// SelectLabelProvider is implemented by inbound adapters whose provider <select>
// entry needs a longer, descriptive label than Description(). When absent the
// UI falls back to Description().
type SelectLabelProvider interface {
	SelectLabel() string
}

// InboundSelectLabel returns the provider-picker label for an inbound transport:
// its descriptive SelectLabel when it provides one, otherwise its Description.
func InboundSelectLabel(t InboundTransport) string {
	if sl, ok := t.(SelectLabelProvider); ok {
		if label := sl.SelectLabel(); label != "" {
			return label
		}
	}
	return t.Description()
}
