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

// mailLabel exposes one user-facing label while preserving provider semantics.
// 'exclusive' means a MOVE, not an additive metadata update.
type mailLabel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Membership string `json:"membership"`
	Source     string `json:"source"`
	FolderID   string `json:"folder_id,omitempty"`
	FolderPath string `json:"-"`
	URL        string `json:"-"`
	Selected   bool   `json:"-"`
	CanRemove  bool   `json:"can_remove"`
	CanRename  bool   `json:"can_rename"`
	CanDelete  bool   `json:"can_delete"`
}

func (s *Server) mailboxLabels(ctx context.Context, p model.Principal, box model.Inbox) ([]mailLabel, error) {
	folders, err := s.Service.Store.ListFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		return nil, err
	}
	google := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID)
	microsoft := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID)
	prefix := "/ui/inboxes/" + url.PathEscape(box.ID)
	out := make([]mailLabel, 0, len(folders))
	seen := map[string]bool{}
	for _, folder := range folders {
		if folder.IsSystem || !folder.Selectable || (folder.Role != model.FolderRoleFolder && folder.Role != model.FolderRoleLabel) {
			continue
		}
		entry := mailLabel{
			ID: "folder:" + folder.ID, Name: folder.Name, Kind: "folder",
			Membership: "exclusive", Source: "local", FolderID: folder.ID,
			FolderPath: folder.Path, URL: prefix + "/folder?folder=" + url.QueryEscape(folder.ID),
			CanRename: true, CanDelete: true,
		}
		if box.Kind == model.InboxKindStandalone {
			entry.Source = "provider"
		}
		if google {
			// The Gmail custom-label index also appears as remote folders.
			// Project it once, as a multi-valued label, with a folder-indexed
			// view (Google stores membership by provider label ID).
			entry.ID = "tag:" + folder.Name
			entry.Kind = "tag"
			entry.Membership = "multiple"
			entry.CanRemove = true
			seen[strings.ToLower(folder.Name)] = true
		}
		out = append(out, entry)
	}
	var tags []string
	if box.Kind == model.InboxKindDomain {
		tags, err = s.Service.Store.ListInboxLabels(ctx, p, box.ID)
	} else if google || microsoft {
		tags, err = s.remoteMailbox().ListRemoteLabels(ctx, p, box.ID)
	} // Raw IMAP has no server-side multi-valued label semantics. Never
	// advertise legacy local IMAP tags as if they were remote folders.
	if err != nil {
		return nil, err
	}
	if box.Kind == model.InboxKindDomain || microsoft {
		defined, e := s.Service.Store.ListDefinedLabels(ctx, p.AccountID, box.ID)
		if e != nil {
			return nil, e
		}
		tags = append(tags, defined...)
	}
	for _, name := range tags {
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		source := "local"
		editable := box.Kind == model.InboxKindDomain
		if microsoft || google {
			source = "provider"
		}
		out = append(out, mailLabel{
			ID: "tag:" + name, Name: name, Kind: "tag", Membership: "multiple",
			Source: source, CanRemove: true, CanRename: editable, CanDelete: editable,
			URL: prefix + "/label?name=" + url.QueryEscape(name),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].ID < out[j].ID
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
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

// labelAction translates free-text or explicit IDs to a provider-aware action.
// A free-text name matches an existing label first; an unrecognised name becomes
// an additive label only on providers supporting several labels per message.
// For folder/category name collisions, a plain name favours an additive tag;
// an explicit folder ID remains available to API clients.
func (s *Server) labelAction(ctx context.Context, p model.Principal, box model.Inbox, labelID, name, operation string) (store.MessageActionInput, string, error) {
	if operation != "add" && operation != "remove" {
		return store.MessageActionInput{}, "operation must be add or remove", nil
	}
	if !p.CanAssist(box.ID) && !p.Admin {
		return store.MessageActionInput{}, "forbidden", nil
	}
	catalog, err := s.mailboxLabels(ctx, p, box)
	if err != nil {
		return store.MessageActionInput{}, "", err
	}
	var target *mailLabel
	labelID, name = strings.TrimSpace(labelID), strings.TrimSpace(name)
	if labelID != "" {
		for i := range catalog {
			if catalog[i].ID == labelID {
				target = &catalog[i]
				break
			}
		}
		if target == nil {
			// API clients may apply a new tag by ID without pre-creating it.
			if !strings.HasPrefix(labelID, "tag:") {
				return store.MessageActionInput{}, "label not found", nil
			}
			name = strings.TrimPrefix(labelID, "tag:")
		}
	}
	if target == nil {
		clean, valid := model.NormalizeLabel(name)
		if !valid || clean != name {
			return store.MessageActionInput{}, "invalid label name", nil
		}
		for i := range catalog {
			if strings.EqualFold(catalog[i].Name, name) {
				// Tag wins if Outlook also has a folder with the same name.
				if target == nil || catalog[i].Kind == "tag" {
					target = &catalog[i]
				}
			}
		}
	}
	if target == nil {
		if operation == "remove" {
			// Existing legacy free-text IMAP tags can still be removed without
			// exposing them in the unified catalogue.
			if box.Kind != model.InboxKindStandalone || s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) ||
				s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
				return store.MessageActionInput{}, "label not found", nil
			}
		} else if box.Kind == model.InboxKindStandalone &&
			!s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) &&
			!s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
			return store.MessageActionInput{}, "IMAP labels must match an existing folder; create one under Labels first", nil
		}
		target = &mailLabel{Name: name, Kind: "tag", Membership: "multiple"}
	}
	if target.Kind == "folder" {
		if operation == "remove" {
			return store.MessageActionInput{}, "folder-backed labels cannot be removed; move the message instead", nil
		}
		destination := target.FolderID
		if box.Kind == model.InboxKindStandalone && !s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
			destination = target.FolderPath
		}
		return store.MessageActionInput{Kind: "move", Destination: destination}, "", nil
	}
	return store.MessageActionInput{Kind: "label", Label: target.Name, LabelOp: operation}, "", nil
}

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
// {"name":"Invoices","operation":"add"} is equivalent to
// {"label_id":"tag:Invoices","operation":"add"}. Explicit folder IDs safely
// distinguish moves from classification, even with duplicate display names.
func (s *Server) apiUnifiedMessageLabels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LabelID string `json:"label_id"`
		Name string `json:"name"`
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
	action, problem, err := s.labelAction(r.Context(), p, mb.inbox, in.LabelID, in.Name, in.Operation)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if problem != "" {
		code := http.StatusBadRequest
		if problem == "forbidden" { code = http.StatusForbidden }
		if problem == "label not found" { code = http.StatusNotFound }
		if strings.Contains(problem, "cannot be removed") { code = http.StatusConflict }
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
	action, problem, err := s.labelAction(r.Context(), p, box, r.Form.Get("label_id"), r.Form.Get("label"), r.Form.Get("action"))
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

// Create through the same path as the sidebar. Native and Microsoft use
// additive label names, Gmail creates a real user label, IMAP creates a folder.
func (s *Server) createMailboxLabel(ctx context.Context, p model.Principal, box model.Inbox, name string) (mailLabel, error) {
	clean, valid := model.NormalizeLabel(strings.TrimSpace(name))
	if !valid || clean != strings.TrimSpace(name) {
		return mailLabel{}, model.NewMailboxError(model.ErrKindInvalid, "invalid label name", false, nil)
	}
	if box.Kind == model.InboxKindDomain || s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
		created, err := s.Service.Store.CreateDefinedLabel(ctx, p, box.ID, clean)
		if err != nil { return mailLabel{}, err }
		return mailLabel{ID:"tag:"+created, Name:created, Kind:"tag", Membership:"multiple", Source:"local", CanRemove:true, CanRename:box.Kind==model.InboxKindDomain, CanDelete:box.Kind==model.InboxKindDomain}, nil
	}
	path := clean
	if s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) {
		path = ""
	} else {
		// The default INBOX scope also permits sibling personal folders.
		// Do not accidentally create every new label inside INBOX. A custom
		// scope, however, must place new folders under its configured root.
		root := strings.TrimSpace(box.Namespace)
		if root != "" && !strings.EqualFold(root, model.NamespaceINBOX) &&
			!strings.EqualFold(root, clean) && !strings.HasPrefix(clean, root+"/") {
			path = root + "/" + clean
		}
	}
	folder, err := s.remoteMailbox().CreateRemoteFolder(ctx, p, box.ID, path, clean)
	if err != nil { return mailLabel{}, err }
	if s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) {
		return mailLabel{ID:"tag:"+folder.Name, Name:folder.Name, Kind:"tag", Membership:"multiple", Source:"provider", CanRemove:true, CanRename:true, CanDelete:true, FolderID:folder.ID}, nil
	}
	return mailLabel{ID:"folder:"+folder.ID, Name:folder.Name, Kind:"folder", Membership:"exclusive", Source:"provider", FolderID:folder.ID, CanRename:true, CanDelete:true}, nil
}

func (s *Server) apiMailboxLabelCreate(w http.ResponseWriter, r *http.Request) {
	var in struct { Name string `json:"name"` }
	if !decodeJSON(w, r, &in) { return }
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil { mapMailboxError(w, err); return }
	if !p.CanAssist(mb.inbox.ID) && !p.Admin { writeError(w, 403, "forbidden"); return }
	created, err := s.createMailboxLabel(r.Context(), p, mb.inbox, in.Name)
	if err != nil { mapMailboxError(w, err); return }
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) uiMailboxLabelCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil { mapMailboxError(w, err); return }
	if !p.CanAssist(mb.inbox.ID) && !p.Admin { http.Error(w,"forbidden",403); return }
	if _, err := s.createMailboxLabel(r.Context(), p, mb.inbox, r.Form.Get("name")); err != nil {
		s.uiError(w, err, 400); return
	}
	http.Redirect(w, r, "/ui/inboxes/"+url.PathEscape(mb.inbox.ID)+"?notice="+url.QueryEscape("Label created"), http.StatusSeeOther)
}

// unifiedLabelUnreadCounts provides the same badge data on initial render
// and status refresh, from each mailbox's authoritative cached metadata.
func (s *Server) unifiedLabelUnreadCounts(ctx context.Context, p model.Principal, box model.Inbox) map[string]int {
	if box.Kind == model.InboxKindStandalone {
		counts, err := s.Service.Store.RemoteInboxLabelUnreadCounts(ctx, p.AccountID, box.ID)
		if err == nil {
			return counts
		}
		return map[string]int{}
	}
	counts, _ := s.Service.Store.InboxLabelUnreadCounts(ctx, p, box.ID)
	if counts == nil {
		return map[string]int{}
	}
	return counts
}
