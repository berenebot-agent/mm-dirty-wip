package mxdial

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mailmoose/internal/transport"
)

const (
	Provider = "dialmx"
	// ServiceAntler and ServiceCustom select between the Antler MX hosted relay
	// (endpoints resolved from the live manifest and snapshotted at save) and
	// operator-supplied receiver URLs.
	ServiceAntler = "antler"
	ServiceCustom = "custom"
)

type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return Provider }
func (Transport) Description() string {
	// The short label. The receiving provider selector shows the long Antler MX
	// label via selectLabel in the HTTP layer.
	return "Antler MX"
}

// SelectLabel is the longer provider-picker label (transport.SelectLabelProvider).
// The Dial MX provider offers the zero-config Antler MX shared relay, so its
// selector entry states that plainly while the short Description stays "Antler
// MX" in the domain row and provider box.
func (Transport) SelectLabel() string {
	return "Antler MX (Free SMTP Relay - no port forwards required)"
}
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "service", Label: "Service", Type: "select", Default: ServiceAntler, Options: []transport.ConfigOption{
			{Value: ServiceAntler, Label: "Antler MX (Free SMTP Relay - no port forwards required)"},
			{Value: ServiceCustom, Label: "Custom receiver URLs"},
		}},
		{Name: "contact_email", Label: "Contact email (Antler MX)", Type: "text", Placeholder: "you@example.com"},
		{Name: "receiver_urls", Label: "Receiver URLs (custom service only, comma separated HTTPS base URLs)", Type: "text"},
		{Name: "enforcement", Label: "Authentication enforcement", Type: "select", Default: "moderate", Options: []transport.ConfigOption{
			{Value: "moderate", Label: "Moderate"},
			{Value: "hard", Label: "Hard"},
		}},
	}
}

var ErrWebhookUnsupported = errors.New("dialmx provider does not accept webhook dispatch")

func (Transport) Receive(context.Context, *http.Request, transport.BindingResolver, string, int64) (transport.InboundMessage, transport.InboundBinding, error) {
	return transport.InboundMessage{}, transport.InboundBinding{}, ErrWebhookUnsupported
}
