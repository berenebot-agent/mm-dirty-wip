package mxdial

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mammothmail/internal/transport"
)

const (
	Provider = "dialmx"
	// ServiceHosted and ServiceCustom select between the Tusk MX hosted relay
	// (endpoints resolved from the live manifest and snapshotted at save) and
	// operator-supplied receiver URLs.
	ServiceHosted = "hosted"
	ServiceCustom = "custom"
	// ServiceAntler remains as an internal Go alias while callers are migrated.
	ServiceAntler = ServiceHosted
	// LegacyServiceAntler identifies encrypted configurations written before the rename.
	LegacyServiceAntler = "antler"
	// EnforcementModerate and EnforcementHard are the inbound authentication
	// enforcement policies. Moderate marks mail as spam when DMARC fails, or both
	// SPF and DKIM fail; hard marks it when any of SPF, DKIM or DMARC fails.
	EnforcementModerate = "moderate"
	EnforcementHard     = "hard"
)

// ValidEnforcement reports whether a string is an enforcement policy this
// provider accepts. It is the single source of allowed values, so the settings
// update path and the config schema cannot drift.
func ValidEnforcement(v string) bool {
	return v == EnforcementModerate || v == EnforcementHard
}

type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return Provider }
func (Transport) Description() string {
	// The short label. The receiving provider selector shows the long Tusk MX
	// label via selectLabel in the HTTP layer.
	return "Tusk MX"
}

// SelectLabel is the longer provider-picker label (transport.SelectLabelProvider).
// The Dial MX provider offers the zero-config Tusk MX shared relay, so its
// selector entry states that plainly while the short Description stays "Tusk
// MX" in the domain row and provider box.
func (Transport) SelectLabel() string {
	return "Tusk MX (Free SMTP Relay - no port forwards required)"
}
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "service", Label: "Service", Type: "select", Default: ServiceHosted, Options: []transport.ConfigOption{
			{Value: ServiceHosted, Label: "Tusk MX (Free SMTP Relay - no port forwards required)"},
			{Value: ServiceCustom, Label: "Custom receiver URLs"},
		}},
		{Name: "contact_email", Label: "Contact email — used for important service messages only, no marketing", Type: "text", Placeholder: "you@example.com"},
		{Name: "receiver_urls", Label: "Receiver URLs (custom service only, comma separated HTTPS base URLs)", Type: "text"},
		{Name: "enforcement", Label: "Receiver authentication enforcement (spam)", Type: "select", Default: "moderate", Options: []transport.ConfigOption{
			{Value: "moderate", Label: "Moderate (Spam on definitive DMARC failure, or SPF and DKIM both failed)"},
			{Value: "hard", Label: "Hard (Spam on any one of SPF, DKIM or definitive DMARC failure)"},
		}},
	}
}

var ErrWebhookUnsupported = errors.New("dialmx provider does not accept webhook dispatch")

func (Transport) Receive(context.Context, *http.Request, transport.BindingResolver, string, int64) (transport.InboundMessage, transport.InboundBinding, error) {
	return transport.InboundMessage{}, transport.InboundBinding{}, ErrWebhookUnsupported
}
