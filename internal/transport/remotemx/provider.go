package remotemx

import (
	"context"
	"errors"
	"net/http"

	"github.com/dellarb/mammothmail/internal/transport"
)

// Provider is the registered receiving provider name for an account-owned
// Remote MX receiver: a standalone Dial MX receiver the account runs itself,
// reached in single mode (bearer key) rather than shared mode (DNS proof).
const Provider = "remotemx"

// Transport is the inbound provider adapter for Remote MX. It has no webhook
// face: the core dials the account's receiver outbound, so Receive is never a
// valid dispatch target. The receiver itself is configured once per account
// through /v1/admin/account/mx; the only per-domain value is the core-side spam
// policy applied to the receiver's auth evidence.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return Provider }

func (Transport) Description() string {
	// The short label shown in the domain row and provider box.
	return "Remote MX"
}

// SelectLabel is the longer provider-picker label (transport.SelectLabelProvider).
// It makes the distinction from the installation Direct MX receiver explicit.
func (Transport) SelectLabel() string { return "Remote Mammoth Mail MX (your own receiver)" }

func (Transport) ConfigFields() []transport.ConfigField {
	// The receiver computes SPF/DKIM/DMARC evidence on its own host; the core
	// only decides what an unauthenticated message becomes, so the enforcement
	// policy is the one receiving setting that applies per domain here.
	return []transport.ConfigField{
		{Name: "enforcement", Label: "Receiver authentication enforcement", Type: "select", Default: "moderate", Options: []transport.ConfigOption{
			{Value: "moderate", Label: "Moderate (Spam on definitive DMARC failure, or SPF and DKIM both failed)"},
			{Value: "hard", Label: "Hard (Spam on any one of SPF, DKIM or definitive DMARC failure)"},
		}},
	}
}

var ErrWebhookUnsupported = errors.New("remotemx provider does not accept webhook dispatch")

func (Transport) Receive(context.Context, *http.Request, transport.BindingResolver, string, int64) (transport.InboundMessage, transport.InboundBinding, error) {
	return transport.InboundMessage{}, transport.InboundBinding{}, ErrWebhookUnsupported
}
