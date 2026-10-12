package httpapp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

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
		http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice=Action+queued", http.StatusSeeOther)
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
			writeJSON(w, 200, sanitizedMessage(updated))
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
	if kind == "labels" {
		m, _, err := s.resolveActionMessage(r, principal(r), r.PathValue("id"))
		if err != nil {
			mapStoreError(w, err)
			return
		}
		labels := append([]string{}, m.Labels...)
		label := strings.TrimSpace(r.Form.Get("label"))
		if r.Form.Get("action") == "add" {
			labels = append(labels, label)
		} else if r.Form.Get("action") == "remove" {
			kept := []string{}
			for _, l := range labels {
				if !strings.EqualFold(l, label) {
					kept = append(kept, l)
				}
			}
			labels = kept
		} else {
			writeError(w, 400, "unknown action")
			return
		}
		in.Kind = "patch"
		in.Labels = &labels
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
	ids := []string{}
	if box.Kind == model.InboxKindStandalone {
		folder, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleTrash)
		if !ok {
			writeError(w, 409, "no Trash folder mapped")
			return
		}
		msgs, e := s.Service.Store.ListRemoteMessages(r.Context(), p.AccountID, box.ID, folder.Path)
		if e != nil {
			mapStoreError(w, e)
			return
		}
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
	} else {
		ids, err = s.Service.Store.AllMessageIDs(r.Context(), p, store.MessageFilter{InboxID: box.ID, Trashed: true, IncludeSpam: true})
		if err != nil {
			mapStoreError(w, err)
			return
		}
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
	if ui {
		http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/trash?notice=Trash+purge+queued", 303)
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
