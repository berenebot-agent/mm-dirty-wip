package store

import (
	"context"

	"github.com/dellarb/mailmoose/internal/model"
)

// MailboxCapabilities reports the declared mailbox surface without remote I/O.
// IMAP retains its pre-handshake defaults; provider bindings refine those
// defaults using local connection and push-subscription state.
func (s *Store) MailboxCapabilities(ctx context.Context, inbox model.Inbox) model.Capabilities {
	if inbox.Kind != model.InboxKindStandalone {
		return model.DomainCapabilities()
	}
	caps := model.StandaloneCapabilities()
	caps.Outbound = inbox.Remote != nil && inbox.Remote.SMTP != nil
	if s.IsMicrosoft(ctx, inbox.AccountID, inbox.ID) {
		caps.Labels, caps.CreateLabels = true, true
		caps.Outbound = true
		caps.Realtime = s.MicrosoftPushActive(ctx, inbox.AccountID, inbox.ID)
	}
	if s.IsGoogle(ctx, inbox.AccountID, inbox.ID) {
		caps.CreateFolders, caps.RenameFolders, caps.DeleteFolders = false, false, false
		caps.Labels, caps.CreateLabels, caps.RenameLabels, caps.DeleteLabels = true, true, true, true
		caps.HierarchicalFolders = false
		caps.Outbound = true
		caps.Realtime = false
		if s.IsOne(ctx, inbox.AccountID, inbox.ID) {
			caps.Outbound = inbox.RemoteConfigured
		}
	}
	return caps
}
