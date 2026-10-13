package httpapp

import (
	"context"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// labelFilter is the resolved form of a repeatable label query. Names are
// additive tag matches that AND together; Scope is a single exclusive
// folder-backed destination, which is single-valued because a message has one
// folder membership. Every message/search route resolves through this one type
// so an inbox-scoped and an account-wide query cannot disagree.
type labelFilter struct {
	Names []string
	Scope string
}

// labelFilterFromQuery reads the repeatable "label" and "label_id" parameters.
// It is the single entry point every list and search handler uses; a raw
// "label" value is still accepted so existing clients keep working, and a
// tag:<Name> or folder:<ID> catalogue id resolves to the same thing.
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
			id := strings.TrimPrefix(v, "folder:")
			if out.Scope != "" && !strings.EqualFold(out.Scope, id) {
				return labelFilter{}, "a message has one folder-backed label; only one folder: id may be requested"
			}
			out.Scope = id
		default:
			// A bare name is an additive label (matching the action API, where a
			// plain name prefers a tag when a folder shares the display name).
			out.Names = appendUniqueFold(out.Names, v)
		}
	}
	if out.Scope != "" {
		if !s.folderScopeBelongsTo(ctx, p, box, out.Scope) {
			return labelFilter{}, "label not found"
		}
	}
	return out, ""
}

// folderScopeBelongsTo confirms an explicit folder: id names a folder of this
// inbox and is a label-eligible destination, so a cross-inbox id cannot be used
// to widen a scoped query. The accepted value is the same "folder id" the
// catalogue advertises; a remote folder path is accepted only when it resolves
// to such a folder in the same inbox.
func (s *Server) folderScopeBelongsTo(ctx context.Context, p model.Principal, box model.Inbox, folderID string) bool {
	folders, err := s.Service.Store.ListFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		return false
	}
	for _, f := range folders {
		if (f.ID == folderID || f.Path == folderID) && labelEligibleFolder(f) {
			return true
		}
	}
	return false
}

// labelEligibleFolder reports whether a folder is exposed as a label. System
// destinations and unselectable folders are deliberately excluded.
func labelEligibleFolder(f model.Folder) bool {
	return !f.IsSystem && f.Selectable && (f.Role == model.FolderRoleFolder || f.Role == model.FolderRoleLabel)
}

func appendUniqueFold(list []string, value string) []string {
	for _, existing := range list {
		if strings.EqualFold(existing, value) {
			return list
		}
	}
	return append(list, value)
}
