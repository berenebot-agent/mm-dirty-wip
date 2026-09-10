package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// draftContentHash fingerprints the send-relevant content of a draft and its
// attachments. It is recorded when a send is requested and re-checked when the
// send is authorized, so an approval always applies to the exact version that
// was reviewed even if the row were changed out of band.
func draftContentHash(d model.Draft, atts []model.DraftAttachment) string {
	type attFingerprint struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	}
	fps := make([]attFingerprint, 0, len(atts))
	for _, a := range atts {
		fps = append(fps, attFingerprint{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size})
	}
	sort.Slice(fps, func(i, j int) bool { return fps[i].ID < fps[j].ID })
	payload := struct {
		InboxID string           `json:"inbox_id"`
		ReplyTo string           `json:"reply_to_message_id"`
		To      []string         `json:"to"`
		CC      []string         `json:"cc"`
		BCC     []string         `json:"bcc"`
		Subject string           `json:"subject"`
		Text    string           `json:"text"`
		HTML    string           `json:"html"`
		Attach  []attFingerprint `json:"attachments"`
	}{InboxID: d.InboxID, ReplyTo: d.ReplyToMessageID, To: d.To, CC: d.CC, BCC: d.BCC, Subject: d.Subject, Text: d.Text, HTML: d.HTML, Attach: fps}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RequestSend records an assistant's request that a draft be authorized and
// sent. The draft is validated as sendable and then frozen.
func (s *Service) RequestSend(ctx context.Context, p model.Principal, draftID string) (model.Draft, error) {
	d, err := s.Store.GetDraft(ctx, p, draftID)
	if err != nil {
		return model.Draft{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.Draft{}, store.ErrForbidden
	}
	to, err := cleanAddresses(d.To)
	if err != nil {
		return model.Draft{}, err
	}
	if len(to) == 0 {
		return model.Draft{}, fmt.Errorf("recipient required")
	}
	if strings.TrimSpace(d.Text) == "" && strings.TrimSpace(d.HTML) == "" {
		return model.Draft{}, fmt.Errorf("message body is required")
	}
	atts, err := s.Store.ListDraftAttachments(ctx, p, draftID)
	if err != nil {
		return model.Draft{}, err
	}
	_, ev, err := s.Store.CreateSendRequest(ctx, p, draftID, draftContentHash(d, atts))
	if err != nil {
		return model.Draft{}, err
	}
	s.publish(ev)
	return s.Store.GetDraft(ctx, p, draftID)
}

// CancelSendRequest withdraws an outstanding send request and unfreezes the
// draft so it can be edited or resent.
func (s *Service) CancelSendRequest(ctx context.Context, p model.Principal, draftID string) (model.Draft, error) {
	_, ev, err := s.Store.CancelSendRequest(ctx, p, draftID)
	if err != nil {
		return model.Draft{}, err
	}
	s.publish(ev)
	return s.Store.GetDraft(ctx, p, draftID)
}

// ApproveDraft authorizes a pending send request and enqueues the frozen draft
// through the existing send flow. The message fields are read server-side from
// the stored draft; no caller-supplied content is trusted.
func (s *Service) ApproveDraft(ctx context.Context, p model.Principal, draftID, feedback, method, idem string) (SendResult, error) {
	d, err := s.Store.GetDraft(ctx, p, draftID)
	if err != nil {
		return SendResult{}, err
	}
	if !p.CanOwn(d.InboxID) {
		return SendResult{}, store.ErrForbidden
	}
	r, err := s.Store.GetSendRequestByDraft(ctx, p, draftID)
	if err != nil {
		return SendResult{}, err
	}
	if r.Status != model.SendRequestPending {
		return SendResult{}, store.ErrConflict
	}
	atts, err := s.Store.ListDraftAttachments(ctx, p, draftID)
	if err != nil {
		return SendResult{}, err
	}
	if draftContentHash(d, atts) != r.ContentHash {
		return SendResult{}, store.ErrConflict
	}
	actor, err := s.Store.ActorIdentity(ctx, p)
	if err != nil {
		return SendResult{}, err
	}
	in := SendInput{
		InboxID:          d.InboxID,
		To:               d.To,
		CC:               d.CC,
		BCC:              d.BCC,
		Subject:          d.Subject,
		Text:             d.Text,
		HTML:             d.HTML,
		ReplyToMessageID: d.ReplyToMessageID,
		DraftID:          draftID,
		SendRequestID:    r.ID,
		DecisionActor:    actor.Label,
		DecisionActorID:  actor.ID(),
		DecisionMethod:   method,
		DecisionFeedback: feedback,
	}
	return s.SendDraft(ctx, p, draftID, in, idem)
}

// RejectDraft records an owner's rejection of a pending send request. The draft
// is kept, marked rejected, and can be revised and resubmitted.
func (s *Service) RejectDraft(ctx context.Context, p model.Principal, draftID, feedback, method string) (model.Draft, error) {
	_, ev, err := s.Store.RejectSendRequest(ctx, p, draftID, feedback, method)
	if err != nil {
		return model.Draft{}, err
	}
	s.publish(ev)
	return s.Store.GetDraft(ctx, p, draftID)
}

func (s *Service) publish(ev model.Event) {
	if ev.Type != "" {
		s.Hub.Publish(ev)
	}
}
