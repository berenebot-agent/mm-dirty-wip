package mxdial

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mailmoose/internal/transport"
)

const Provider = "dialmx"

type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return Provider }
func (Transport) Description() string { return "Dial MX" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "receiver_urls", Label: "Receiver URLs (comma separated HTTPS base URLs)", Type: "text", Required: true},
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
