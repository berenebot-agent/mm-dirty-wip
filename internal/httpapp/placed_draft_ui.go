package httpapp

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// A placed draft is a draft that already lives in a connected mailbox's Drafts
// folder. The UI treats it as an ordinary draft: the same editor, the same save
// route and the same delete button. These helpers resolve a draft id that is not
// in Mammoth Mail's own drafts table against that folder, so the routes do not
// need a second set of paths or a second look.

// placedDraftRecord resolves a cached remote row that really is in this inbox's
// Drafts folder. It reads metadata only, never the body.
func (s *Server) placedDraftRecord(ctx context.Context, p model.Principal, box model.Inbox, id string) (store.RemoteMessage, bool) {
	if strings.TrimSpace(id) == "" || s.remoteMailbox() == nil {
		return store.RemoteMessage{}, false
	}
	folder, ok := s.remoteDraftsFolder(ctx, p, box)
	if !ok {
		return store.RemoteMessage{}, false
	}
	rec, err := s.Service.Store.GetRemoteMessage(ctx, p.AccountID, box.ID, id)
	if err != nil || rec.FolderPath != folder.Path {
		return store.RemoteMessage{}, false
	}
	return rec, true
}

// renderPlacedDraftEditor renders a placed draft in the same compose page the
// local drafts use, pointing the form at the same save route. The body is fetched
// live from the connected mailbox; the marker travels with the form so a save can
// tell whether the draft changed elsewhere in the meantime.
func (s *Server) renderPlacedDraftEditor(w http.ResponseWriter, r *http.Request, p model.Principal, box model.Inbox, draftID string) bool {
	if _, ok := s.placedDraftRecord(r.Context(), p, box, draftID); !ok {
		return false
	}
	pd, err := s.remoteMailbox().LoadPlacedDraft(r.Context(), p, box.ID, draftID)
	if err != nil {
		s.uiError(w, err, http.StatusBadGateway)
		return true
	}
	note := "This draft lives in the connected mailbox's Drafts folder, so a save writes it there."
	if len(pd.Attachments) > 0 {
		note += " Attachments (unchanged by an edit here): " + strings.Join(pd.Attachments, ", ")
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	fromOptions, from := composeFromOptions(box, pd.Fields.FromAddress)
	s.render(w, r, composeBody, pageData{
		Title:              "Edit Draft",
		Principal:          p,
		CSRF:               csrf(r),
		Account:            acc,
		Inbox:              &box,
		Folder:             "drafts",
		ComposeTitle:       "Edit Draft",
		ComposeAction:      actionWithCSRF("/ui/inboxes/"+box.ID+"/drafts/"+url.PathEscape(pd.ID)+"/save", csrf(r)),
		ComposeCancel:      "/ui/inboxes/" + box.ID + "/drafts",
		ComposeTo:          strings.Join(pd.Fields.To, ", "),
		ComposeCC:          strings.Join(pd.Fields.CC, ", "),
		ComposeBCC:         strings.Join(pd.Fields.BCC, ", "),
		ComposeSubject:     pd.Fields.Subject,
		ComposeText:        pd.Fields.Text,
		ComposeNote:        note,
		ComposeDraftID:     pd.ID,
		ComposeMarker:      pd.Marker,
		ComposePlaced:      true,
		ComposeFrom:        from,
		ComposeFromOptions: fromOptions,
		Notice:             r.URL.Query().Get("notice"),
	})
	return true
}

// savePlacedDraftFromForm writes an edited placed draft back through the
// connected mailbox. handled is false when the id is not a placed draft, so the
// caller keeps its local-draft behaviour.
func (s *Server) savePlacedDraftFromForm(w http.ResponseWriter, r *http.Request, p model.Principal, box model.Inbox, draftID string, in app.SendInput, action string) bool {
	if _, ok := s.placedDraftRecord(r.Context(), p, box, draftID); !ok {
		return false
	}
	if action == "send" || action == "request-send" {
		// Sending is the operator's own client's job for a placed draft: Mammoth
		// Mail never sends one, so the editor cannot offer it.
		http.Error(w, "this draft is sent from the connected mailbox, not from here", http.StatusConflict)
		return true
	}
	current, err := s.remoteMailbox().LoadPlacedDraft(r.Context(), p, box.ID, draftID)
	if err != nil {
		s.uiError(w, err, http.StatusBadGateway)
		return true
	}
	fields := app.PlacedDraftFields{
		To: in.To, CC: in.CC, BCC: in.BCC,
		Subject: in.Subject, Text: in.Text,
		FromAddress: in.FromAddress,
	}
	// The compose page edits text only. An HTML alternative the draft already
	// carries is kept rather than dropped by an unrelated subject edit.
	fields.HTML = current.Fields.HTML
	if strings.TrimSpace(in.HTML) != "" {
		fields.HTML = in.HTML
	}
	if strings.TrimSpace(fields.FromAddress) == "" {
		fields.FromAddress = current.Fields.FromAddress
		fields.FromName = current.Fields.FromName
	}
	marker := strings.TrimSpace(r.Form.Get("draft_marker"))
	if _, err = s.remoteMailbox().SavePlacedDraft(r.Context(), p, box.ID, draftID, fields, marker); err != nil {
		if err == app.ErrPlacedDraftChanged {
			http.Error(w, "This draft changed in the connected mailbox since you opened it. Reload it and apply your edit again.", http.StatusConflict)
			return true
		}
		s.uiError(w, err, http.StatusBadGateway)
		return true
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Draft+saved", http.StatusSeeOther)
	return true
}

// deletePlacedDraft queues the provider delete for a placed draft. The action
// queue already owns remote deletes, so the row is removed by the same worker
// that removes any other message.
func (s *Server) deletePlacedDraft(w http.ResponseWriter, r *http.Request, p model.Principal, box model.Inbox, draftID string) bool {
	if _, ok := s.placedDraftRecord(r.Context(), p, box, draftID); !ok {
		return false
	}
	if _, err := s.Service.EnqueueInboxAction(r.Context(), p, box.ID, draftID, store.MessageActionInput{Kind: "delete"}); err != nil {
		s.uiError(w, err, http.StatusBadRequest)
		return true
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Draft+deleted", http.StatusSeeOther)
	return true
}
