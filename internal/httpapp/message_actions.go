package httpapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// messageNoun renders a count with its noun: "1 message", "3 messages".
func messageNoun(count int) string {
	if count == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", count)
}

// queuedNotice names the action a queued job will take, in the future tense, so
// a flash never reads as a bare "action queued". It is shared by the
// single-message, local bulk and remote bulk paths so their wording cannot
// drift. action is one of read, unread, delete, restore, purge, move, spam,
// unspam or labels; dest is the move destination's display name (or path) and
// is only used for move.
func queuedNotice(action string, count int, dest string) string {
	switch action {
	case "read":
		return messageNoun(count) + " will be marked read"
	case "unread":
		return messageNoun(count) + " will be marked unread"
	case "delete":
		return messageNoun(count) + " will move to trash"
	case "restore":
		return messageNoun(count) + " will move back to Inbox"
	case "purge":
		return messageNoun(count) + " will be deleted permanently"
	case "move":
		if dest != "" {
			return messageNoun(count) + " will move to " + dest
		}
		return messageNoun(count) + " will be moved"
	case "spam":
		return messageNoun(count) + " will move to spam"
	case "unspam":
		return messageNoun(count) + " will move out of spam"
	case "labels":
		return "Labels will be updated"
	}
	return messageNoun(count) + " queued"
}

// actionLabel recovers the user-facing action name from a normalised action
// input: read/unread/spam/labels arrive as a "patch" with the specific field
// set, so the display label is derived from the field rather than the kind.
func actionLabel(in store.MessageActionInput) string {
	if in.Kind == "classify" {
		return "move"
	}
	if in.Kind == "label" {
		return "labels"
	}
	if in.Kind == "patch" {
		switch {
		case in.Read != nil && *in.Read:
			return "read"
		case in.Read != nil && !*in.Read:
			return "unread"
		case in.Spam != nil && *in.Spam:
			return "spam"
		case in.Spam != nil && !*in.Spam:
			return "unspam"
		case in.Labels != nil:
			return "labels"
		}
	}
	return in.Kind
}

func (s *Server) enqueueBulkAction(r *http.Request, p model.Principal, box model.Inbox, id, kind, dest string) bool {
	in := store.MessageActionInput{Kind: kind, Destination: dest}
	if kind == "move" {
		folder, err := s.Service.Store.GetFolderDestination(r.Context(), p.AccountID, box.ID, dest)
		if err != nil {
			return false
		}
		in, err = s.folderMoveAction(r.Context(), p, box, &folder.ID)
		if err != nil {
			return false
		}
	}
	if kind == "read" || kind == "unread" {
		read := kind == "read"
		in.Kind = "patch"
		in.Read = &read
	}
	_, err := s.Service.EnqueueInboxAction(r.Context(), p, box.ID, id, in)
	return err == nil
}

func (s *Server) submitMessageAction(w http.ResponseWriter, r *http.Request, id string, in store.MessageActionInput, ui bool) {
	p := principal(r)
	m, mb, err := s.resolveActionMessage(r, p, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	id = m.ID
	a, err := s.Service.EnqueueInboxAction(r.Context(), p, mb.inbox.ID, m.ID, in)
	if err != nil {
		if ui {
			s.uiError(w, err, 400)
		} else {
			// Mailbox capability errors use the same 409 unsupported-operation
			// response as the canonical label endpoints.
			var mailboxErr *model.MailboxError
			if errors.As(err, &mailboxErr) {
				mapMailboxError(w, err)
			} else {
				mapStoreError(w, err)
			}
		}
		return
	}
	if ui {
		notice := queuedNotice(actionLabel(in), 1, "")
		target := "/ui/inboxes/" + url.PathEscape(mb.inbox.ID)
		if strings.HasPrefix(r.URL.Path, "/ui/messages/") && strings.HasSuffix(r.URL.Path, "/labels") {
			target = "/ui/messages/" + url.PathEscape(m.ID)
		}
		http.Redirect(w, r, target+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		a, err = s.Service.WaitForInboxAction(r.Context(), p, a.ID, 30*time.Second)
		if err != nil {
			writeJSON(w, 504, map[string]any{"error": "action wait timed out; action remains queued", "code": "timeout", "action": a})
			return
		}
		if a.State == "done" {
			if strings.HasSuffix(r.URL.Path, "/seen") {
				writeJSON(w, 200, map[string]any{"id": id, "seen": *in.Read})
				return
			}
			if in.Kind == "delete" || in.Kind == "purge" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			updated, _, _, e := s.resolveMessageAny(r.Context(), p, id)
			if e != nil {
				mapStoreError(w, e)
				return
			}
			writeJSON(w, 200, s.messageResponse(r, updated))
			return
		}
	}
	status := http.StatusAccepted
	if a.State == "done" {
		status = http.StatusOK
	}
	if a.State == "failed" || a.State == "ambiguous" {
		status = http.StatusConflict
	}
	w.Header().Set("Location", "/v1/messages/"+id+"?action="+a.ID)
	if status == http.StatusConflict {
		writeJSON(w, status, map[string]any{"error": "action failed or its outcome is uncertain", "code": "conflict", "queued": false, "action": a})
		return
	}
	writeJSON(w, status, map[string]any{"queued": a.State == "pending" || a.State == "running", "action": a})
}

// Submission uses durable cached identity only: never dial a provider just to
// queue an action, including when the connector is temporarily unavailable.
func (s *Server) resolveActionMessage(r *http.Request, p model.Principal, id string) (model.Message, mailboxBackend, error) {
	m, err := s.Service.Store.GetMessage(r.Context(), p, id)
	if err == nil {
		return m, mailboxBackend{inbox: model.Inbox{ID: m.InboxID}}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return m, mailboxBackend{}, err
	}
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return m, mailboxBackend{}, err
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) {
			continue
		}
		rec, e := s.Service.Store.GetRemoteMessage(r.Context(), p.AccountID, box.ID, id)
		if errors.Is(e, store.ErrNotFound) {
			arrival, err := s.Service.Store.GetRemoteArrival(r.Context(), p.AccountID, id)
			if err != nil || arrival.InboxID != box.ID || !s.remoteMailbox().InScope(p.AccountID, box.ID, arrival.FolderPath) {
				continue
			}
			rec, e = s.Service.Store.RemoteMessageForArrival(r.Context(), p.AccountID, id)
			if e == nil && (rec.InboxID != box.ID || !s.remoteMailbox().InScope(p.AccountID, box.ID, rec.FolderPath)) {
				continue
			}
		}
		if e == nil {
			labels, e := s.Service.Store.RemoteMessageLabels(r.Context(), p.AccountID, box.ID, rec.ID)
			if e != nil {
				return m, mailboxBackend{}, e
			}
			return model.Message{ID: rec.ID, InboxID: box.ID, Read: rec.Read, Labels: labels}, mailboxBackend{inbox: box}, nil
		}
	}
	return m, mailboxBackend{}, store.ErrNotFound
}

func (s *Server) queuedUIAction(w http.ResponseWriter, r *http.Request, kind string) {
	in := store.MessageActionInput{Kind: kind}
	if kind == "read" {
		read := r.Form.Get("read") == "1"
		in.Kind = "patch"
		in.Read = &read
	}
	if kind == "spam" {
		spam := r.Form.Get("spam") == "1"
		in.Kind = "patch"
		in.Spam = &spam
	}
	s.submitMessageAction(w, r, r.PathValue("id"), in, true)
}

// actionView is one row of the queue: the action plus the name of the inbox it
// belongs to. The queue lists the account's whole mailbox set, so a row that
// does not name its mailbox leaves the reader to guess which one it came from.
type actionView struct {
	store.MessageAction
	InboxName string `json:"inbox_name,omitempty"`
}

// inboxLabel is a mailbox's display name, or its address when it has no name.
func inboxLabel(box model.Inbox) string {
	if box.DisplayName != "" {
		return box.DisplayName
	}
	return box.Address
}

// actionViews names each action's inbox. A name that cannot be read leaves the
// row unnamed rather than failing the poll.
func (s *Server) actionViews(ctx context.Context, p model.Principal, actions []store.MessageAction) []actionView {
	names := map[string]string{}
	if boxes, err := s.Service.Store.ListInboxes(ctx, p); err == nil {
		for _, box := range boxes {
			names[box.ID] = inboxLabel(box)
		}
	}
	out := make([]actionView, 0, len(actions))
	for _, a := range actions {
		out = append(out, actionView{MessageAction: a, InboxName: names[a.InboxID]})
	}
	return out
}

// inboxName is the name of the mailbox an action belongs to.
func (s *Server) inboxName(ctx context.Context, p model.Principal, inboxID string) string {
	box, err := s.Service.Store.GetInbox(ctx, p, inboxID)
	if err != nil {
		return ""
	}
	return inboxLabel(box)
}

func (s *Server) uiActionQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		action := r.Form.Get("action")
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			var in struct {
				Action string `json:"action"`
			}
			if !decodeJSON(w, r, &in) {
				return
			}
			action = in.Action
		}
		if action != "retry" && action != "dismiss" {
			writeError(w, 400, "action must be retry or dismiss")
			return
		}
		if err := s.Service.Store.UpdateMessageAction(r.Context(), principal(r), r.PathValue("id"), action == "retry"); err != nil {
			mapStoreError(w, err)
			return
		}
		if s.Service.ActionWaker != nil {
			s.Service.ActionWaker.Wake()
		}
	}
	actions, err := s.Service.Store.ListMessageActions(r.Context(), principal(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// The same response carries the live metadata-sync status so a single poll
	// drives both the action-queue popup and the standalone mailbox banner, plus
	// the connection health so the reconnect banner and its control appear
	// without a page load when a provider rejects an authorization.
	writeJSON(w, 200, map[string]any{
		"items":       s.actionViews(r.Context(), principal(r), actions),
		"syncs":       s.metadataSyncStatuses(r.Context(), principal(r)),
		"connections": s.connectionStatesFor(r.Context(), principal(r)),
		"csrf":        csrf(r),
	})
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	a, err := s.Service.Store.GetMessageAction(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"action": actionView{MessageAction: a, InboxName: s.inboxName(r.Context(), p, a.InboxID)}})
}

// pendingMessageOverlay leaves durable provider locators and confirmed local
// state untouched. The UI projects pending intent until the worker settles it.
func (s *Server) pendingMessageOverlay(r *http.Request, data *pageData) {
	actions, err := s.Service.Store.ListMessageActions(r.Context(), principal(r))
	if err != nil {
		return
	}
	for i := len(actions) - 1; i >= 0; i-- {
		a := actions[i]
		if a.State != "pending" && a.State != "running" {
			continue
		}
		for j := 0; j < len(data.Messages); j++ {
			m := &data.Messages[j]
			if m.ID != a.MessageID {
				continue
			}
			if a.Input.Read != nil {
				m.Read = *a.Input.Read
			}
			if a.Input.Labels != nil {
				m.Labels = *a.Input.Labels
			}
			if a.Input.Kind == "delete" || a.Input.Kind == "purge" || a.Input.Kind == "move" || a.Input.Kind == "classify" || a.Input.Kind == "restore" || a.Input.Spam != nil {
				data.Messages = append(data.Messages[:j], data.Messages[j+1:]...)
				j--
			}
		}
	}
}

func (s *Server) enqueueEmptyTrash(w http.ResponseWriter, r *http.Request, ui bool) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !p.CanAssist(box.ID) {
		writeError(w, 403, "forbidden")
		return
	}
	// The parent owns discovery and batching; the request never enumerates an
	// entire mailbox or waits for provider calls merely to submit the operation.
	a, err := s.Service.Store.EnqueueEmptyTrash(r.Context(), p, box.ID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if s.Service.ActionWaker != nil {
		s.Service.ActionWaker.Wake()
	}
	if ui {
		notice := "Empty trash is running in the background."
		if a.State == "failed" || a.State == "ambiguous" {
			notice = "Empty trash needs attention — open Queue."
		}
		http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/trash?notice="+url.QueryEscape(notice), 303)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", "/v1/actions/"+a.ID)
	if a.State == "failed" || a.State == "ambiguous" {
		writeJSON(w, 409, map[string]any{"error": "empty trash failed or its outcome is uncertain", "code": "conflict", "action": a})
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		settled, e := s.Service.WaitForInboxAction(r.Context(), p, a.ID, 30*time.Second)
		if e != nil {
			writeJSON(w, 504, map[string]any{"error": "action wait timed out; action remains queued", "code": "timeout", "action": settled, "actions": []store.MessageAction{settled}})
			return
		}
		if settled.State != "done" {
			writeJSON(w, 409, map[string]any{"error": "empty trash failed or its outcome is uncertain", "code": "conflict", "action": settled})
			return
		}
		writeJSON(w, 200, map[string]any{"purged": settled.EmptyTrash.Purged, "action": settled})
		return
	}
	writeJSON(w, 202, map[string]any{"queued": true, "action": a, "actions": []store.MessageAction{a}})
}
