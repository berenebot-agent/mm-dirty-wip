package app

import (
	"context"
	"errors"
	"fmt"
	"os"
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
}

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
		a, ok, err := w.svc.Store.ClaimMessageAction(ctx, "actions")
		if err != nil || !ok {
			cancel()
			return
		}
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
		next := time.Now().Add(time.Duration(a.Attempts) * time.Second)
		outcome, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.svc.Store.SettleMessageAction(outcome, a.ID, state, reason, next); err != nil {
			w.svc.Log.Error("action settlement", "action_id", a.ID, "error", err)
		}
		w.svc.Hub.Publish(model.Event{AccountID: a.AccountID, InboxID: a.InboxID, Type: "actions.changed", Transient: true})
	}()
	err := w.apply(ctx, a)
	if err == nil {
		return
	}
	state, reason = "failed", "Action could not be completed"
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		if mb.Message != "" {
			reason = mb.Message
		}
		if mb.Retryable && a.Input.Kind == "patch" && a.Input.Spam == nil && a.Attempts < 5 {
			state = "pending"
		}
		if mb.Retryable && (a.Input.Kind != "patch" || a.Input.Spam != nil) {
			state, reason = "ambiguous", "Remote outcome uncertain; verify the message before dismissing or submitting another action"
		}
	}
}

func (w *ActionWorker) apply(ctx context.Context, a store.MessageAction) error {
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
