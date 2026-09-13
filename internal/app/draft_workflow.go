package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// draftContentHash fingerprints the send-relevant content of a draft and its
// attachments, including each attachment's byte hash. It is recorded when a
// send is requested and re-checked when the send is authorized, so an approval
// always applies to the exact version that was reviewed even if the row or an
// attachment file were changed out of band.
func draftContentHash(d model.Draft, atts []model.DraftAttachment) string {
	type attFingerprint struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		ContentHash string `json:"content_hash"`
	}
	fps := make([]attFingerprint, 0, len(atts))
	for _, a := range atts {
		fps = append(fps, attFingerprint{ID: a.ID, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, ContentHash: a.ContentHash})
	}
	sort.Slice(fps, func(i, j int) bool { return fps[i].ID < fps[j].ID })
	payload := struct {
		InboxID string           `json:"inbox_id"`
		From    string           `json:"from_address"`
		ReplyTo string           `json:"reply_to_message_id"`
		To      []string         `json:"to"`
		CC      []string         `json:"cc"`
		BCC     []string         `json:"bcc"`
		Subject string           `json:"subject"`
		Text    string           `json:"text"`
		HTML    string           `json:"html"`
		Attach  []attFingerprint `json:"attachments"`
	}{InboxID: d.InboxID, From: d.FromAddress, ReplyTo: d.ReplyToMessageID, To: d.To, CC: d.CC, BCC: d.BCC, Subject: d.Subject, Text: d.Text, HTML: d.HTML, Attach: fps}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ensureAttachmentHashes computes each attachment's byte hash and verifies it
// against the stored hash. A missing hash is filled in and persisted; a stored
// hash that no longer matches the file is a conflict, so an approval can never
// apply to bytes other than the ones reviewed. It returns the attachments with
// ContentHash populated.
func (s *Service) ensureAttachmentHashes(ctx context.Context, accountID, draftID string, atts []model.DraftAttachment) ([]model.DraftAttachment, error) {
	pending := map[string]string{}
	for i := range atts {
		data, err := os.ReadFile(filepath.Join(s.Config.DataDir, filepath.FromSlash(atts[i].RawPath)))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		switch {
		case strings.TrimSpace(atts[i].ContentHash) == "":
			atts[i].ContentHash = hash
			pending[atts[i].ID] = hash
		case !equalTokenHash(atts[i].ContentHash, hash):
			return nil, store.ErrConflict
		}
	}
	if len(pending) > 0 {
		if err := s.Store.SetDraftAttachmentHashes(ctx, accountID, draftID, pending); err != nil {
			return nil, err
		}
	}
	return atts, nil
}

// newApprovalToken returns a URL-safe random token and its stored hash. The
// plaintext is only ever placed in the approval email; only the hash is kept.
// 12 bytes (96 bits) is used so the token stays short in an email subject while
// remaining infeasible to guess; the decision is additionally bound to the
// approver address, single use and an expiry.
func newApprovalToken() (string, string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// hashApprovalToken hashes a presented token for constant-time comparison with
// a stored hash.
func hashApprovalToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// equalTokenHash compares two hex token hashes in constant time.
func equalTokenHash(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RequestSend records an assistant's request that a draft be authorized and
// sent, freezing the draft. When the inbox has a configured approver, the
// approval-request email is queued in the same transaction that creates the
// request; the explicit external flag is therefore optional and only forces an
// error when the inbox has no approver.
func (s *Service) RequestSend(ctx context.Context, p model.Principal, draftID string, external bool) (model.Draft, error) {
	d, err := s.Store.GetDraft(ctx, p, draftID)
	if err != nil {
		return model.Draft{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.Draft{}, store.ErrForbidden
	}
	inbox, err := s.Store.GetInboxInternal(ctx, p.AccountID, d.InboxID)
	if err != nil {
		return model.Draft{}, err
	}
	// A configured approver makes a request external automatically. The flag
	// remains accepted for compatibility and to make the intent explicit.
	if external || inbox.HasApprover() {
		return s.requestExternalSend(ctx, p, d, inbox)
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
	atts, err = s.ensureAttachmentHashes(ctx, p.AccountID, draftID, atts)
	if err != nil {
		return model.Draft{}, err
	}
	_, events, err := s.Store.CreateSendRequest(ctx, p, draftID, draftContentHash(d, atts))
	if err != nil {
		return model.Draft{}, err
	}
	s.publishAll(events)
	return s.Store.GetDraft(ctx, p, draftID)
}

// requestExternalSend freezes the draft and queues the external approval email
// atomically with the send request.
func (s *Service) requestExternalSend(ctx context.Context, p model.Principal, d model.Draft, inbox model.Inbox) (model.Draft, error) {
	if !inbox.HasApprover() {
		return model.Draft{}, fmt.Errorf("no external approver configured for this inbox")
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
	if events, err := s.Store.ExpireStaleRequestForDraft(ctx, p.AccountID, d.ID); err != nil {
		return model.Draft{}, err
	} else {
		s.publishAll(events)
	}
	if pending, err := s.Store.PendingRequestExists(ctx, p.AccountID, d.ID); err != nil {
		return model.Draft{}, err
	} else if pending {
		return model.Draft{}, store.ErrConflict
	}
	atts, err := s.Store.ListDraftAttachments(ctx, p, d.ID)
	if err != nil {
		return model.Draft{}, err
	}
	atts, err = s.ensureAttachmentHashes(ctx, p.AccountID, d.ID, atts)
	if err != nil {
		return model.Draft{}, err
	}
	contentHash := draftContentHash(d, atts)

	token, tokenHash, err := newApprovalToken()
	if err != nil {
		return model.Draft{}, err
	}
	// The token expiry clock is deliberately NOT started here. It starts when
	// the approval-request email is actually handed to the outbound path, so a
	// request whose notification was never delivered does not silently lapse.
	actor, err := s.Store.ActorIdentity(ctx, p)
	if err != nil {
		return model.Draft{}, err
	}
	body, html, err := s.buildApprovalEmail(d, atts, inbox, token)
	if err != nil {
		return model.Draft{}, err
	}
	msgID := fmt.Sprintf("<%s@%s>", strings.TrimPrefix(idgen.New("msg"), "msg_"), domainOf(inbox.Address))
	raw, attachments, err := s.buildApprovalMessage(inbox, []string{inbox.ApproverEmail}, d, msgID, body, html, atts)
	if err != nil {
		return model.Draft{}, err
	}
	if size := attachmentsSize(attachments); size > s.Config.MaxMessageBytes {
		return model.Draft{}, fmt.Errorf("draft exceeds the maximum message size")
	}
	// The final built MIME is what the provider sees, so it — not just the
	// attachment payloads — must fit the configured limit.
	if int64(len(raw)) > s.Config.MaxMessageBytes {
		return model.Draft{}, fmt.Errorf("approval message exceeds the maximum message size")
	}
	path := s.workflowPath()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return model.Draft{}, err
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return model.Draft{}, err
	}
	rel, _ := filepath.Rel(s.Config.DataDir, path)

	sending, cfgErr := s.Store.GetDomainSendingConfig(ctx, p.AccountID, inbox.DomainID)
	provider := ""
	queuedReason := ""
	if cfgErr != nil {
		if !errors.Is(cfgErr, store.ErrNoProvider) {
			_ = os.Remove(path)
			return model.Draft{}, cfgErr
		}
		queuedReason = "no outbound provider configured for this domain"
	} else {
		provider = sending.Provider
	}
	_, draftEvent, err := s.Store.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox:     inbox,
		Kind:      model.WorkflowKindApprovalRequest,
		Provider:  provider,
		From:      model.Address{Name: inbox.DisplayName, Address: inbox.Address},
		To:        []string{inbox.ApproverEmail},
		Subject:   approvalEmailSubject(d.Subject),
		Text:      body,
		HTML:      html,
		RawPath:   filepath.ToSlash(rel),
		SizeBytes: int64(len(raw)),
		LastError: queuedReason,
		NewSendRequest: &store.SendRequestInsert{
			ID:                  idgen.New("dsr"),
			DraftID:             d.ID,
			InboxID:             d.InboxID,
			ContentHash:         contentHash,
			RequestedAt:         time.Now().UTC(),
			RequestedBy:         actor.Label,
			RequestedByAPIKeyID: actor.APIKeyID,
			RequestedByUserID:   actor.UserID,
			ApproverEmail:       strings.ToLower(strings.TrimSpace(inbox.ApproverEmail)),
			TokenHash:           tokenHash,
		},
	})
	if err != nil {
		_ = os.Remove(path)
		return model.Draft{}, err
	}
	// The request starts in notification_status 'queued' (set by CommitWorkflow)
	// and only becomes 'sent' once the worker hands the job off, so the draft is
	// never presented as successfully awaiting approval before delivery.
	s.publish(draftEvent)
	return s.Store.GetDraft(ctx, p, d.ID)
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
	atts, err = s.ensureAttachmentHashes(ctx, p.AccountID, draftID, atts)
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
		FromAddress:      d.FromAddress,
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

// ApproveExternal authorizes an outstanding external approval request and
// enqueues the frozen draft. It is reached only from the validated control-email
// handler and does not use a Principal: the token and approver match are the
// proof. The claim is still performed by the shared store primitive, so an
// email approval and a UI approval race safely.
func (s *Service) ApproveExternal(ctx context.Context, accountID, inboxID, requestID, approverEmail, feedback string) (SendResult, error) {
	r, err := s.Store.GetSendRequestInternal(ctx, accountID, requestID)
	if err != nil {
		return SendResult{}, err
	}
	if r.InboxID != inboxID {
		return SendResult{}, store.ErrForbidden
	}
	if r.Status != model.SendRequestPending {
		return SendResult{}, store.ErrConflict
	}
	if !strings.EqualFold(strings.TrimSpace(r.ApproverEmail), strings.TrimSpace(approverEmail)) {
		return SendResult{}, store.ErrForbidden
	}
	if r.TokenExpiresAt != nil && time.Now().UTC().After(*r.TokenExpiresAt) {
		return SendResult{}, store.ErrConflict
	}
	d, err := s.Store.GetDraftInternal(ctx, accountID, r.DraftID)
	if err != nil {
		return SendResult{}, err
	}
	atts, err := s.Store.ListDraftAttachmentsInternal(ctx, accountID, d.ID)
	if err != nil {
		return SendResult{}, err
	}
	atts, err = s.ensureAttachmentHashes(ctx, accountID, d.ID, atts)
	if err != nil {
		return SendResult{}, err
	}
	if draftContentHash(d, atts) != r.ContentHash {
		return SendResult{}, store.ErrConflict
	}
	in := SendInput{
		InboxID:          d.InboxID,
		FromAddress:      d.FromAddress,
		To:               d.To,
		CC:               d.CC,
		BCC:              d.BCC,
		Subject:          d.Subject,
		Text:             d.Text,
		HTML:             d.HTML,
		ReplyToMessageID: d.ReplyToMessageID,
		DraftID:          d.ID,
		SendRequestID:    r.ID,
		DecisionActor:    r.ApproverEmail,
		DecisionMethod:   model.DecisionMethodEmail,
		DecisionFeedback: feedback,
	}
	return s.sendDraftCore(ctx, accountID, d, in, "")
}

// RejectExternal records an external rejection of an outstanding request.
func (s *Service) RejectExternal(ctx context.Context, accountID, inboxID, requestID, approverEmail, feedback string) error {
	r, err := s.Store.GetSendRequestInternal(ctx, accountID, requestID)
	if err != nil {
		return err
	}
	if r.InboxID != inboxID {
		return store.ErrForbidden
	}
	if !strings.EqualFold(strings.TrimSpace(r.ApproverEmail), strings.TrimSpace(approverEmail)) {
		return store.ErrForbidden
	}
	_, ev, err := s.Store.RejectSendRequestInternal(ctx, accountID, requestID, r.ApproverEmail, "", model.DecisionMethodEmail, feedback)
	if err != nil {
		return err
	}
	s.publish(ev)
	return nil
}

func (s *Service) publish(ev model.Event) {
	if ev.Type != "" {
		s.Hub.Publish(ev)
	}
}

func (s *Service) publishAll(events []model.Event) {
	for _, ev := range events {
		s.publish(ev)
	}
}

func domainOf(address string) string {
	if i := strings.LastIndex(address, "@"); i >= 0 {
		return address[i+1:]
	}
	return address
}
