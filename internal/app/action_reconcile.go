package app

import (
	"context"
	"errors"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
)

// ambiguousCheckTimeout bounds the live provider check of one action.
const ambiguousCheckTimeout = 20 * time.Second

// recheckAmbiguous settles uncertain Empty Trash work from the provider's live
// state, so a lost response does not leave a person to verify it by hand.
//
// Only actions with durable exact evidence are reconsidered: a purge child
// carries its frozen folder, UID and UIDVALIDITY. The check never relocates by
// Message-ID and never treats a cache miss as evidence.
//
//   - The exact UID is absent in an unchanged generation: the purge took effect.
//   - The exact UID is still present: nothing was removed, so the UID-targeted
//     purge is safe to run again.
//   - Anything else (changed generation, unreachable provider, changed Trash
//     mapping, unsupported provider): nothing is decided and the check repeats
//     later. No destructive call is made.
//
// An Empty Trash parent is handed back to the coordinator only once none of its
// purge children is still uncertain, so its summary reflects the real outcome.
func (w *ActionWorker) recheckAmbiguous(ctx context.Context) {
	actions, err := w.svc.Store.ListAmbiguousForRecheck(ctx, time.Now(), recheckBatch)
	if err != nil {
		w.svc.Log.Error("ambiguous action recheck list", "error", err)
		return
	}
	for _, a := range actions {
		decided, state := w.reconcileAmbiguous(ctx, a)
		if !decided {
			if derr := w.svc.Store.DeferAmbiguousRecheck(ctx, a.ID, time.Now().Add(recheckDeferral)); derr != nil {
				w.svc.Log.Error("ambiguous action defer", "action_id", a.ID, "error", derr)
			}
			continue
		}
		changed, rerr := w.svc.Store.ResolveAmbiguousAction(ctx, a.ID, state)
		if rerr != nil {
			w.svc.Log.Error("ambiguous action resolve", "action_id", a.ID, "error", rerr)
			continue
		}
		if changed {
			w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
			if state == "pending" {
				w.Wake()
			}
		}
	}
}

// reconcileAmbiguous reports whether the provider settled the action and, if
// so, the state to move it to.
func (w *ActionWorker) reconcileAmbiguous(ctx context.Context, a store.MessageAction) (bool, string) {
	if a.Input.Kind == "empty_trash" {
		p, err := w.svc.Store.EmptyTrashProgress(ctx, a.ID)
		if err != nil || p.Ambiguous > 0 {
			return false, ""
		}
		return true, "pending"
	}
	target := a.Input.TrashTarget
	if target == nil || target.Kind != "imap" || target.UID == 0 || target.UIDValidity == 0 {
		return false, ""
	}
	folder, err := w.svc.Store.GetSystemFolder(ctx, a.AccountID, a.InboxID, model.FolderRoleTrash)
	if err != nil || folder.Path != target.Folder {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, ambiguousCheckTimeout)
	defer cancel()
	sess, _, err := w.remote.open(ctx, a.AccountID, a.InboxID)
	if err != nil {
		return false, ""
	}
	defer sess.Close()
	_, err = sess.FetchHeader(ctx, imap.Locator{FolderPath: target.Folder, UID: target.UID, UIDValidity: target.UIDValidity})
	switch {
	case err == nil:
		return true, "pending"
	case errors.Is(err, imap.ErrNotFound):
		if rec, e := w.svc.Store.GetRemoteMessageByUID(ctx, a.AccountID, a.InboxID, target.Folder, target.UIDValidity, target.UID); e == nil {
			_ = w.svc.Store.DeleteRemoteMessage(ctx, a.AccountID, a.InboxID, rec.ID)
		}
		return true, "done"
	}
	return false, ""
}
