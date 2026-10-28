package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/one"
)

// remoteRoleFolderPullLimit bounds how many newest messages one Gmail
// folder pull reads, so a label-scoped list can never balloon into an unbounded
// request. A folder pull only needs the just-appended item, so a single bounded
// page is always enough; anything older is handled by the periodic sync.
const remoteRoleFolderPullLimit = 100

// ScheduleRoleFolderRefresh schedules an immediate, folder-scoped pull of one
// folder role into a standalone inbox's cached remote index. It is called right
// after a confirmed remote write (a RemoteDraft handoff placed in Drafts, a sent
// copy placed in Sent) so the new item is visible to reads without waiting for
// the next full-sync pass.
//
// The work runs on the same bounded, process-owned pool as the whole-inbox
// refresh (refreshSlots, refreshContext), coalesced per inbox+role: a burst of
// writes to one folder collapses into a single pull, and Stop drains it. It never
// blocks the caller.
func (m *RemoteMailboxService) ScheduleRoleFolderRefresh(accountID, inboxID, role string) {
	role = strings.TrimSpace(role)
	if accountID == "" || inboxID == "" || role == "" {
		return
	}
	key := accountID + ":" + inboxID + ":" + role
	m.refreshMu.Lock()
	if m.refreshContext.Err() != nil || m.roleRefreshing[key] {
		m.refreshMu.Unlock()
		return
	}
	m.roleRefreshing[key] = true
	m.refreshWG.Add(1)
	m.refreshMu.Unlock()
	go func() {
		defer m.refreshWG.Done()
		defer func() {
			m.refreshMu.Lock()
			delete(m.roleRefreshing, key)
			m.refreshMu.Unlock()
		}()
		select {
		case m.refreshSlots <- struct{}{}:
			defer func() { <-m.refreshSlots }()
		case <-m.refreshContext.Done():
			return
		}
		ctx, cancel := context.WithTimeout(m.refreshContext, time.Minute)
		defer cancel()
		if err := m.PullRoleFolder(one.Background(ctx), accountID, inboxID, role); err != nil {
			m.Service.Log.Debug("remote role folder refresh", "inbox_id", inboxID, "role", role, "error", err)
		}
	}()
}

// PullRoleFolder pulls one folder role's live contents into the cached remote
// index. It is the immediate, folder-scoped counterpart to ReconcileRemote: it
// refreshes only the folder a just-completed remote write targeted, so the new
// item is visible without a full pass. It never stores a body, and a missing role
// mapping or an unconfigured inbox is a no-op. An error is returned rather than
// swallowed so a caller can log it; the write that requested the pull has already
// succeeded and is never affected.
func (m *RemoteMailboxService) PullRoleFolder(ctx context.Context, accountID, inboxID, role string) error {
	switch role {
	case model.FolderRoleDrafts, model.FolderRoleSent:
	default:
		return nil
	}
	inbox, err := m.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return nil
	}
	if m.IsMicrosoft(ctx, accountID, inboxID) {
		return m.pullRoleFolderMicrosoft(ctx, inbox, role)
	}
	if m.IsGoogle(ctx, accountID, inboxID) {
		return m.pullRoleFolderGoogle(ctx, inbox, role)
	}
	return m.pullRoleFolderIMAP(ctx, inbox, role)
}

// pullRoleFolderIMAP reconciles one IMAP role folder: it resolves the folder's
// live UIDVALIDITY and runs the same bounded index pass the deep reconcile uses,
// for that folder only.
func (m *RemoteMailboxService) pullRoleFolderIMAP(ctx context.Context, inbox model.Inbox, role string) error {
	folder, ferr := m.Service.Store.GetSystemFolder(ctx, inbox.AccountID, inbox.ID, role)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return nil
	}
	if !m.folderInScope(inbox, folder.Path) {
		return nil
	}
	sess, _, err := m.open(ctx, inbox.AccountID, inbox.ID)
	if err != nil {
		return err
	}
	defer sess.Close()
	validity, verr := sess.EnsureFolderExists(ctx, folder.Path)
	if verr != nil {
		return normalizeRemoteError(verr)
	}
	if _, err := m.indexRemoteFolder(ctx, sess, inbox.AccountID, inbox.ID, folder.Path, validity); err != nil {
		return err
	}
	return nil
}

// pullRoleFolderMicrosoft applies one delta page for a Microsoft role folder,
// reusing the same folder-delta path the deep reconcile runs for every folder.
func (m *RemoteMailboxService) pullRoleFolderMicrosoft(ctx context.Context, inbox model.Inbox, role string) error {
	f, err := m.Service.Store.RemoteFolderByRole(ctx, inbox.AccountID, inbox.ID, role)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return mapStoreError(err)
	}
	t, c, err := m.microsoftAccess(ctx, inbox.AccountID, inbox.ID)
	if err != nil {
		return err
	}
	_, _, _, err = m.syncMicrosoftFolderDelta(ctx, t, c, inbox.AccountID, inbox.ID, f)
	return err
}

// pullRoleFolderGoogle lists the newest bounded page of a Gmail system label
// (DRAFT or SENT) and upserts each message's metadata, reusing the same cache
// path the history reconcile uses. A missing message is skipped; a metadata quota
// cap stops the pull without failing it.
func (m *RemoteMailboxService) pullRoleFolderGoogle(ctx context.Context, inbox model.Inbox, role string) error {
	label := ""
	switch role {
	case model.FolderRoleDrafts:
		label = "DRAFT"
	case model.FolderRoleSent:
		label = "SENT"
	default:
		return nil
	}
	client, t, err := m.GoogleMailboxClient(ctx, inbox.AccountID, inbox.ID)
	if err != nil {
		return err
	}
	page, err := client.List(ctx, t, "", label, "", remoteRoleFolderPullLimit)
	if err != nil {
		return err
	}
	for _, ref := range page.Messages {
		g, gerr := client.Get(ctx, t, ref.ID, "metadata")
		if gerr != nil {
			var mb *model.MailboxError
			if errors.As(gerr, &mb) && mb.Kind == model.ErrKindNotFound {
				continue
			}
			return gerr
		}
		if _, cerr := m.cacheGoogle(ctx, inbox.AccountID, inbox.ID, g); errors.Is(cerr, store.ErrRemoteMetadataQuota) {
			return nil
		} else if cerr != nil {
			return cerr
		}
	}
	return nil
}
