package app

import (
	"bytes"
	"context"
	"fmt"

	"github.com/dellarb/mammothmail/internal/transport"
	"github.com/dellarb/mammothmail/internal/transport/microsoft"
)

type microsoftOutbound struct{}

func (microsoftOutbound) Name() string        { return "microsoft" }
func (microsoftOutbound) Description() string { return "Connected Microsoft inbox" }
func (microsoftOutbound) PreferRawMIME() bool { return true }
func (microsoftOutbound) Send(ctx context.Context, cfg map[string]any, msg transport.OutboundMessage) (transport.OutboundResult, error) {
	t, _ := cfg["access_token"].(string)
	mailbox, _ := cfg["mailbox"].(string)
	if t == "" || mailbox == "" {
		return transport.OutboundResult{}, fmt.Errorf("Microsoft sending binding unavailable")
	}
	c := microsoft.NewClient(nil)
	d, e := c.CreateDraft(ctx, t, mailbox, bytes.NewReader(msg.RawMIME))
	if e != nil {
		return transport.OutboundResult{}, e
	}
	// The transmitted MIME intentionally omits Bcc. Use the structured envelope
	// for every recipient class, then verify before submitting to Microsoft.
	if e = c.SetDraftRecipientsVerified(ctx, t, mailbox, d.ID, msg.To, msg.CC, msg.BCC); e != nil {
		return transport.OutboundResult{ProviderMessageID: d.ID}, &transport.AmbiguousError{Err: e}
	}
	if e = c.SendDraft(ctx, t, mailbox, d.ID); e != nil {
		// A remote draft now exists. Retrying this two-step operation would
		// create another copy, so require reconciliation even for a send reject.
		return transport.OutboundResult{ProviderMessageID: d.ID}, &transport.AmbiguousError{Err: e}
	}
	return transport.OutboundResult{ProviderMessageID: d.ID}, nil
}
func init() { transport.RegisterOutbound(microsoftOutbound{}) }
