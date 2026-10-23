package httpapp

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// labelFilter keeps additive names separate from folder placement.
type labelFilter struct {
	Names    []string
	FolderID string
	Role     string
}

// labelFilterFromQuery reads the repeatable "label" and "label_id" parameters.
// It is the single entry point every list and search handler uses; a raw
// "label" value is still accepted so existing clients keep working, and a
// tag:<Name> catalogue id resolves to the same additive classification.
func (s *Server) labelFilterFromQuery(ctx context.Context, p model.Principal, box model.Inbox, q url.Values) (labelFilter, string) {
	var out labelFilter
	raw := append([]string{}, q["label"]...)
	raw = append(raw, q["label_id"]...)
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch {
		case strings.HasPrefix(v, "tag:"):
			out.Names = appendUniqueFold(out.Names, strings.TrimPrefix(v, "tag:"))
		case strings.HasPrefix(v, "folder:"):
			return labelFilter{}, "folders are not labels; use folder= instead"
		default:
			// A bare name is an additive label (matching the action API, where a
			// plain name prefers a tag when a folder shares the display name).
			out.Names = appendUniqueFold(out.Names, v)
		}
	}
	if len(out.Names) > 0 && box.Kind == model.InboxKindStandalone && !s.providerLabelInbox(ctx, p, box) {
		return labelFilter{}, "IMAP does not support additive label filters"
	}
	return out, ""
}

func (labels labelFilter) applyLocal(f *store.MessageFilter) {
	if labels.FolderID == "" {
		return
	}
	switch labels.Role {
	case model.FolderRoleInbox:
		f.FolderScoped, f.MailboxID = true, labels.FolderID
	case model.FolderRoleSpam:
		f.SpamOnly = true
	case model.FolderRoleTrash:
		f.Trashed, f.IncludeSpam = true, true
	case model.FolderRoleSent:
		f.Direction = "outbound"
		f.SentOnly = true
		f.FolderScoped, f.InboxRole = true, model.FolderRoleSent
	default:
		f.FolderScoped, f.MailboxID = true, labels.FolderID
	}
}

func (s *Server) applyLocalFolder(ctx context.Context, p model.Principal, inbox, folder string, f *store.MessageFilter) error {
	if folder == "" {
		return nil
	}
	f.FolderScoped, f.MailboxID = true, folder
	if inbox == "" {
		return nil
	}
	item, err := s.Service.Store.GetFolderByID(ctx, p.AccountID, inbox, folder)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch item.Role {
	case model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleSpam, model.FolderRoleTrash:
		f.FolderScoped, f.MailboxID = false, ""
		(labelFilter{FolderID: item.ID, Role: item.Role}).applyLocal(f)
	default:
		// Legacy domain custom folders and Archive have no canonical location.
		f.MailboxID = "unsupported:" + folder
	}
	return nil
}

// A stable folder ID already identifies its inbox, including on account-wide routes.
func (s *Server) folderInboxScope(ctx context.Context, p model.Principal, inbox, folder string) (string, error) {
	if inbox != "" || folder == "" {
		return inbox, nil
	}
	boxes, err := s.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return "", err
	}
	for _, box := range boxes {
		if _, err := s.Service.Store.GetFolderByID(ctx, p.AccountID, box.ID, folder); err == nil {
			return box.ID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	return "", nil
}

// Unselectable hierarchy nodes are not message classifications.
func labelEligibleFolder(f model.Folder) bool {
	return f.Selectable
}

func (s *Server) labelFolders(ctx context.Context, p model.Principal, box model.Inbox) ([]model.Folder, error) {
	folders, err := s.Service.Store.ListFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		return nil, err
	}
	if box.Kind == model.InboxKindDomain {
		roles := map[string]bool{}
		for _, f := range folders {
			roles[f.Role] = true
		}
		if !roles[model.FolderRoleInbox] || !roles[model.FolderRoleSent] || !roles[model.FolderRoleSpam] || !roles[model.FolderRoleTrash] || !roles[model.FolderRoleArchive] {
			folders, err = s.Service.Store.EnsureSystemFolders(ctx, p.AccountID, box.ID)
			if err != nil {
				return nil, err
			}
		}
		out := make([]model.Folder, 0, len(folders))
		for _, f := range folders {
			// Local drafts and outbox jobs have separate APIs, not Message membership.
			if f.Role != model.FolderRoleDrafts && f.Role != model.FolderRoleOutbox {
				out = append(out, f)
			}
		}
		return out, nil
	}
	out := make([]model.Folder, 0, len(folders))
	for _, f := range folders {
		if f.IsSystem && f.Origin != "remote" && !f.RoleLocked {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func appendUniqueFold(list []string, value string) []string {
	for _, existing := range list {
		if strings.EqualFold(existing, value) {
			return list
		}
	}
	return append(list, value)
}
