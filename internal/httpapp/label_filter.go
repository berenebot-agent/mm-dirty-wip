package httpapp

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
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
		// The folder stays the location and the role narrows it: an Inbox-role
		// view is the Inbox bucket, not every inbound message in the mailbox.
		f.FolderScoped, f.MailboxID, f.InboxRole = true, labels.FolderID, model.FolderRoleInbox
	case model.FolderRoleSpam:
		// Spam membership is the message's own classification (is_spam), not a
		// placement: marking mail Spam does not file it, so an exact placement
		// match would empty the folder. Apply the same rule the role shorthand
		// and the reader use.
		f.SpamOnly = true
	case model.FolderRoleTrash:
		// The Trash folder's membership is deleted_at set; the folder selection
		// alone cannot express it, so the visibility rule is applied on that
		// folder (where spam keeps its mail visible, as the Trash view does).
		f.Trashed, f.IncludeSpam = true, true
	case model.FolderRoleSent:
		// The Sent folder's membership is completed outbound mail, not a
		// placement: sends completed before explicit filing live in the implicit
		// mailbox_id NULL bucket, so the clause widens to the role like the
		// Inbox view does.
		f.Direction, f.SentOnly = "outbound", true
		f.FolderScoped, f.InboxRole = true, model.FolderRoleSent
	default:
		f.FolderScoped, f.MailboxID = true, labels.FolderID
	}
}

// applyLocalFolder scopes a domain read to one canonical folder. An explicit
// folder always stays the location: a role filter then narrows it, so
// folder=Inbox&trashed=true selects nothing and folder=Sent&direction=outbound
// lists Sent — the meaning every other inbox kind gives those filters. The bare
// role shorthand (trashed, spam or direction with no folder) keeps its
// system-folder mapping in labelFilter.
func (s *Server) applyLocalFolder(ctx context.Context, p model.Principal, inbox, folder string, f *store.MessageFilter) error {
	if folder == "" {
		return nil
	}
	f.FolderScoped, f.MailboxID = true, folder
	if inbox == "" {
		return nil
	}
	// A legacy path form must reach the same role mapping as the id form, so
	// the lookup accepts either (ids are unique and are never a valid path).
	item, err := s.Service.Store.GetFolderDestination(ctx, p.AccountID, inbox, folder)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// The filter binds a folder id, never the caller's raw selector: a legacy
	// path resolves to its folder here, so the path and id forms select the same
	// mail instead of the path form silently selecting nothing.
	f.MailboxID = item.ID
	switch item.Role {
	case model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleSpam, model.FolderRoleTrash:
		// The folder is the location and its role is its membership rule, exactly
		// as the role shorthand applies it: Spam is spam-flagged mail, Trash is
		// trashed mail, Sent is completed outbound. A role filter the folder does
		// not carry (trashed on Inbox, outbound on Inbox) selects nothing rather
		// than another folder, so the answer matches every other inbox kind.
		matches := (!f.Trashed || item.Role == model.FolderRoleTrash) &&
			(!f.SpamOnly || item.Role == model.FolderRoleSpam) &&
			(f.Direction != "outbound" || item.Role == model.FolderRoleSent)
		if !matches {
			f.MailboxID = "unsupported:" + item.ID
			return nil
		}
		f.Direction, f.SentOnly, f.SpamOnly = "", false, false
		switch item.Role {
		case model.FolderRoleSpam:
			f.FolderScoped, f.MailboxID = false, ""
			f.SpamOnly = true
		case model.FolderRoleTrash:
			f.FolderScoped, f.MailboxID = false, ""
			f.Trashed, f.IncludeSpam = true, true
		case model.FolderRoleSent:
			// The Sent folder's membership is completed outbound mail, not a
			// folder placement: sends completed before explicit filing live in
			// the implicit mailbox_id NULL bucket, so the clause widens to the
			// role rather than matching the exact id.
			f.FolderScoped, f.MailboxID, f.InboxRole = true, "", model.FolderRoleSent
			f.Direction, f.SentOnly = "outbound", true
		}
	default:
		// Legacy domain custom folders and Archive have no canonical location.
		f.MailboxID = "unsupported:" + item.ID
	}
	return nil
}

// A stable folder ID or a legacy path already identifies its inbox, including
// on account-wide routes. A path is ambiguous when several inboxes hold a
// folder of that name (every mailbox has a "Spam"), so only a unique match
// scopes the query; anything else keeps the caller's unscoped value.
func (s *Server) folderInboxScope(ctx context.Context, p model.Principal, inbox, folder string) (string, error) {
	if inbox != "" || folder == "" {
		return inbox, nil
	}
	boxes, err := s.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return "", err
	}
	matches := 0
	owner := ""
	for _, box := range boxes {
		if _, err := s.Service.Store.GetFolderDestination(ctx, p.AccountID, box.ID, folder); err == nil {
			matches++
			owner = box.ID
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	if matches == 1 {
		return owner, nil
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
