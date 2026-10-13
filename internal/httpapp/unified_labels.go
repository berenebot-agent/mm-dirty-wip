package httpapp

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// mailLabel is the shared discovery and UI projection. Provider folders remain
// folders in storage: only their presentation is unified. In particular an IMAP
// move must never be implemented by changing local free-text message labels.
type mailLabel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"` // folder or tag
	Membership string `json:"membership"` // exclusive or multiple
	Source     string `json:"source"` // provider or local
	FolderID   string `json:"folder_id,omitempty"`
	FolderPath string `json:"-"`
	URL        string `json:"-"`
	Selected   bool   `json:"-"`
	CanRemove  bool   `json:"can_remove"`
}

// mailboxLabels draws from the *existing* provider folder and label indexes.
// Gmail's custom labels already exist in both indexes: prefer the provider's
// persistent label/folder ID and suppress the duplicate free-text entry.
func (s *Server) mailboxLabels(ctx context.Context, p model.Principal, box model.Inbox) ([]mailLabel, error) {
	folders, err := s.Service.Store.ListFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		return nil, err
	}
	google := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID)
	microsoft := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID)
	prefix := "/ui/inboxes/" + url.PathEscape(box.ID)
	labels := make([]mailLabel, 0, len(folders))
	seenTags := map[string]bool{}
	for _, f := range folders {
		// System destinations are exposed in the fixed top navigation, not
		// repeated under Labels. Non-selectable IMAP hierarchy placeholders
		// are not actionable labels either.
		if f.IsSystem || !f.Selectable || (f.Role != model.FolderRoleFolder && f.Role != model.FolderRoleLabel) {
			continue
		}
		tag := google
		entry := mailLabel{
			ID: "folder:" + f.ID, Name: f.Name, Kind: "folder",
			Membership: "exclusive", Source: "local", FolderID: f.ID,
			FolderPath: f.Path, URL: prefix + "/folder?folder=" + url.QueryEscape(f.ID),
		}
		if box.Kind == model.InboxKindStandalone {
			entry.Source = "provider"
		}
		if tag {
			entry.Kind, entry.Membership, entry.ID, entry.CanRemove = "tag", "multiple", "tag:" + f.Name, true
			seenTags[strings.ToLower(f.Name)] = true
		}
		labels = append(labels, entry)
	}
	var tags []string
	if box.Kind == model.InboxKindStandalone {
		tags, err = s.remoteMailbox().ListRemoteLabels(ctx, p, box.ID)
	} else {
		tags, err = s.Service.Store.ListInboxLabels(ctx, p, box.ID)
	}
	if err != nil {
		return nil, err
	}
	for _, name := range tags {
		if seenTags[strings.ToLower(name)] {
			continue
		}
		source := "local"
		if microsoft || google {
			source = "provider"
		}
		labels = append(labels, mailLabel{
			ID: "tag:" + name, Name: name, Kind: "tag",
			Membership: "multiple", Source: source, CanRemove: true,
			URL: prefix + "/label?name=" + url.QueryEscape(name),
		})
	}
	sort.SliceStable(labels, func(i, j int) bool {
		if strings.EqualFold(labels[i].Name, labels[j].Name) {
			return labels[i].ID < labels[j].ID
		}
		return strings.ToLower(labels[i].Name) < strings.ToLower(labels[j].Name)
	})
	return labels, nil
}

func selectedMailboxLabels(labels []mailLabel, m model.Message) []mailLabel {
	for i := range labels {
		if labels[i].Kind == "folder" {
			labels[i].Selected = (m.MailboxID != "" && m.MailboxID == labels[i].FolderID) ||
				(m.FolderPath != "" && m.FolderPath == labels[i].FolderPath)
		} else {
			for _, applied := range m.Labels {
				if strings.EqualFold(applied, labels[i].Name) {
					labels[i].Selected = true
					break
				}
			}
		}
	}
	return labels
}

// labelAction validates the target against the current inbox before enqueuing
// anything. A folder label means a MOVE, an ordinary label means a metadata
// patch. Removing a folder is rejected instead of silently moving to Inbox.
// IMAP does not expose the local-only tag writer in this unified API.
func (s *Server) labelAction(ctx context.Context, p model.Principal, box model.Inbox, m model.Message, labelID, operation string) (store.MessageActionInput, string, error) {
	if operation != "add" && operation != "remove" {
		return store.MessageActionInput{}, "operation must be add or remove", nil
	}
	if !p.CanAssist(box.ID) && !p.Admin {
		return store.MessageActionInput{}, "forbidden", nil
	}
	labels, err := s.mailboxLabels(ctx, p, box)
	if err != nil {
		return store.MessageActionInput{}, "", err
	}
	var target *mailLabel
	for i := range labels {
		if labels[i].ID == labelID {
			target = &labels[i]
			break
		}
	}
	if target == nil {
		// Adding a new tag remains supported for local, Gmail and Microsoft
		// inboxes. IMAP uses remote folders: an unknown tag must not be
		// silently invented as MailMoose-only metadata.
		if strings.HasPrefix(labelID, "tag:") {
			name := strings.TrimPrefix(labelID, "tag:")
			clean, valid := model.NormalizeLabel(name)
			if !valid || clean != name {
				return store.MessageActionInput{}, "invalid label", nil
			}
			if box.Kind == model.InboxKindStandalone &&
				!s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) &&
				!s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
				return store.MessageActionInput{}, "IMAP labels must refer to an existing folder", nil
			}
			target = &mailLabel{ID: labelID, Name: name, Kind: "tag"}
		} else {
			return store.MessageActionInput{}, "label not found", nil
		}
	}
	if target.Kind == "folder" {
		if operation == "remove" {
			return store.MessageActionInput{}, "a folder-backed label cannot be removed; move to another folder", nil
		}
		destination := target.FolderID
		if box.Kind == model.InboxKindStandalone && !s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
			destination = target.FolderPath
		}
		return store.MessageActionInput{Kind: "move", Destination: destination}, "", nil
	}
	current := append([]string{}, m.Labels...)
	found := false
	next := make([]string, 0, len(current)+1)
	for _, old := range current {
		if strings.EqualFold(old, target.Name) {
			found = true
			if operation == "remove" {
				continue
			}
		}
		next = append(next, old)
	}
	if operation == "add" && !found {
		next = append(next, target.Name)
	}
	return store.MessageActionInput{Kind: "patch", Labels: &next}, "", nil
}

// GET /v1/inboxes/{id}/label-catalog is the capability-aware label API. The
// older /labels flat array and /folders management routes remain unchanged.
func (s *Server) apiMailboxLabelCatalog(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	labels, err := s.mailboxLabels(r.Context(), p, mb.inbox)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if labels == nil {
		labels = []mailLabel{}
	}
	writeJSON(w, http.StatusOK, newEnvelope(labels, "", model.CompletenessComplete, nil))
}

// POST /v1/inboxes/{id}/messages/{messageId}/labels
// {"label_id":"folder:<id>","operation":"add"} or
// {"label_id":"tag:Invoices","operation":"remove"}. The existing action queue
// and ?wait=true behaviour are reused, including replayable events.
func (s *Server) apiUnifiedMessageLabels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LabelID   string `json:"label_id"`
		Operation string `json:"operation"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	m, _, err := s.resolveActionMessage(r, p, r.PathValue("messageId"))
	if err != nil || m.InboxID != mb.inbox.ID {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	action, problem, err := s.labelAction(r.Context(), p, mb.inbox, m, in.LabelID, in.Operation)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if problem != "" {
		code := http.StatusBadRequest
		if problem == "forbidden" {
			code = http.StatusForbidden
		} else if problem == "label not found" {
			code = http.StatusNotFound
		} else if strings.Contains(problem, "cannot be removed") {
			code = http.StatusConflict
		}
		writeError(w, code, problem)
		return
	}
	s.submitMessageAction(w, r, m.ID, action, false)
}

func (s *Server) uiUnifiedMessageLabels(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, mb, err := s.resolveActionMessage(r, p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	box, err := s.Service.Store.GetInbox(r.Context(), p, mb.inbox.ID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	action, problem, err := s.labelAction(r.Context(), p, box, m, r.Form.Get("label_id"), r.Form.Get("action"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if problem != "" {
		s.uiError(w, model.NewMailboxError(model.ErrKindInvalid, problem, false, nil), 400)
		return
	}
	s.submitMessageAction(w, r, m.ID, action, true)
}
