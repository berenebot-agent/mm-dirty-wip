package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/safepath"
	"github.com/dellarb/mailmoose/internal/store"
)

// ActionWorker owns the shared local/remote mutation queue. HTTP requests only
// enqueue; waiting for completion never executes a second copy of an action.
type ActionWorker struct {
	svc    *Service
	remote *RemoteMailboxService
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	// Only the worker goroutine accesses the round-robin cursor.
	lastInbox string
}

// recheckInterval is how often failed actions are reconsidered. It is long
// enough to absorb a provider outage without hammering it, and short enough that
// a transient failure retries within minutes rather than waiting for a human.
// It is the single delay in the recheck cycle: a failed action's next_attempt_at
// is set to now+recheckInterval, and the rechecker selects rows that are due,
// so the wait between rechecks is exactly one interval.
const recheckInterval = 10 * time.Minute

// recheckBatch bounds how many failed actions one recheck pass examines, so a
// large error backlog is worked through steadily instead of in one burst.
const recheckBatch = 200

// recheckDeferral pushes an unresolvable failed action's next recheck out by one
// interval, so the pass does not retry it again immediately. It matches
// recheckInterval: a deferred action waits the same single interval as a freshly
// failed one.
const recheckDeferral = recheckInterval

func NewActionWorker(svc *Service, remote *RemoteMailboxService) *ActionWorker {
	return &ActionWorker{svc: svc, remote: remote, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *ActionWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *ActionWorker) Start() { go w.run() }
func (w *ActionWorker) Stop()  { close(w.stop); <-w.done }

func (w *ActionWorker) run() {
	defer close(w.done)
	_ = w.svc.Store.RecoverMessageActions(context.Background())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	recheck := time.NewTicker(recheckInterval)
	defer recheck.Stop()
	lastSweep := time.Time{}
	for {
		w.drain()
		if time.Since(lastSweep) > time.Hour {
			_ = w.svc.Store.SweepMessageActions(context.Background())
			lastSweep = time.Now()
		}
		select {
		case <-w.stop:
			return
		case <-w.wake:
		case <-tick.C:
		case <-recheck.C:
			w.RecheckFailed()
		}
	}
}

func (w *ActionWorker) drain() {
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a, ok, err := w.svc.Store.ClaimMessageActionAfterInbox(ctx, "actions", w.lastInbox)
		if err != nil || !ok {
			cancel()
			return
		}
		w.lastInbox = a.InboxID
		w.execute(ctx, a)
		cancel()
	}
}

func (w *ActionWorker) execute(ctx context.Context, a store.MessageAction) {
	state, reason := "done", ""
	defer func() {
		if recover() != nil {
			state, reason = "ambiguous", "Action interrupted; verify the message before retrying"
		}
		// For a failed action, next_attempt_at is the next recheck time: the
		// rechecker reconsiders it one interval from now. A retryable patch that
		// stays pending keeps the short per-attempt backoff instead, which the
		// claim query honours directly.
		next := time.Now().Add(time.Duration(a.Attempts) * time.Second)
		if a.Input.Kind == "empty_trash" {
			next = time.Now().Add(emptyTrashTurnDelay)
		}
		if state == "failed" {
			next = time.Now().Add(recheckInterval)
		}
		outcome, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.svc.Store.SettleMessageAction(outcome, a.ID, state, reason, next); err != nil {
			w.svc.Log.Error("action settlement", "action_id", a.ID, "error", err)
		}
		// An Empty Trash parent re-runs a coordination turn every
		// emptyTrashTurnDelay while its purge children are still in flight. Such
		// a turn only re-reads progress and changes nothing the queue shows, so
		// publishing here would flood listeners with events that each trigger a
		// full UI refresh. Real transitions (done, failed, ambiguous) and every
		// child settle still publish.
		if a.Input.Kind == "empty_trash" && state == "pending" {
			return
		}
		w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
	}()
	if a.Input.Kind == "empty_trash" {
		state, reason = w.advanceEmptyTrash(ctx, a)
		if state == "pending" || state == "done" {
			reason = ""
		}
		return
	}
	err := w.apply(ctx, a)
	if err == nil {
		return
	}
	state, reason = "failed", "Action could not be completed"
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		// Prefer the public message. When a provider error was normalised with no
		// public detail (an internal/unclassified remote failure), fall back to the
		// error's own text so the real cause reaches the action queue instead of the
		// opaque generic string. MailboxError.Error() already prefers Message and
		// otherwise returns the wrapped cause.
		switch {
		case mb.Message != "":
			reason = mb.Message
		default:
			if text := strings.TrimSpace(err.Error()); text != "" {
				reason = text
			}
		}
		if mb.Retryable && a.Input.Kind == "patch" && a.Input.Spam == nil && a.Attempts < 5 {
			state = "pending"
		}
		if mb.Retryable && (a.Input.Kind != "patch" || a.Input.Spam != nil) {
			state, reason = "ambiguous", "Remote outcome uncertain; verify the message before dismissing or submitting another action"
		}
		return
	}
	// A non-MailboxError failure is a store or validation error with no provider
	// detail. Give the common ones a reason an operator can act on rather than the
	// generic string, which otherwise hides a missing destination folder.
	switch {
	case errors.Is(err, store.ErrNotFound):
		reason = "the destination folder could not be found"
	case errors.Is(err, store.ErrConflict):
		reason = "the destination is not a valid move target"
	case errors.Is(err, store.ErrForbidden):
		reason = "not permitted"
	}
}

func (w *ActionWorker) apply(ctx context.Context, a store.MessageAction) error {
	if a.Input.TrashTarget != nil {
		return w.purgeTrashTarget(ctx, a)
	}
	p := model.Principal{AccountID: a.AccountID, Admin: true}
	box, err := w.svc.Store.GetInbox(ctx, p, a.InboxID)
	if err != nil {
		return err
	}
	// Locally stored outbound messages in a standalone inbox remain local.
	_, localErr := w.svc.Store.GetMessage(ctx, p, a.MessageID)
	remote := box.Kind == model.InboxKindStandalone && errors.Is(localErr, store.ErrNotFound)
	if localErr != nil && !remote {
		return localErr
	}
	in := a.Input
	if remote {
		// Resolve label membership when executing the queued action, not when
		// accepting it. Concurrent adds must not overwrite one another.
		if in.Kind == "label" {
			if in.LabelOp == "add" {
				_, err = w.remote.AddRemoteLabels(ctx, p, box.ID, a.MessageID, []string{in.Label})
			} else {
				_, err = w.remote.RemoveRemoteLabels(ctx, p, box.ID, a.MessageID, []string{in.Label})
			}
			return err
		}
		if in.Kind == "patch" {
			if in.Read != nil {
				if _, err = w.remote.SetRemoteRead(ctx, p, box.ID, a.MessageID, *in.Read); err != nil {
					return err
				}
			}
			if in.Labels != nil {
				if _, err = w.remote.SetRemoteLabels(ctx, p, box.ID, a.MessageID, *in.Labels); err != nil {
					return err
				}
			}
			if in.Spam == nil {
				return nil
			}
		}
		if in.Kind == "purge" {
			return w.remote.PurgeRemoteMessage(ctx, p, box.ID, a.MessageID)
		}
		dest := in.Destination
		role := ""
		switch in.Kind {
		case "delete":
			role = model.FolderRoleTrash
		case "restore":
			role = model.FolderRoleInbox
		}
		if in.Spam != nil {
			role = model.FolderRoleInbox
			if *in.Spam {
				role = model.FolderRoleSpam
			}
		}
		if role != "" {
			folders, e := w.svc.Store.ListFolders(ctx, p.AccountID, box.ID)
			if e != nil {
				return e
			}
			for _, f := range folders {
				if f.Role == role && (f.Origin == "remote" || f.RoleLocked) {
					dest = f.Path
					break
				}
			}
			if dest == "" {
				return fmt.Errorf("missing remote role folder")
			}
		}
		_, err = w.remote.MoveRemoteMessage(ctx, p, box.ID, a.MessageID, dest)
		return err
	}
	publish := func(ev *model.Event, e error) error {
		if e == nil && ev != nil {
			w.svc.Hub.Publish(*ev)
		}
		return e
	}
	switch in.Kind {
	case "label":
		ev, err := w.svc.Store.ModifyMessageLabel(ctx, p, a.MessageID, in.Label, in.LabelOp, in.ProviderLabel)
		return publish(ev, err)
	case "patch":
		if in.Read != nil {
			ev, e := w.svc.Store.UpdateMessageState(ctx, p, a.MessageID, in.Read)
			if e = publish(ev, e); e != nil {
				return e
			}
		}
		if in.Spam != nil {
			_, ev, e := w.svc.Store.SetMessageSpam(ctx, p, a.MessageID, *in.Spam)
			if e = publish(ev, e); e != nil {
				return e
			}
		}
		if in.Labels != nil {
			ev, e := w.svc.Store.ReplaceMessageLabels(ctx, p, a.MessageID, *in.Labels)
			if e = publish(&ev, e); e != nil {
				return e
			}
		}
		return nil
	case "delete":
		_, ev, e := w.svc.Store.TrashMessage(ctx, p, a.MessageID)
		return publish(ev, e)
	case "restore":
		_, ev, e := w.svc.Store.RestoreMessage(ctx, p, a.MessageID)
		return publish(ev, e)
	case "move":
		_, ev, e := w.svc.Store.MoveMessageToFolder(ctx, p, a.MessageID, in.Destination)
		return publish(ev, e)
	case "purge":
		path, _, ev, e := w.svc.Store.PurgeMessage(ctx, p, a.MessageID)
		if e != nil {
			return e
		}
		if path != "" {
			full, e := safepath.Join(w.svc.Config.DataDir, path)
			if e != nil {
				return e
			}
			if e = os.Remove(full); e != nil && !os.IsNotExist(e) {
				w.svc.Log.Error("purged MIME cleanup", "action_id", a.ID, "error", e)
			}
		}
		return publish(&ev, nil)
	}
	return fmt.Errorf("unknown action")
}

// EnqueueInboxAction is the single submission surface shared by REST and UI.
func (s *Service) EnqueueInboxAction(ctx context.Context, p model.Principal, inboxID, messageID string, in store.MessageActionInput) (store.MessageAction, error) {
	if in.TrashTarget != nil {
		return store.MessageAction{}, store.ErrConflict
	}
	if !p.CanAssist(inboxID) {
		return store.MessageAction{}, store.ErrForbidden
	}
	if _, err := s.Store.GetInbox(ctx, p, inboxID); err != nil {
		return store.MessageAction{}, err
	}
	m, err := s.Store.GetMessage(ctx, p, messageID)
	if err == nil {
		if m.InboxID != inboxID || m.Internal {
			return store.MessageAction{}, store.ErrNotFound
		}
		if in.Kind == "purge" && m.DeletedAt == nil {
			return store.MessageAction{}, store.ErrConflict
		}
	} else {
		if !errors.Is(err, store.ErrNotFound) {
			return store.MessageAction{}, err
		}
		rec, e := s.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
		if e != nil {
			return store.MessageAction{}, e
		}
		if in.Kind == "purge" {
			folders, e := s.Store.ListFolders(ctx, p.AccountID, inboxID)
			if e != nil {
				return store.MessageAction{}, e
			}
			trash := false
			for _, f := range folders {
				if f.Path == rec.FolderPath && f.Role == model.FolderRoleTrash {
					trash = true
				}
			}
			if !trash {
				return store.MessageAction{}, store.ErrConflict
			}
		}
	}
	switch in.Kind {
	case "label":
		if in.LabelOp != "add" && in.LabelOp != "remove" {
			return store.MessageAction{}, store.ErrConflict
		}
		if in.ProviderLabel {
			if _, ok := model.NormalizeProviderLabel(in.Label); !ok {
				return store.MessageAction{}, store.ErrConflict
			}
		} else if _, ok := model.NormalizeLabel(in.Label); !ok {
			return store.MessageAction{}, store.ErrConflict
		}
	case "patch":
		if in.Read == nil && in.Spam == nil && in.Labels == nil {
			return store.MessageAction{}, store.ErrConflict
		}
	case "delete", "restore", "purge":
	case "move":
		if in.Destination == "" {
			return store.MessageAction{}, store.ErrNotFound
		}
	default:
		return store.MessageAction{}, store.ErrConflict
	}
	a, err := s.Store.EnqueueMessageAction(ctx, p, inboxID, messageID, in)
	if err == nil {
		if s.ActionWaker != nil {
			s.ActionWaker.Wake()
		}
		s.Hub.Publish(model.Event{AccountID: p.AccountID, InboxID: inboxID, Type: "actions.changed", Transient: true})
	}
	return a, err
}

func (s *Service) WaitForInboxAction(ctx context.Context, p model.Principal, id string, timeout time.Duration) (store.MessageAction, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		a, err := s.Store.GetMessageAction(ctx, p, id)
		if err != nil {
			return a, err
		}
		if a.State != "pending" && a.State != "running" {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return a, ctx.Err()
		case <-timer.C:
			return a, fmt.Errorf("action timed out")
		case <-tick.C:
		}
	}
}

// RecheckFailed reconsiders failed actions and, when it is safe, returns them to
// the pending queue so the main worker retries them. It never executes an action
// itself and never touches 'ambiguous' rows.
//
// Safety is decided before requeueing, because "the last try failed" is not the
// same as "the operation still needs doing":
//
//   - A newer action for the same message supersedes the failure — whether it is
//     still unsettled or has already completed — because the old attempt is
//     stale and replaying it could undo the newer instruction.
//   - The provider's current state is checked where the action targets a remote
//     message, so an operation that already took effect is not repeated.
func (w *ActionWorker) RecheckFailed() {
	w.recheckFailed(context.Background())
}

func (w *ActionWorker) recheckFailed(ctx context.Context) {
	// next_attempt_at on a failed row is the next recheck time, so eligibility is
	// a plain "is it due now" comparison. The interval is applied once, where the
	// timestamp is written, never again in the query.
	due := time.Now()
	actions, err := w.svc.Store.ListFailedActionsForRecheck(ctx, due, recheckBatch)
	if err != nil {
		w.svc.Log.Error("action recheck list", "error", err)
		return
	}
	for _, a := range actions {
		requeue, err := w.recheckOne(ctx, a)
		if err != nil {
			w.svc.Log.Warn("action recheck deferred", "action_id", a.ID, "error", err)
			if derr := w.svc.Store.DeferMessageActionRecheck(ctx, a.ID, time.Now().Add(recheckDeferral)); derr != nil {
				w.svc.Log.Error("action recheck defer", "action_id", a.ID, "error", derr)
			}
			continue
		}
		if requeue {
			if err := w.svc.Store.RequeueMessageAction(ctx, a.ID); err != nil {
				w.svc.Log.Error("action requeue", "action_id", a.ID, "error", err)
				continue
			}
			w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
			w.Wake()
		}
	}
}

// recheckOne decides a single failed action's fate. It reports whether the
// action should be requeued. An error means the state could not be determined and
// the action's next recheck should be deferred.
func (w *ActionWorker) recheckOne(ctx context.Context, a store.MessageAction) (bool, error) {
	// Bulk purge locators can exist without any cache row. Cache absence must
	// never be mistaken for provider-confirmed deletion; leave these for review.
	if a.Input.Kind == "empty_trash" || a.Input.TrashTarget != nil {
		return false, nil
	}
	// A patch that only sets a flag is absolute state: repeating it cannot
	// duplicate an effect and is safe to retry once no newer action supersedes
	// it. A spam patch is the exception — against a standalone mailbox it is a
	// physical folder move at the provider, so it is decided from the provider's
	// current state like any other location change.
	if a.Input.Kind == "patch" && a.Input.Spam == nil {
		superseded, err := w.supersededByNewerAction(ctx, a)
		if err != nil {
			return false, err
		}
		return !superseded, nil
	}
	// Every other kind is a location change at the provider. Decide from the
	// provider's current state, never by blind replay.
	return w.resolveRemoteLocationAction(ctx, a)
}

// resolveRemoteLocationAction checks a failed move/delete/restore/purge against
// the provider's current location. It reports whether the action should be
// requeued. A location change counts as already applied only when the message is
// positively observed in the intended destination (or, for a purge, absent
// entirely). A move/delete whose target can no longer be located is left for a
// human rather than retried or silently completed, because its outcome is
// genuinely unknown.
func (w *ActionWorker) resolveRemoteLocationAction(ctx context.Context, a store.MessageAction) (bool, error) {
	superseded, err := w.supersededByNewerAction(ctx, a)
	if err != nil {
		return false, err
	}
	if superseded {
		// A newer instruction for the same message governs. Leave the stale
		// failure as-is so it is not replayed out of order.
		return false, nil
	}
	p := model.Principal{AccountID: a.AccountID, Admin: true}
	box, err := w.svc.Store.GetInbox(ctx, p, a.InboxID)
	if err != nil {
		return false, err
	}
	if _, err := w.svc.Store.GetMessage(ctx, p, a.MessageID); err == nil {
		// A locally stored message: the local store is authoritative and the
		// location action is safe to retry.
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if box.Kind != model.InboxKindStandalone {
		return false, nil
	}
	rec, err := w.svc.Store.GetRemoteMessage(ctx, p.AccountID, a.InboxID, a.MessageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A purge is satisfied by the message's absence: settle it as done. A
			// move/delete that lost its locator has an unknown outcome; leave it
			// failed rather than guess.
			if a.Input.Kind == "purge" {
				if serr := w.svc.Store.SettleMessageAction(ctx, a.ID, "done", "", time.Now()); serr != nil {
					return false, serr
				}
				w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
			}
			return false, nil
		}
		return false, err
	}
	// A purge has no destination folder: its intended effect is the message's
	// absence, so a message that still exists has not been purged and is safe to
	// retry.
	if a.Input.Kind == "purge" {
		return true, nil
	}
	target := ""
	if a.Input.Kind == "move" {
		target = a.Input.Destination
	}
	if role := destinationRole(a); role != "" {
		folders, err := w.svc.Store.ListFolders(ctx, p.AccountID, a.InboxID)
		if err != nil {
			return false, err
		}
		for _, f := range folders {
			if f.Role == role && (f.Origin == "remote" || f.RoleLocked) {
				target = f.Path
				break
			}
		}
	}
	if target == "" {
		return false, nil
	}
	if strings.EqualFold(target, rec.FolderPath) {
		// The message already sits where the action wanted it: it is settled.
		if err := w.svc.Store.SettleMessageAction(ctx, a.ID, "done", "", time.Now()); err != nil {
			return false, err
		}
		w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
		return false, nil
	}
	// Still in the source folder: the earlier attempt did not take effect, so the
	// move/purge is safe to retry.
	return true, nil
}

// supersededByNewerAction reports whether a newer instruction for the same
// message governs, so the failed attempt must not be replayed.
//
// Ordering is durable: ListMessageActionsForMessage returns the message's
// actions oldest first by rowid, so position in that slice is the enqueue order.
// Only actions created *after* the failed one count; an older action can never
// supersede it.
//
// A newer action supersedes the failure when it is unsettled (pending, running
// or ambiguous) — replaying the old attempt would jump the queue — or when it
// has already completed. A completed newer action is the case the old check
// missed: replaying the stale failure would silently undo a change the user
// made after the failure, so the failure stays put for a human.
func (w *ActionWorker) supersededByNewerAction(ctx context.Context, a store.MessageAction) (bool, error) {
	actions, err := w.svc.Store.ListMessageActionsForMessage(ctx, a.AccountID, a.InboxID, a.MessageID)
	if err != nil {
		return false, err
	}
	seen := false
	for _, other := range actions {
		if other.ID == a.ID {
			seen = true
			continue
		}
		if !seen {
			// Enqueued before the failed action: it cannot supersede it.
			continue
		}
		switch other.State {
		case "pending", "running", "ambiguous", "done":
			return true, nil
		}
	}
	return false, nil
}

// destinationRole maps a location action onto the folder role it moves toward,
// or "" when the action's destination is an explicit path rather than a role.
func destinationRole(a store.MessageAction) string {
	if a.Input.Spam != nil {
		if *a.Input.Spam {
			return model.FolderRoleSpam
		}
		return model.FolderRoleInbox
	}
	switch a.Input.Kind {
	case "delete":
		return model.FolderRoleTrash
	case "restore":
		return model.FolderRoleInbox
	}
	return ""
}
