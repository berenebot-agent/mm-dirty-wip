package httpapp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	if kind == "move" && box.Kind != model.InboxKindStandalone {
		folder, err := s.Service.Store.GetFolderByPath(r.Context(), p.AccountID, box.ID, dest)
		if err != nil {
			return false
		}
		dest = folder.ID
	}
	in := store.MessageActionInput{Kind: kind, Destination: dest}
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
	a, err := s.Service.EnqueueInboxAction(r.Context(), p, mb.inbox.ID, m.ID, in)
	if err != nil {
		if ui {
			s.uiError(w, err, 400)
		} else {
			mapStoreError(w, err)
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
			writeJSON(w, 504, map[string]any{"error": "action wait timed out; action remains queued", "action": a})
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
		if e == nil {
			labels, e := s.Service.Store.RemoteMessageLabels(r.Context(), p.AccountID, box.ID, id)
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
	// drives both the action-queue popup and the standalone mailbox banner.
	writeJSON(w, 200, map[string]any{
		"items": actions,
		"syncs": s.metadataSyncStatuses(r.Context(), principal(r)),
		"csrf":  csrf(r),
	})
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.Service.Store.GetMessageAction(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"action": a})
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
			if a.Input.Kind == "delete" || a.Input.Kind == "purge" || a.Input.Kind == "move" || a.Input.Kind == "restore" || a.Input.Spam != nil {
				data.Messages = append(data.Messages[:j], data.Messages[j+1:]...)
				j--
			}
		}
	}
}

// emptyTrashBatch bounds how many purge jobs a single empty-trash request
// queues. Every job is its own write transaction against the single write
// connection, so an inbox with a large trash folder would otherwise pin the
// connection (and the UI POST) for minutes. The extra trash beyond the cap
// drains on later empty-trash calls, so nothing is lost, only deferred.
const emptyTrashBatch = 500

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
	// Fetch one beyond the work cap so the response can accurately tell the
	// caller whether another batch remains, without materializing the whole
	// trash folder in memory.
	ids := []string{}
	if box.Kind == model.InboxKindStandalone {
		folder, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleTrash)
		if !ok {
			writeError(w, 409, "no Trash folder mapped")
			return
		}
		msgs, e := s.Service.Store.ListRemoteMessagesFiltered(r.Context(), p.AccountID, box.ID, store.RemoteMessageFilter{
			FolderPath: folder.Path,
			Limit:      emptyTrashBatch + 1,
		})
		if e != nil {
			mapStoreError(w, e)
			return
		}
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
	} else {
		ids, err = s.Service.Store.AllMessageIDsLimit(r.Context(), p, store.MessageFilter{InboxID: box.ID, Trashed: true, IncludeSpam: true}, emptyTrashBatch+1)
		if err != nil {
			mapStoreError(w, err)
			return
		}
	}
	truncated := false
	if len(ids) > emptyTrashBatch {
		ids = ids[:emptyTrashBatch]
		truncated = true
	}
	jobs := []store.MessageAction{}
	for _, id := range ids {
		a, e := s.Service.EnqueueInboxAction(r.Context(), p, box.ID, id, store.MessageActionInput{Kind: "purge"})
		if e != nil {
			mapStoreError(w, e)
			return
		}
		jobs = append(jobs, a)
	}
	if truncated {
		s.Service.Log.Info("empty trash batch truncated", "inbox_id", box.ID, "queued", len(jobs))
	}
	if ui {
		notice := queuedNotice("purge", len(jobs), "")
		if truncated {
			notice += " Batch capped at " + strconv.Itoa(emptyTrashBatch) + ": after this batch finishes, empty trash again to continue."
		}
		http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/trash?notice="+url.QueryEscape(notice), 303)
		return
	}
	if truncated {
		writeJSON(w, 202, map[string]any{"queued": true, "actions": jobs, "truncated": true, "remaining": true})
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		deadline := time.Now().Add(30 * time.Second)
		for _, a := range jobs {
			settled, e := s.Service.WaitForInboxAction(r.Context(), p, a.ID, time.Until(deadline))
			if e != nil {
				writeJSON(w, 504, map[string]any{"actions": jobs})
				return
			}
			if settled.State != "done" {
				writeJSON(w, 409, map[string]any{"action": settled})
				return
			}
		}
		writeJSON(w, 200, map[string]int{"purged": len(jobs)})
		return
	}
	writeJSON(w, 202, map[string]any{"queued": true, "actions": jobs})
}
