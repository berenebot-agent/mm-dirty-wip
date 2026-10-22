package httpapp

import (
	"context"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// labelFilter is the resolved form of a repeatable label query. Names are
// additive tag matches that AND together; Scope is the backend's first membership
// scope and MembershipIDs retains every requested classification for intersection.
type labelFilter struct {
	Names         []string
	Scope         string
	FolderID      string
	Role          string
	MembershipIDs []string
	// ScopePaths preserves every provider-native membership selector for
	// indexed thread aggregation, including intersected Gmail system labels.
	ScopePaths []string
}

// labelFilterFromQuery reads the repeatable "label" and "label_id" parameters.
// It is the single entry point every list and search handler uses; a raw
// "label" value is still accepted so existing clients keep working, and a
// tag:<Name> or folder:<ID> catalogue id resolves to the same thing.
func (s *Server) labelFilterFromQuery(ctx context.Context, p model.Principal, box model.Inbox, q url.Values) (labelFilter, string) {
	var out labelFilter
	var catalog []mailLabel
	var exclusive string
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
			if catalog == nil {
				var err error
				catalog, err = s.mailboxLabels(ctx, p, box)
				if err != nil {
					return labelFilter{}, "label catalogue unavailable"
				}
			}
			var target *mailLabel
			for i := range catalog {
				if catalog[i].ID == v {
					target = &catalog[i]
					break
				}
			}
			if target == nil {
				return labelFilter{}, "label not found"
			}
			if target.Membership == "exclusive" {
				if exclusive != "" && exclusive != target.ID {
					return labelFilter{}, "only one exclusive label may be requested"
				}
				exclusive = target.ID
			}
			out.MembershipIDs = appendUniqueFold(out.MembershipIDs, target.ID)
			out.ScopePaths = appendUniqueFold(out.ScopePaths, target.FolderPath)
			if out.Scope == "" {
				out.Scope, out.FolderID, out.Role = target.FolderPath, target.FolderID, target.Role
				if box.Kind == model.InboxKindDomain {
					out.Scope = target.FolderID
				}
			}
		default:
			// A bare name is an additive label (matching the action API, where a
			// plain name prefers a tag when a folder shares the display name).
			out.Names = appendUniqueFold(out.Names, v)
		}
	}
	return out, ""
}

func (labels labelFilter) applyLocal(f *store.MessageFilter) {
	if labels.FolderID == "" {
		return
	}
	switch labels.Role {
	case model.FolderRoleInbox:
		f.FolderScoped, f.InboxRole = true, model.FolderRoleInbox
		f.Direction = "inbound"
	case model.FolderRoleSpam:
		f.SpamOnly = true
	case model.FolderRoleTrash:
		f.Trashed, f.IncludeSpam = true, true
	case model.FolderRoleSent:
		f.Direction = "outbound"
		f.SentOnly = true
	default:
		f.FolderScoped, f.MailboxID = true, labels.FolderID
	}
}

func (labels labelFilter) matchesMembership(m model.Message, id inboxIdentity) bool {
	if len(labels.MembershipIDs) == 0 {
		return true
	}
	memberships := labelIDsFor(m, id)
	for _, want := range labels.MembershipIDs {
		if !containsLabel(memberships, want) {
			return false
		}
	}
	return true
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
