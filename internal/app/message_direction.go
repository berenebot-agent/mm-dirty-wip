package app

import (
	"context"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// remoteView adds the same cached direction evidence to every remote surface,
// including reply sources. No live provider call or sender-address guess occurs.
func (m *RemoteMailboxService) remoteView(ctx context.Context, rec store.RemoteMessage) RemoteMessageView {
	v := remoteView(rec)
	var sent, inbox bool
	providerID, folder := "", rec.FolderPath
	if m.IsGoogle(ctx, rec.AccountID, rec.InboxID) {
		id, _, labels, err := m.Service.Store.GoogleMessage(ctx, rec.AccountID, rec.InboxID, rec.ID)
		if err == nil {
			providerID = id
			sent = containsFoldApp(labels, "SENT")
			inbox = containsFoldApp(labels, "INBOX")
		}
	} else {
		if m.IsMicrosoft(ctx, rec.AccountID, rec.InboxID) {
			providerID, _ = m.Service.Store.MicrosoftMessage(ctx, rec.AccountID, rec.InboxID, rec.ID)
			folder, _ = m.Service.Store.MicrosoftMessageFolder(ctx, rec.AccountID, rec.InboxID, rec.ID)
		}
		if f, err := m.Service.Store.GetFolderByPath(ctx, rec.AccountID, rec.InboxID, folder); err == nil {
			sent = f.Role == model.FolderRoleSent
			inbox = f.Role == model.FolderRoleInbox
		}
	}
	recorded := m.Service.Store.RecordedRemoteDirection(ctx, rec, providerID)
	v.Direction = model.MessageDirection(recorded, sent, inbox, rec.Draft)
	return v
}
