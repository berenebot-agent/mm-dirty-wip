package httpapp

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// manageMailboxLabel preserves remote folder identities and refuses unsupported
// provider operations; Graph categories do not have safe folder-rename semantics.
func (s *Server) manageMailboxLabel(r *http.Request, labelID, operation, newName string) error {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil { return err }
	if !p.CanAssist(mb.inbox.ID) && !p.Admin {
		return model.NewMailboxError(model.ErrKindForbidden, "forbidden", false, nil)
	}
	catalog, err := s.mailboxLabels(r.Context(), p, mb.inbox)
	if err != nil { return err }
	var label *mailLabel
	for i := range catalog {
		if catalog[i].ID == labelID { label = &catalog[i]; break }
	}
	if label == nil {
		return model.NewMailboxError(model.ErrKindNotFound, "label not found", false, nil)
	}
	if (operation == "rename" && !label.CanRename) || (operation == "delete" && !label.CanDelete) {
		return model.NewMailboxError(model.ErrKindUnsupported, "this provider does not support renaming or deleting this label", false, nil)
	}
	if operation == "rename" {
		if clean, ok := model.NormalizeLabel(strings.TrimSpace(newName)); !ok || clean != strings.TrimSpace(newName) {
			return model.NewMailboxError(model.ErrKindInvalid, "invalid label name", false, nil)
		}
	}
	if label.FolderID != "" {
		if mb.routed {
			if operation == "rename" {
				_, err = mb.remote.RenameRemoteFolder(r.Context(), p, mb.inbox.ID, label.FolderID, newName)
			} else {
				err = mb.remote.DeleteRemoteFolder(r.Context(), p, mb.inbox.ID, label.FolderID)
			}
		} else {
			if operation == "rename" {
				_, err = s.Service.Store.RenameFolder(r.Context(), p, mb.inbox.ID, label.FolderID, newName)
			} else {
				err = s.Service.Store.DeleteFolder(r.Context(), p, mb.inbox.ID, label.FolderID)
			}
		}
		return err
	}
	events, err := s.Service.Store.ManageDefinedLabel(r.Context(), p, mb.inbox.ID, label.Name, newName, operation)
	if err == nil {
		for _, ev := range events { s.Service.Hub.Publish(ev) }
	}
	return err
}

func (s *Server) apiMailboxLabelItem(w http.ResponseWriter, r *http.Request) {
	var in struct { Name string `json:"name"` }
	operation := "delete"
	if r.Method == http.MethodPatch {
		operation = "rename"
		if !decodeJSON(w,r,&in) { return }
	}
	if err := s.manageMailboxLabel(r, r.PathValue("labelId"), operation, in.Name); err != nil {
		mapMailboxError(w,err); return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) uiMailboxLabelRename(w http.ResponseWriter, r *http.Request) {
	if err := s.manageMailboxLabel(r, r.PathValue("labelId"), "rename", r.Form.Get("name")); err != nil {
		s.uiError(w,err,400); return
	}
	http.Redirect(w,r,"/ui/inboxes/"+url.PathEscape(r.PathValue("id"))+"?notice="+url.QueryEscape("Label renamed"),http.StatusSeeOther)
}

func (s *Server) uiMailboxLabelDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.manageMailboxLabel(r, r.PathValue("labelId"), "delete", ""); err != nil {
		s.uiError(w,err,400); return
	}
	http.Redirect(w,r,"/ui/inboxes/"+url.PathEscape(r.PathValue("id"))+"?notice="+url.QueryEscape("Label deleted"),http.StatusSeeOther)
}
