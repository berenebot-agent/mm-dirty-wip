package httpapp

import (
	"context"
	"errors"
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
	CanAdd     bool   `json:"can_add"`
	Role       string `json:"role,omitempty"`
	CanRename  bool   `json:"can_rename"`
	CanDelete  bool   `json:"can_delete"`
}

// mailboxLabelCapabilities describes what an inbox supports before it has any
// labels. Without it an empty catalogue tells a client nothing, and it would have
// to branch on the mailbox backend to know whether creating a label is additive
// or a move.
type mailboxLabelCapabilities struct {
	CanCreate        bool   `json:"can_create"`
	CreateKind       string `json:"create_kind"`
	CreateMembership string `json:"create_membership"`
	CanRename        bool   `json:"can_rename"`
	CanDelete        bool   `json:"can_delete"`
}

// labelCapabilities mirrors createMailboxLabel's provider rules, so the declared
// capability and the actual create behaviour cannot disagree.
func (s *Server) labelCapabilities(ctx context.Context, p model.Principal, box model.Inbox) mailboxLabelCapabilities {
	switch {
	case box.Kind == model.InboxKindDomain:
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "tag", CreateMembership: "multiple", CanRename: true, CanDelete: true}
	case s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID):
		// Graph categories are additive; category rename/delete are not exposed.
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "tag", CreateMembership: "multiple"}
	case s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID):
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "tag", CreateMembership: "multiple", CanRename: true, CanDelete: true}
	default:
		// Raw IMAP: a label is a remote folder, so creation is a move destination.
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "folder", CreateMembership: "exclusive", CanRename: true, CanDelete: true}
	}
}

// newMailLabel projects one catalogue entry through a single helper so a create
// response and the subsequent catalogue listing can never advertise different
// descriptors for the same label.
func newMailLabel(name, kind, membership, source, folderID string, canRename, canDelete bool, url string) mailLabel {
	return mailLabel{
		ID: labelIDFor(kind, name, folderID), Name: name, Kind: kind, Membership: membership,
		Source: source, FolderID: folderID, URL: url, CanAdd: true, CanRemove: kind == "tag",
		CanRename: canRename, CanDelete: canDelete,
	}
}

// labelIDFor is the single definition of a catalogue identity: additive labels
// are name-addressed, folder-backed labels are id-addressed.
func labelIDFor(kind, name, folderID string) string {
	if kind == "folder" {
		return "folder:" + folderID
	}
	return "tag:" + name
}

func (s *Server) mailboxLabels(ctx context.Context, p model.Principal, box model.Inbox) ([]mailLabel, error) {
	folders, err := s.labelFolders(ctx, p, box)
	if err != nil {
		return nil, err
	}
	google := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID)
	microsoft := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID)
	prefix := "/ui/inboxes/" + url.PathEscape(box.ID)
	out := make([]mailLabel, 0, len(folders))
	seen := map[string]bool{}
	for _, folder := range folders {
		if !labelEligibleFolder(folder) {
			continue
		}
		url := prefix + "/folder?folder=" + url.QueryEscape(folder.ID)
		entry := newMailLabel(folder.Name, "folder", "exclusive", "local", folder.ID, true, true, url)
		entry.FolderPath = folder.Path
		if folder.IsSystem || (folder.Role != model.FolderRoleFolder && folder.Role != model.FolderRoleLabel) {
			entry.Role = folder.Role
			entry.CanRename, entry.CanDelete = false, false
			entry.CanAdd = folder.Role == model.FolderRoleInbox || folder.Role == model.FolderRoleSpam || folder.Role == model.FolderRoleTrash || folder.Role == model.FolderRoleArchive
		}
		if box.Kind == model.InboxKindStandalone {
			entry.Source = "provider"
		}
		if entry.FolderID == "" {
			entry.FolderID = folder.ID
		}
		if google && entry.Role == "" {
			// The Gmail custom-label index also appears as remote folders.
			// Project it once, as a multi-valued label, with a folder-indexed
			// view (Google stores membership by provider label ID).
			entry = newMailLabel(folder.Name, "tag", "multiple", "provider", folder.ID, true, true, url)
			entry.FolderPath = folder.Path
			seen[strings.ToLower(folder.Name)] = true
		}
		if google && entry.Role != "" {
			entry.Membership = "multiple"
			entry.CanRemove = entry.CanAdd && entry.Role != model.FolderRoleArchive
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
		out = append(out, newMailLabel(name, "tag", "multiple", source, "", editable, editable, prefix+"/label?name="+url.QueryEscape(name)))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].ID < out[j].ID
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (s *Server) selectedMailboxLabels(ctx context.Context, p model.Principal, box model.Inbox, labels []mailLabel, m model.Message) []mailLabel {
	id := s.labelIdentity(ctx, p, box, []model.Message{m})
	memberships := labelIDsFor(m, id)
	for i := range labels {
		for _, applied := range memberships {
			if applied == labels[i].ID {
				labels[i].Selected = true
				break
			}
		}
	}
	return labels
}

// System views already have dedicated sidebar links.
func sidebarLabels(labels []mailLabel) []mailLabel {
	out := make([]mailLabel, 0, len(labels))
	for _, label := range labels {
		if label.Role == "" {
			out = append(out, label)
		}
	}
	return out
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
		// A name from the catalogue is accepted verbatim: a provider may allow a
		// hierarchical name that the MailMoose-native rule rejects.
		provider := s.providerLabelInbox(ctx, p, box)
		for i := range catalog {
			if strings.HasPrefix(labelID, "tag:") && catalog[i].Kind != "tag" {
				continue
			}
			if strings.EqualFold(catalog[i].Name, name) {
				// Tag wins if Outlook also has a folder with the same name.
				if target == nil || catalog[i].Kind == "tag" {
					target = &catalog[i]
				}
			}
		}
		if target == nil {
			if provider {
				clean, valid := model.NormalizeProviderLabel(name)
				if !valid {
					return store.MessageActionInput{}, "invalid label name", nil
				}
				name = clean
			} else {
				clean, valid := model.NormalizeLabel(name)
				if !valid || clean != name {
					return store.MessageActionInput{}, "invalid label name", nil
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
	if operation == "add" && target.ID != "" && !target.CanAdd {
		return store.MessageActionInput{}, "label cannot be applied; membership is managed by the mailbox", nil
	}
	if target.Role != "" && target.Role != model.FolderRoleArchive && s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) && target.Membership == "multiple" {
		if operation == "remove" && !target.CanRemove {
			return store.MessageActionInput{}, "label cannot be removed", nil
		}
		return store.MessageActionInput{Kind: "label", Destination: target.FolderPath, Label: target.Name, LabelOp: operation, ProviderLabel: true}, "", nil
	}
	if target.Kind == "folder" {
		if operation == "remove" {
			return store.MessageActionInput{}, "exclusive labels cannot be removed; apply a replacement label instead", nil
		}
		if target.Role != "" && box.Kind == model.InboxKindDomain {
			return store.MessageActionInput{Kind: "classify", Destination: target.FolderID}, "", nil
		}
		destination := target.FolderID
		if box.Kind == model.InboxKindStandalone && !s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
			destination = target.FolderPath
		}
		return store.MessageActionInput{Kind: "move", Destination: destination}, "", nil
	}
	return store.MessageActionInput{Kind: "label", Label: target.Name, LabelOp: operation, ProviderLabel: s.providerLabelInbox(ctx, p, box)}, "", nil
}

// providerLabelInbox reports whether an inbox's additive labels are owned by the
// provider (Gmail user labels, Outlook categories), where names may be
// hierarchical, rather than local MailMoose metadata.
func (s *Server) providerLabelInbox(ctx context.Context, p model.Principal, box model.Inbox) bool {
	if box.Kind != model.InboxKindStandalone {
		return false
	}
	return s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) || s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID)
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
	resp := mailboxLabelCatalogResponse{Items: labels, Completeness: model.CompletenessComplete, Capabilities: s.labelCapabilities(r.Context(), p, mb.inbox)}
	writeJSON(w, http.StatusOK, resp)
}

// mailboxLabelCatalogResponse is the common listing envelope plus the inbox's
// label capabilities, so an empty catalogue still describes what the inbox
// supports.
type mailboxLabelCatalogResponse struct {
	Items        []mailLabel              `json:"items"`
	NextCursor   string                   `json:"next_cursor,omitempty"`
	Completeness model.ListCompleteness   `json:"completeness"`
	Errors       []model.InboxFailure     `json:"errors,omitempty"`
	Capabilities mailboxLabelCapabilities `json:"capabilities"`
}

// POST /v1/inboxes/{id}/messages/{messageId}/labels
// {"name":"Invoices","operation":"add"} is equivalent to
// {"label_id":"tag:Invoices","operation":"add"}. Explicit folder IDs safely
// distinguish moves from classification, even with duplicate display names.
func (s *Server) apiUnifiedMessageLabels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LabelID   string `json:"label_id"`
		Name      string `json:"name"`
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
		if problem == "forbidden" {
			code = http.StatusForbidden
		}
		if strings.Contains(problem, "label not found") {
			code = http.StatusNotFound
		}
		if strings.Contains(problem, "cannot be removed") {
			code = http.StatusConflict
		}
		writeError(w, code, problem)
		return
	}
	s.submitMessageAction(w, r, m.ID, action, false)
}

// POST /v1/inboxes/{id}/messages/{messageId}/move
// {"folder_id":"..."} moves a message to a folder in the same inbox.
func (s *Server) apiMoveMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FolderID string `json:"folder_id"`
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
	folder, err := s.Service.Store.GetFolderByID(r.Context(), p.AccountID, mb.inbox.ID, strings.TrimSpace(in.FolderID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "folder not found")
		} else {
			mapStoreError(w, err)
		}
		return
	}
	destination := folder.ID
	if mb.inbox.Kind == model.InboxKindStandalone && !s.remoteMailbox().IsMicrosoft(r.Context(), p.AccountID, mb.inbox.ID) {
		destination = folder.Path
	}
	s.submitMessageAction(w, r, m.ID, store.MessageActionInput{Kind: "move", Destination: destination}, false)
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
		if err != nil {
			return mailLabel{}, err
		}
		return newMailLabel(created, "tag", "multiple", "local", "", box.Kind == model.InboxKindDomain, box.Kind == model.InboxKindDomain, ""), nil
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
	if err != nil {
		return mailLabel{}, err
	}
	if s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID) {
		return newMailLabel(folder.Name, "tag", "multiple", "provider", folder.ID, true, true, ""), nil
	}
	return newMailLabel(folder.Name, "folder", "exclusive", "provider", folder.ID, true, true, ""), nil
}

func (s *Server) apiMailboxLabelCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
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
	if !p.CanAssist(mb.inbox.ID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	created, err := s.createMailboxLabel(r.Context(), p, mb.inbox, in.Name)
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) uiMailboxLabelCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	if !p.CanAssist(mb.inbox.ID) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	if _, err := s.createMailboxLabel(r.Context(), p, mb.inbox, r.Form.Get("name")); err != nil {
		s.uiError(w, err, 400)
		return
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
