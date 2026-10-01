// Package mx registers the "mx" receiving provider. Unlike the webhook
// providers, MX delivery does not arrive as a provider HTTP request that this
// adapter parses: the unified Go receiver (dialmx/cmd/receiver) terminates
// SMTP and calls the explicit authenticated MX service entry point. This
// adapter exists so the receiving-provider editor can offer "mx" with its
// setup guidance and so the provider registry is complete.
//
// It deliberately does NOT implement transport.InboundTransport.Receive as a
// usable webhook: provider webhook dispatch for "mx" is rejected, and the core
// must never call Service.IngestInbound("mx", ...) because that path always
// invokes Receive. MX ingest uses Service.IngestMX instead.
package mx

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mailmoose/internal/transport"
)

// Provider is the registered name and receiving provider identifier.
const Provider = "mx"

// Transport is the registry stub for the MX provider.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return Provider }
func (Transport) Description() string { return "MX Direct SMTP to MailMoose on Port 25" }

// ConfigFields exposes the per-domain auth enforcement mode. There is no
// secret: MX is enabled by the operator edge configuration, and credentials are
// operator-managed, never tenant domain-editor values. The selector offers only
// the two shipped modes; there is no inactive ARC toggle or auth-delete action.
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "enforcement", Label: "Authentication enforcement", Type: "select", Default: "moderate", Options: []transport.ConfigOption{
			{Value: "moderate", Label: "Moderate (Spam on definitive DMARC failure, or SPF and DKIM both failed)"},
			{Value: "hard", Label: "Hard (Spam on any one of SPF, DKIM or definitive DMARC failure)"},
		}},
	}
}

// ErrWebhookUnsupported is returned when an ordinary webhook request reaches
// the MX provider. The MX edge uses the signed /internal/mx endpoints.
var ErrWebhookUnsupported = errors.New("mx provider does not accept webhook dispatch")

// Receive always rejects. MX ingest goes through the explicit authenticated
// service entry point, never through provider webhook dispatch.
func (Transport) Receive(context.Context, *http.Request, transport.BindingResolver, string, int64) (transport.InboundMessage, transport.InboundBinding, error) {
	return transport.InboundMessage{}, transport.InboundBinding{}, ErrWebhookUnsupported
}
