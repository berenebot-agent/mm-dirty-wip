package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
)

// advanceEmptyTrash does bounded coordination work on the ordinary action
// worker. It never purges while discovering; children own destructive calls and
// therefore retain the queue's existing restart/ambiguity rules.
func (w *ActionWorker) advanceEmptyTrash(ctx context.Context, a store.MessageAction) (string, string) {
	p, err := w.svc.Store.EmptyTrashProgress(ctx, a.ID)
	if err != nil {
		return "failed", "Could not read Empty Trash progress"
	}
	if p.Phase == "discovering" {
		err = w.discoverTrash(ctx, a, p)
		if err != nil {
			return "failed", publicTrashError(err)
		}
		return "pending", "Finding messages in Trash…"
	}
	if _, err = w.svc.Store.QueueTrashBatch(ctx, a); err != nil {
		return "failed", "Could not queue the next Empty Trash batch"
	}
	p, err = w.svc.Store.EmptyTrashProgress(ctx, a.ID)
	if err != nil {
		return "failed", "Could not read Empty Trash progress"
	}
	progress := fmt.Sprintf("Empty Trash: %d of %d purged", p.Purged, p.Total)
	if p.Purged+p.Failed+p.Ambiguous < p.Total {
		return "pending", progress
	}
	if p.Ambiguous > 0 {
		return "ambiguous", progress + "; some remote outcomes are uncertain—verify before retrying"
	}
	if p.Failed > 0 {
		return "failed", progress + "; some messages could not be purged"
	}
	return "done", progress
}

func publicTrashError(err error) string {
	var mb *model.MailboxError
	if errors.As(err, &mb) && mb.Message != "" {
		return mb.Message
	}
	return "Empty Trash could not finish discovering messages; no purge was started"
}

func (w *ActionWorker) discoverTrash(ctx context.Context, a store.MessageAction, p store.EmptyTrashProgress) error {
	principal := model.Principal{AccountID: a.AccountID, Admin: true}
	box, err := w.svc.Store.GetInbox(ctx, principal, a.InboxID)
	if err != nil {
		return err
	}
	if box.Kind != model.InboxKindStandalone {
		return w.svc.Store.SnapshotLocalTrash(ctx, a, true)
	}
	// Preserve the existing Google capability boundary: the connected OAuth grant
	// does not enable permanent deletion. Never claim a cached-only success.
	if w.remote.IsGoogle(ctx, a.AccountID, a.InboxID) {
		return model.NewMailboxError(model.ErrKindUnsupported, "Permanent deletion is not enabled for Google inboxes; use Gmail", false, nil)
	}
	if p.Folder == "" {
		if err := w.svc.Store.SnapshotLocalTrash(ctx, a, false); err != nil {
			return err
		}
		folder, e := w.svc.Store.GetSystemFolder(ctx, a.AccountID, a.InboxID, model.FolderRoleTrash)
		if e != nil {
			return model.NewMailboxError(model.ErrKindConflict, "No Trash folder mapped", false, e)
		}
		p.Folder = folder.Path
	}
	var targets []store.TrashTarget
	if w.remote.IsMicrosoft(ctx, a.AccountID, a.InboxID) {
		token, c, e := w.remote.microsoftAccess(ctx, a.AccountID, a.InboxID)
		if e != nil {
			return e
		}
		page, e := w.remote.Microsoft.Messages(ctx, token, c.Mailbox, p.Folder, "", p.Cursor, 100)
		if e != nil {
			return e
		}
		for _, msg := range page.Messages {
			if msg.ID == "" || msg.ParentFolderID != p.Folder {
				return model.NewMailboxError(model.ErrKindUnavailable, "Microsoft returned an invalid Trash page", false, nil)
			}
			targets = append(targets, store.TrashTarget{Kind: "microsoft", ID: msg.ID, Folder: p.Folder})
		}
		if page.NextLink != "" && page.NextLink == p.Cursor {
			return model.NewMailboxError(model.ErrKindUnavailable, "Microsoft Trash pagination did not advance", false, nil)
		}
		return w.svc.Store.SaveTrashDiscovery(ctx, a.ID, p.Folder, page.NextLink, 0, targets, page.NextLink == "")
	}
	if !w.remote.folderInScope(box, p.Folder) {
		return model.NewMailboxError(model.ErrKindConflict, "Trash is outside this inbox's scope", false, nil)
	}
	sess, _, err := w.remote.open(ctx, a.AccountID, a.InboxID)
	if err != nil {
		return err
	}
	defer sess.Close()
	status, err := sess.Status(ctx, p.Folder)
	if err != nil {
		return normalizeRemoteError(err)
	}
	if status.UIDValidity == 0 || (p.UIDValidity != 0 && p.UIDValidity != status.UIDValidity) {
		return model.NewMailboxError(model.ErrKindConflict, "Trash identity changed during discovery; start Empty Trash again", false, nil)
	}
	var before uint64
	if p.Cursor != "" {
		before, err = strconv.ParseUint(p.Cursor, 10, 32)
		if err != nil {
			return err
		}
	}
	result, err := sess.Search(ctx, p.Folder, imap.SearchQuery{Limit: 500, NewestFirst: true, BeforeUID: uint32(before)})
	if err != nil {
		return normalizeRemoteError(err)
	}
	if result.Completeness == imap.CompletenessUnknown {
		return model.NewMailboxError(model.ErrKindUnavailable, "Trash enumeration was incomplete", false, nil)
	}
	for _, uid := range result.UIDs {
		if uid == 0 || (before != 0 && uint64(uid) >= before) {
			return model.NewMailboxError(model.ErrKindUnavailable, "Trash pagination did not advance", false, nil)
		}
		id := strconv.FormatUint(uint64(uid), 10)
		targets = append(targets, store.TrashTarget{Kind: "imap", ID: id, Folder: p.Folder, UID: uid, UIDValidity: status.UIDValidity})
	}
	cursor := ""
	if result.NextCursor != 0 {
		cursor = strconv.FormatUint(uint64(result.NextCursor), 10)
	}
	if result.Completeness == imap.CompletenessPartial && cursor == "" {
		return model.NewMailboxError(model.ErrKindUnavailable, "Trash enumeration was incomplete", false, nil)
	}
	return w.svc.Store.SaveTrashDiscovery(ctx, a.ID, p.Folder, cursor, status.UIDValidity, targets, cursor == "")
}

func (w *ActionWorker) purgeTrashTarget(ctx context.Context, a store.MessageAction) error {
	target := a.Input.TrashTarget
	if target.Kind == "local" {
		// Use the ordinary purge implementation, including its transactional
		// still-in-Trash check and MIME cleanup. A missing message is already gone.
		copy := a
		copy.Input.TrashTarget = nil
		copy.MessageID = target.ID
		err := w.apply(ctx, copy)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	// A mapping change must not redirect an existing destructive operation. The
	// seeded system folder is the role's address for every provider.
	folder, err := w.svc.Store.GetSystemFolder(ctx, a.AccountID, a.InboxID, model.FolderRoleTrash)
	if err != nil || folder.Path != target.Folder {
		return model.NewMailboxError(model.ErrKindConflict, "Trash mapping changed; verify this operation", false, err)
	}
	if target.Kind == "microsoft" {
		token, c, e := w.remote.microsoftAccess(ctx, a.AccountID, a.InboxID)
		if e != nil {
			return e
		}
		msg, e := w.remote.Microsoft.Message(ctx, token, c.Mailbox, target.ID)
		if mailboxNotFound(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if msg.ParentFolderID != target.Folder {
			return model.NewMailboxError(model.ErrKindConflict, "Message is no longer in Trash", false, nil)
		}
		if e = w.remote.Microsoft.Delete(ctx, token, c.Mailbox, target.ID); e != nil && !mailboxNotFound(e) {
			return e
		}
		if id, e := w.svc.Store.MicrosoftLocalID(ctx, a.AccountID, a.InboxID, target.ID); e == nil {
			_ = w.svc.Store.DeleteRemoteMessage(ctx, a.AccountID, a.InboxID, id)
		}
		return nil
	}
	if target.Kind != "imap" || target.UID == 0 || target.UIDValidity == 0 {
		return store.ErrConflict
	}
	box, err := w.svc.Store.GetInbox(ctx, model.Principal{AccountID: a.AccountID, Admin: true}, a.InboxID)
	if err != nil {
		return err
	}
	if !w.remote.folderInScope(box, target.Folder) {
		return model.NewMailboxError(model.ErrKindConflict, "Trash is outside this inbox's scope", false, nil)
	}
	sess, _, err := w.remote.open(ctx, a.AccountID, a.InboxID)
	if err != nil {
		return err
	}
	defer sess.Close()
	// The adapter checks UIDVALIDITY and uses UID-targeted expunge. Never fall
	// back to Message-ID relocation: a restored message must not be followed.
	err = sess.DeleteMessage(ctx, imap.Locator{FolderPath: target.Folder, UID: target.UID, UIDValidity: target.UIDValidity})
	if errors.Is(err, imap.ErrNotFound) {
		return nil
	}
	if err != nil {
		return normalizeRemoteError(err)
	}
	if rec, e := w.svc.Store.GetRemoteMessageByUID(ctx, a.AccountID, a.InboxID, target.Folder, target.UIDValidity, target.UID); e == nil {
		_ = w.svc.Store.DeleteRemoteMessage(ctx, a.AccountID, a.InboxID, rec.ID)
	}
	return nil
}

func mailboxNotFound(err error) bool {
	var mb *model.MailboxError
	return errors.As(err, &mb) && mb.Kind == model.ErrKindNotFound
}

// Keep coordination turns out of a hot loop and let child/user actions run.
const emptyTrashTurnDelay = 100 * time.Millisecond
