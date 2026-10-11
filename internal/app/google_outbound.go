package app

import (
	"bytes"
	"context"
	"fmt"
	"github.com/dellarb/mailmoose/internal/transport/gmail"

	"github.com/dellarb/mailmoose/internal/transport"
)

// Google outbound uses the process-owned mailbox service so token refresh is
// shared with reads; tokens are never embedded in queued sending configuration.
type googleOutbound struct{}

func (g googleOutbound) Name() string        { return "gmail" }
func (g googleOutbound) Description() string { return "Connected Google inbox" }
func (g googleOutbound) PreferRawMIME() bool { return true }
func (g googleOutbound) Send(ctx context.Context, cfg map[string]any, msg transport.OutboundMessage) (transport.OutboundResult, error) {
	t, _ := cfg["access_token"].(string)
	if t == "" {
		return transport.OutboundResult{}, fmt.Errorf("Google sending binding unavailable")
	}
	res, e := gmail.NewClient(nil).SendRaw(ctx, t, bytes.NewReader(msg.RawMIME), "")
	return transport.OutboundResult{ProviderMessageID: res.ID}, e
}

func init() { transport.RegisterOutbound(googleOutbound{}) }
