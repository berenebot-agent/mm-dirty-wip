package app

import (
	"context"
	"html"
	"regexp"
	"strings"
	"time"

	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

// controlSubjectRe matches the strict approval control subject. It is
// case-insensitive and tolerates surrounding text such as a Re:/Fwd: prefix.
// The token is opaque and URL-safe.
var controlSubjectRe = regexp.MustCompile(`(?i)\[GH-(APPROVE|REJECT):([A-Za-z0-9_-]{20,})\]`)

// maxFeedbackRunes bounds stored feedback so a control email cannot grow the
// request, event or UI without limit.
const maxFeedbackRunes = 2000

type controlDirective struct {
	action string // APPROVE or REJECT
	token  string
}

// looksLikeControl reports whether the subject contains any approval control
// token, even a malformed or duplicated one. Such mail is consumed rather than
// delivered so a token never leaks into mailbox content.
func looksLikeControl(subject string) bool {
	return controlSubjectRe.MatchString(subject)
}

// parseControlSubject returns the single approval directive in a subject. It
// reports ok=false when there are zero or more than one matches.
func parseControlSubject(subject string) (controlDirective, bool) {
	m := controlSubjectRe.FindAllStringSubmatch(subject, -1)
	if len(m) != 1 {
		return controlDirective{}, false
	}
	return controlDirective{action: strings.ToUpper(m[0][1]), token: m[0][2]}, true
}

// extractFeedback returns the deliberate feedback inside the first
// [GH-FEEDBACK-BEGIN]...[GH-FEEDBACK-END] block, ignoring everything outside it.
// The plain-text body is preferred; an HTML-only message falls back to a
// tag-stripped version of its body.
func extractFeedback(parsed mailparse.Parsed) string {
	if fb, ok := blockBetween(parsed.Text); ok {
		return capFeedback(fb)
	}
	if parsed.HTML != "" {
		if fb, ok := blockBetween(stripTags(parsed.HTML)); ok {
			return capFeedback(fb)
		}
	}
	return ""
}

func blockBetween(body string) (string, bool) {
	lower := strings.ToLower(body)
	begin := strings.Index(lower, strings.ToLower(feedbackBegin))
	if begin < 0 {
		return "", false
	}
	rest := lower[begin+len(feedbackBegin):]
	end := strings.Index(rest, strings.ToLower(feedbackEnd))
	if end < 0 {
		return "", false
	}
	inner := body[begin+len(feedbackBegin) : begin+len(feedbackBegin)+end]
	return strings.TrimSpace(inner), true
}

func capFeedback(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > maxFeedbackRunes {
		return string(r[:maxFeedbackRunes])
	}
	return s
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	return html.UnescapeString(tagRe.ReplaceAllString(s, " "))
}

// handleControlMessage consumes an inbound approval control message. It returns
// transport.ErrInboundIgnored so the webhook is acknowledged without the
// message being stored. Every outcome is recorded for the domain log; a
// successful decision publishes the normal draft event.
func (s *Service) handleControlMessage(ctx context.Context, provider string, msg transport.InboundMessage, inbox model.Inbox, parsed mailparse.Parsed) error {
	dir, ok := parseControlSubject(parsed.Subject)
	record := func(requestID, action, outcome, reason string) {
		_, _ = s.Store.RecordControlMessage(ctx, storeControlRecord(msg, inbox, parsed, requestID, action, outcome, reason))
	}
	if !ok {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_invalid", "malformed approval control subject")
		record("", "", "invalid", "malformed control subject")
		return transport.ErrInboundIgnored
	}
	action := "reject"
	if dir.action == "APPROVE" {
		action = "approve"
	}
	hash := hashApprovalToken(dir.token)
	r, err := s.Store.FindPendingSendRequestByToken(ctx, inbox.AccountID, inbox.ID, hash)
	if err != nil {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_unknown", "no live request for approval token")
		record("", action, "invalid", "unknown or already decided token")
		return transport.ErrInboundIgnored
	}
	if !strings.EqualFold(strings.TrimSpace(r.ApproverEmail), strings.TrimSpace(parsed.From.Address)) {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_sender", "approval sender does not match nominated approver")
		record(r.ID, action, "invalid", "sender does not match approver")
		return transport.ErrInboundIgnored
	}
	if r.TokenExpiresAt != nil && time.Now().UTC().After(*r.TokenExpiresAt) {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_expired", "approval token expired")
		record(r.ID, action, "invalid", "expired")
		return transport.ErrInboundIgnored
	}
	feedback := extractFeedback(parsed)
	if dir.action == "APPROVE" {
		if _, err := s.ApproveExternal(ctx, inbox.AccountID, inbox.ID, r.ID, parsed.From.Address, feedback); err != nil {
			s.Store.Audit(ctx, inbox.AccountID, provider+".control_approve_failed", err.Error())
			record(r.ID, action, "error", err.Error())
			return transport.ErrInboundIgnored
		}
		record(r.ID, action, "approved", "")
		return transport.ErrInboundIgnored
	}
	if err := s.RejectExternal(ctx, inbox.AccountID, inbox.ID, r.ID, parsed.From.Address, feedback); err != nil {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_reject_failed", err.Error())
		record(r.ID, action, "error", err.Error())
		return transport.ErrInboundIgnored
	}
	record(r.ID, action, "rejected", "")
	return transport.ErrInboundIgnored
}

func storeControlRecord(msg transport.InboundMessage, inbox model.Inbox, parsed mailparse.Parsed, requestID, action, outcome, reason string) store.ControlMessageRecord {
	return store.ControlMessageRecord{
		AccountID:          inbox.AccountID,
		InboxID:            inbox.ID,
		Provider:           msg.Provider,
		ProviderDeliveryID: msg.DeliveryID,
		EnvelopeRecipient:  msg.Recipient,
		FromName:           parsed.From.Name,
		FromAddress:        parsed.From.Address,
		RequestID:          requestID,
		Action:             action,
		Outcome:            outcome,
		Reason:             reason,
	}
}
