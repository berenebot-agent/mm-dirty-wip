package remotemx

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mailmoose/internal/transport"
)

// Provider is the registered receiving provider name for an account-owned
// Remote MX receiver: a standalone Dial MX receiver the account runs itself,
// reached in single mode (bearer key) rather than shared mode (DNS proof).
const Provider = "remotemx"

// Transport is the inbound provider adapter for Remote MX. It has no webhook
// face: the core dials the account's receiver outbound, so Receive is never a
// valid dispatch target and no ConfigFields are offered (the receiver is
// configured once per account through /v1/admin/account/mx and selected per
// domain by choosing this provider).
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return Provider }

func (Transport) Description() string {
	// The short label shown in the domain row and provider box.
	return "Remote MX"
}

// SelectLabel is the longer provider-picker label (transport.SelectLabelProvider).
// It makes the distinction from the installation Direct MX receiver explicit.
func (Transport) SelectLabel() string { return "Remote MailMoose MX (your own receiver)" }

func (Transport) ConfigFields() []transport.ConfigField { return nil }

var ErrWebhookUnsupported = errors.New("remotemx provider does not accept webhook dispatch")

func (Transport) Receive(context.Context, *http.Request, transport.BindingResolver, string, int64) (transport.InboundMessage, transport.InboundBinding, error) {
	return transport.InboundMessage{}, transport.InboundBinding{}, ErrWebhookUnsupported
}
