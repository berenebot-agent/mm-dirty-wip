package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// mailLabel is an additive classification, independent of folder location.
type mailLabel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Membership string `json:"membership"`
	Source     string `json:"source"`
	FolderID   string `json:"-"`
	URL        string `json:"-"`
	Selected   bool   `json:"-"`
	CanRemove  bool   `json:"can_remove"`
	CanAdd     bool   `json:"can_add"`
	CanRename  bool   `json:"can_rename"`
	CanDelete  bool   `json:"can_delete"`
}

// mailboxLabelCapabilities describes what an inbox supports before it has any
// labels. Even an empty catalogue declares supported additive operations.
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
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "tag", CreateMembership: "multiple", CanRename: true, CanDelete: true}
	case s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID):
		return mailboxLabelCapabilities{CanCreate: true, CreateKind: "tag", CreateMembership: "multiple", CanRename: true, CanDelete: true}
	default:
		return mailboxLabelCapabilities{}
	}
}

// newMailLabel projects one catalogue entry through a single helper so a create
// response and the subsequent catalogue listing can never advertise different
// descriptors for the same label.
func newMailLabel(name, source, folderID string, canRename, canDelete bool, url string) mailLabel {
	return mailLabel{
		ID: "tag:" + name, Name: name, Kind: "tag", Membership: "multiple",
		Source: source, FolderID: folderID, URL: url, CanAdd: true, CanRemove: true,
		CanRename: canRename, CanDelete: canDelete,
	}
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
		if !google || !labelEligibleFolder(folder) || (folder.Role != model.FolderRoleFolder && folder.Role != model.FolderRoleLabel) || folder.IsSystem {
			continue
		}
		labelURL := prefix + "/label?name=" + url.QueryEscape(folder.Name)
		entry := newMailLabel(folder.Name, "provider", folder.ID, true, true, labelURL)
		seen[strings.ToLower(folder.Name)] = true
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
		// A provider category is edited on the provider, which is why it is
		// editable too; a raw IMAP tag is neither.
		editable := box.Kind == model.InboxKindDomain || microsoft || google
		if microsoft || google {
			source = "provider"
		}
		out = append(out, newMailLabel(name, source, "", editable, editable, prefix+"/label?name="+url.QueryEscape(name)))
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

func sidebarLabels(labels []mailLabel) []mailLabel {
	return labels
}

// labelAction translates free-text or explicit IDs to a provider-aware action.
// A free-text name matches an existing label first; an unrecognised name becomes
// an additive label only on providers supporting several labels per message.
// Folder identities are never accepted as labels.
func (s *Server) labelAction(ctx context.Context, p model.Principal, box model.Inbox, labelID, name, operation string) (store.MessageActionInput, string, error) {
	if operation != "add" && operation != "remove" {
		return store.MessageActionInput{}, "operation must be add or remove", nil
	}
	if !p.CanAssist(box.ID) && !p.Admin {
		return store.MessageActionInput{}, "forbidden", nil
	}
	if box.Kind == model.InboxKindStandalone && !s.providerLabelInbox(ctx, p, box) {
		return store.MessageActionInput{}, "", model.NewMailboxError(model.ErrKindUnsupported, "IMAP does not support additive labels; use folders and move", false, nil)
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
		// hierarchical name that the MammothMail-native rule rejects.
		provider := s.providerLabelInbox(ctx, p, box)
		for i := range catalog {
			if strings.EqualFold(catalog[i].Name, name) {
				target = &catalog[i]
				break
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
			return store.MessageActionInput{}, "label not found", nil
		}
		target = &mailLabel{Name: name, Kind: "tag", Membership: "multiple"}
	}
	if operation == "add" && target.ID != "" && !target.CanAdd {
		return store.MessageActionInput{}, "label cannot be applied; membership is managed by the mailbox", nil
	}
	return store.MessageActionInput{Kind: "label", Label: target.Name, LabelOp: operation, ProviderLabel: s.providerLabelInbox(ctx, p, box)}, "", nil
}

// providerLabelInbox reports whether an inbox's additive labels are owned by the
// provider (Gmail user labels, Outlook categories), where names may be
// hierarchical, rather than local Mammoth Mail metadata.
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
// {"label_id":"tag:Invoices","operation":"add"}.
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
		mapMailboxError(w, err)
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
		FolderID json.RawMessage `json:"folder_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.FolderID) == 0 {
		writeError(w, http.StatusBadRequest, "folder_id is required; use null to archive where supported")
		return
	}
	var folderID *string
	if err := json.Unmarshal(in.FolderID, &folderID); err != nil || (folderID != nil && strings.TrimSpace(*folderID) == "") {
		writeError(w, http.StatusBadRequest, "folder_id must be a nonempty folder ID or null")
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
	action, err := s.folderMoveAction(r.Context(), p, mb.inbox, folderID)
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	s.submitMessageAction(w, r, m.ID, action, false)
}

// folderMoveAction is shared by API and UI; labels never enter this path.
func (s *Server) folderMoveAction(ctx context.Context, p model.Principal, box model.Inbox, folderID *string) (store.MessageActionInput, error) {
	google := box.Kind == model.InboxKindStandalone && s.remoteMailbox().IsGoogle(ctx, p.AccountID, box.ID)
	if folderID == nil {
		if box.Kind == model.InboxKindDomain {
			if _, err := s.labelFolders(ctx, p, box); err != nil {
				return store.MessageActionInput{}, err
			}
			archive, err := s.Service.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleArchive)
			return store.MessageActionInput{Kind: "classify", Destination: archive.ID}, err
		}
		if google {
			return store.MessageActionInput{Kind: "move", Destination: "ARCHIVE"}, nil
		}
		return store.MessageActionInput{}, model.NewMailboxError(model.ErrKindUnsupported, "this provider requires a physical destination folder", false, nil)
	}
	folder, err := s.Service.Store.GetFolderByID(ctx, p.AccountID, box.ID, strings.TrimSpace(*folderID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MessageActionInput{}, model.NewMailboxError(model.ErrKindNotFound, "folder not found", false, nil)
		}
		return store.MessageActionInput{}, err
	}
	if !folder.Selectable {
		return store.MessageActionInput{}, model.NewMailboxError(model.ErrKindInvalid, "folder is not selectable", false, nil)
	}
	if folder.Role == model.FolderRoleSent || folder.Role == model.FolderRoleDrafts || folder.Role == model.FolderRoleOutbox {
		return store.MessageActionInput{}, model.NewMailboxError(model.ErrKindUnsupported, "destination is provider-managed", false, nil)
	}
	if box.Kind == model.InboxKindDomain || google {
		switch folder.Role {
		case model.FolderRoleInbox, model.FolderRoleSpam, model.FolderRoleTrash, model.FolderRoleArchive:
		default:
			return store.MessageActionInput{}, model.NewMailboxError(model.ErrKindUnsupported, "this inbox supports system folders only", false, nil)
		}
	}
	if box.Kind == model.InboxKindDomain {
		return store.MessageActionInput{Kind: "classify", Destination: folder.ID}, nil
	}
	destination := folder.ID
	if !s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
		destination = folder.Path
	}
	return store.MessageActionInput{Kind: "move", Destination: destination}, nil
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

// Create through the same path as the sidebar, independently of folder placement.
func (s *Server) createMailboxLabel(ctx context.Context, p model.Principal, box model.Inbox, name string) (mailLabel, error) {
	if box.Kind == model.InboxKindStandalone && !s.providerLabelInbox(ctx, p, box) {
		return mailLabel{}, model.NewMailboxError(model.ErrKindUnsupported, "IMAP does not support additive labels; use folders", false, nil)
	}
	clean, valid := model.NormalizeLabel(strings.TrimSpace(name))
	if s.providerLabelInbox(ctx, p, box) {
		clean, valid = model.NormalizeProviderLabel(strings.TrimSpace(name))
	}
	if !valid || clean != strings.TrimSpace(name) {
		return mailLabel{}, model.NewMailboxError(model.ErrKindInvalid, "invalid label name", false, nil)
	}
	if s.remoteMailbox().IsMicrosoft(ctx, p.AccountID, box.ID) {
		created, err := s.remoteMailbox().CreateMicrosoftCategory(ctx, p, box.ID, clean)
		return newMailLabel(created, "provider", "", false, false, ""), err
	}
	if box.Kind == model.InboxKindDomain {
		created, err := s.Service.Store.CreateDefinedLabel(ctx, p, box.ID, clean)
		if err != nil {
			return mailLabel{}, err
		}
		return newMailLabel(created, "local", "", true, true, ""), nil
	}
	folder, err := s.remoteMailbox().CreateRemoteFolder(ctx, p, box.ID, "", clean)
	if err != nil {
		return mailLabel{}, err
	}
	return newMailLabel(folder.Name, "provider", folder.ID, true, true, ""), nil
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
