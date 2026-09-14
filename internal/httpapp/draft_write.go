package httpapp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
)

// draftWriteInput is the JSON body shared by the draft write endpoints
// (POST /v1/drafts, PATCH /v1/drafts/{id}, POST /v1/drafts/{id}/send and
// POST /v1/drafts/{id}/request-send). It mirrors the send/reply attachment
// shape (filename, content_type, base64 content) so an agent can create a
// draft, submit it for approval, or send it in a single request.
type draftWriteInput struct {
	// ID, FromName and Status are accepted (and ignored on create) for
	// compatibility with the former model.Draft request body, which these
	// endpoints decoded before. DisallowUnknownFields would otherwise reject
	// clients that still send them.
	ID               string `json:"id,omitempty"`
	Status           string `json:"status,omitempty"`
	FromName         string `json:"from_name,omitempty"`
	InboxID          string `json:"inbox_id,omitempty"`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	FromAddress      string `json:"from_address,omitempty"`
	// Sender is an alias for FromAddress on the draft endpoints, matching the
	// send/reply field name for the same concept.
	Sender      string               `json:"sender,omitempty"`
	To          *[]string            `json:"to,omitempty"`
	CC          *[]string            `json:"cc,omitempty"`
	BCC         *[]string            `json:"bcc,omitempty"`
	Subject     *string              `json:"subject,omitempty"`
	Text        *string              `json:"text,omitempty"`
	HTML        *string              `json:"html,omitempty"`
	Attachments []app.SendAttachment `json:"attachments,omitempty"`
	// Action selects what happens after the draft is written: "draft" (default)
	// leaves it as a draft, "request-send" submits it for approval (Assistant),
	// and "send" sends it immediately (Owner, only on the send endpoint).
	Action string `json:"action,omitempty"`
	// External is accepted by request-send for compatibility; a configured
	// inbox approver makes the request external automatically.
	External bool `json:"external,omitempty"`
}

// decodeDraftWrite reads and validates a draft write body. A missing body is
// valid and yields the zero value, so bodyless POST /send and request-send keep
// working.
func (s *Server) decodeDraftWrite(w http.ResponseWriter, r *http.Request) (draftWriteInput, bool) {
	var in draftWriteInput
	if r.Body == nil || r.ContentLength == 0 {
		return in, true
	}
	if !decodeJSONLimit(w, r, &in, s.Service.Config.MaxMessageBytes*2) {
		return in, false
	}
	return in, true
}

// draftFromInput builds a full draft from a create body (a create, not a
// partial patch). Attachments are persisted separately.
func draftFromInput(in draftWriteInput) model.Draft {
	from := in.FromAddress
	if from == "" {
		from = in.Sender
	}
	return model.Draft{
		InboxID:          in.InboxID,
		ReplyToMessageID: in.ReplyToMessageID,
		FromAddress:      from,
		To:               derefStrings(in.To),
		CC:               derefStrings(in.CC),
		BCC:              derefStrings(in.BCC),
		Subject:          derefString(in.Subject),
		Text:             derefString(in.Text),
		HTML:             derefString(in.HTML),
	}
}

// patchDraft applies only the fields present in a PATCH body to an existing
// draft, so an omitted field is left unchanged rather than wiped.
func patchDraft(in draftWriteInput, old model.Draft) model.Draft {
	d := old
	if in.InboxID != "" {
		d.InboxID = in.InboxID
	}
	if in.ReplyToMessageID != "" {
		d.ReplyToMessageID = in.ReplyToMessageID
	}
	// sender is an alias for from_address; an explicit from_address wins.
	switch {
	case in.FromAddress != "":
		d.FromAddress = in.FromAddress
	case in.Sender != "":
		d.FromAddress = in.Sender
	}
	if in.To != nil {
		d.To = derefStrings(in.To)
	}
	if in.CC != nil {
		d.CC = derefStrings(in.CC)
	}
	if in.BCC != nil {
		d.BCC = derefStrings(in.BCC)
	}
	if in.Subject != nil {
		d.Subject = *in.Subject
	}
	if in.Text != nil {
		d.Text = *in.Text
	}
	if in.HTML != nil {
		d.HTML = *in.HTML
	}
	return d
}

// persistDraftAttachments writes each uploaded attachment to disk and records
// it against the draft. On any failure it removes the files and rows written
// during this call so a partial upload neither leaks storage nor leaves a
// corrupt draft.
func (s *Server) persistDraftAttachments(ctx context.Context, p model.Principal, draftID string, atts []app.SendAttachment) ([]model.DraftAttachment, error) {
	out := make([]model.DraftAttachment, 0, len(atts))
	type persisted struct {
		rec  model.DraftAttachment
		path string
	}
	written := make([]persisted, 0, len(atts))
	cleanup := func() {
		for _, w := range written {
			if _, derr := s.Service.Store.DeleteDraftAttachment(ctx, p, draftID, w.rec.ID); derr == nil {
				_ = os.Remove(w.path)
			}
		}
	}
	for _, a := range atts {
		rawPath := s.draftAttachmentPath()
		if err := os.MkdirAll(filepath.Dir(rawPath), 0o700); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.WriteFile(rawPath, a.Content, 0o600); err != nil {
			cleanup()
			return nil, err
		}
		rel, _ := filepath.Rel(s.Service.Config.DataDir, rawPath)
		rec, err := s.Service.Store.AddDraftAttachment(ctx, p, draftID, model.DraftAttachment{Filename: a.Filename, ContentType: a.ContentType, Size: int64(len(a.Content)), RawPath: filepath.ToSlash(rel)})
		if err != nil {
			_ = os.Remove(rawPath)
			cleanup()
			return nil, err
		}
		written = append(written, persisted{rec: rec, path: rawPath})
		out = append(out, rec)
	}
	return out, nil
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefStrings(v *[]string) []string {
	if v == nil {
		return nil
	}
	return *v
}

// normalizeDraftAction maps an action alias to its canonical form.
func normalizeDraftAction(action string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "draft", "save":
		return "draft", nil
	case "request-send", "requestsend", "request_send":
		return "request-send", nil
	case "send":
		return "send", nil
	default:
		return "", errUnknownDraftAction
	}
}

type draftActionError struct{ msg string }

func (e *draftActionError) Error() string { return e.msg }

var errUnknownDraftAction error = &draftActionError{msg: "unknown action: expected draft, request-send or send"}
