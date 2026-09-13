package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/mail"
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
var controlSubjectRe = regexp.MustCompile(`(?i)\[GH-(APPROVE|REJECT):([A-Za-z0-9_-]{16,})\]`)

// maxFeedbackRunes bounds stored feedback so a control email cannot grow the
// request, event or UI without limit.
const maxFeedbackRunes = 2000

type controlDirective struct {
	action string // "approve" or "reject"
	token  string
	reply  bool // true when derived from a body reference line, not a subject
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
	return controlDirective{action: strings.ToLower(m[0][1]), token: m[0][2]}, true
}

// controlRequestRe matches the neutral reference line embedded in an approval
// email body. A plain reply quotes it, binding the reply to the same one-time
// request the mailto buttons target.
var controlRequestRe = regexp.MustCompile(`\[GH-REQUEST:([A-Za-z0-9_-]{16,})\]`)

// replyBody returns the reply's readable body. The plain-text part is preferred;
// an HTML-only reply falls back to a tag-stripped version.
func replyBody(parsed mailparse.Parsed) string {
	if strings.TrimSpace(parsed.Text) != "" {
		return parsed.Text
	}
	if parsed.HTML != "" {
		return stripTags(parsed.HTML)
	}
	return ""
}

// looksLikeControlReply reports whether the body carries any control reference
// line, even a malformed or duplicated one. Such mail is consumed rather than
// delivered so the token can never leak into mailbox content. A cheap substring
// pre-check keeps this off the tag-stripping path for ordinary HTML mail.
func looksLikeControlReply(parsed mailparse.Parsed) bool {
	if strings.Contains(parsed.Text, "[GH-REQUEST:") {
		return controlRequestRe.MatchString(parsed.Text)
	}
	if parsed.HTML != "" && strings.Contains(parsed.HTML, "[GH-REQUEST:") {
		return controlRequestRe.MatchString(stripTags(parsed.HTML))
	}
	return false
}

// controlReplyToken returns the single control-request token in a reply body.
// Repeated occurrences of the same token (nested quoting) collapse to one; two
// different tokens are ambiguous and rejected.
func controlReplyToken(parsed mailparse.Parsed) (string, bool) {
	matches := controlRequestRe.FindAllStringSubmatch(replyBody(parsed), -1)
	token := ""
	for _, m := range matches {
		if token == "" {
			token = m[1]
			continue
		}
		if m[1] != token {
			return "", false
		}
	}
	if token == "" {
		return "", false
	}
	return token, true
}

// parseControlReply derives a directive from a body reference line. The action
// comes from the approver's first line of new text, so quoted content (which
// always contains the word "Approve") can never select the action.
func parseControlReply(parsed mailparse.Parsed) (controlDirective, bool) {
	token, ok := controlReplyToken(parsed)
	if !ok {
		return controlDirective{}, false
	}
	return controlDirective{action: firstLineAction(replyNewText(replyBody(parsed))), token: token, reply: true}, true
}

// replyNewText returns the approver's own text at the top of a reply, cut at the
// first quoted-history boundary. It deliberately does not implement full
// client-specific quote parsing: it only needs to stop before the quoted
// original so the decision and feedback never come from quoted content.
func replyNewText(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	end := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isQuoteBoundary(trimmed) {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}

// isQuoteBoundary reports whether a line begins quoted history rather than the
// approver's new text.
func isQuoteBoundary(line string) bool {
	if strings.HasPrefix(line, ">") || strings.HasPrefix(line, "<") {
		return true
	}
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "on ") && strings.Contains(lower, " wrote:"):
		return true
	case strings.HasPrefix(lower, "from:"), strings.HasPrefix(lower, "sent:"),
		strings.HasPrefix(lower, "to:"), strings.HasPrefix(lower, "subject:"),
		strings.HasPrefix(lower, "cc:"), strings.HasPrefix(lower, "reference:"):
		return true
	case strings.HasPrefix(lower, "-----original message"), strings.HasPrefix(lower, "________"):
		return true
	}
	return false
}

// approveFirstLineRe matches the first lines that approve: the approve words
// plus common affirmatives, any case. The word must be the whole first token,
// so "approval", "unapproved", "disapprove" and "no" reject.
var approveFirstLineRe = regexp.MustCompile(`(?i)^(approve|approved|yes|yep|yeah|ok|okay|accept|accepted|confirm|confirmed|authorize|authorized|authorised|lgtm|y)\b`)

// firstLineAction reads the decision from the first non-empty line of the
// approver's new text. Only the exact approve word (any case, optionally
// followed by punctuation or more text) approves; everything else rejects.
func firstLineAction(newText string) string {
	for _, line := range strings.Split(newText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if approveFirstLineRe.MatchString(strings.Trim(line, "\"'.,:;!*-` ")) {
			return "approve"
		}
		return "reject"
	}
	return "reject"
}

// replyFeedback returns the approver's typed feedback. For an approval the
// decision line is dropped; a rejection keeps the whole text because the reason
// is often the first line. It is capped like the marker-based feedback.
func replyFeedback(newText, action string) string {
	if strings.TrimSpace(newText) == "" {
		return ""
	}
	lines := strings.Split(newText, "\n")
	if action == "approve" {
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines = lines[i+1:]
			break
		}
	}
	return capFeedback(strings.TrimSpace(strings.Join(lines, "\n")))
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

// canonicalSender normalizes an address for comparison, stripping any display
// name and lowercasing it. It is applied to both the provider-attested envelope
// sender and the message header sender.
func canonicalSender(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if a, err := mail.ParseAddress(raw); err == nil {
		return strings.ToLower(strings.TrimSpace(a.Address))
	}
	return strings.ToLower(raw)
}

// handleControlMessage consumes an inbound approval control message. It returns
// transport.ErrInboundIgnored so the webhook is acknowledged without the
// message being stored. Every outcome is recorded for the domain log; a
// successful decision publishes the normal draft event.
func (s *Service) handleControlMessage(ctx context.Context, provider string, msg transport.InboundMessage, inbox model.Inbox, parsed mailparse.Parsed) error {
	dir, ok := parseControlSubject(parsed.Subject)
	if !ok {
		dir, ok = parseControlReply(parsed)
	}
	// subject is the reviewed draft subject, filled in once the request (and
	// therefore the draft) is resolved. It is snapshotted because an approved
	// send deletes the draft before the record is written.
	subject := ""
	record := func(requestID, action, outcome, reason string) {
		_, _ = s.Store.RecordControlMessage(ctx, storeControlRecord(msg, inbox, parsed, requestID, action, outcome, reason, subject))
	}
	if !ok {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_invalid", "malformed approval control subject")
		record("", "", "invalid", "malformed control subject")
		return transport.ErrInboundIgnored
	}
	action := dir.action
	hash := hashApprovalToken(dir.token)
	r, err := s.Store.FindPendingSendRequestByToken(ctx, inbox.AccountID, inbox.ID, hash)
	if err != nil {
		// Distinguish a genuinely unknown/decided token from a transient store
		// fault. The former is terminal and acknowledged; the latter is
		// returned so the delivery is retried rather than silently dropped.
		if !errors.Is(err, store.ErrNotFound) {
			s.Store.Audit(ctx, inbox.AccountID, provider+".control_lookup_failed", err.Error())
			record("", action, "error", err.Error())
			return fmt.Errorf("approval token lookup failed: %w", err)
		}
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_unknown", "no live request for approval token")
		record("", action, "invalid", "unknown or already decided token")
		return transport.ErrInboundIgnored
	}
	if d, derr := s.Store.GetDraftInternal(ctx, inbox.AccountID, r.DraftID); derr == nil {
		subject = d.Subject
	}
	// The provider-attested envelope sender is authoritative: the MIME From
	// header is attacker-controlled, while the envelope is what the receiving
	// MTA observed and what SPF covers. Both must match the nominated approver,
	// and a missing envelope fails closed.
	approver := strings.ToLower(strings.TrimSpace(r.ApproverEmail))
	envelopeFrom := canonicalSender(msg.EnvelopeFrom)
	mimeFrom := canonicalSender(parsed.From.Address)
	if envelopeFrom == "" {
		s.Store.Audit(ctx, inbox.AccountID, provider+".control_sender", "approval envelope sender missing")
		record(r.ID, action, "invalid", "approval envelope sender missing")
		return transport.ErrInboundIgnored
	}
	if envelopeFrom != approver || mimeFrom != approver {
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
	if dir.reply && strings.TrimSpace(feedback) == "" {
		feedback = replyFeedback(replyNewText(replyBody(parsed)), action)
	}
	if action == "approve" {
		if _, err := s.ApproveExternal(ctx, inbox.AccountID, inbox.ID, r.ID, parsed.From.Address, feedback); err != nil {
			return s.controlDecisionError(ctx, inbox.AccountID, provider, action, r.ID, "approve", err, record)
		}
		record(r.ID, action, "approved", "")
		return transport.ErrInboundIgnored
	}
	if err := s.RejectExternal(ctx, inbox.AccountID, inbox.ID, r.ID, parsed.From.Address, feedback); err != nil {
		return s.controlDecisionError(ctx, inbox.AccountID, provider, action, r.ID, "reject", err, record)
	}
	record(r.ID, action, "rejected", "")
	return transport.ErrInboundIgnored
}

// controlDecisionError classifies a failed approval/rejection. A terminal
// outcome (the request was already decided, expired, withdrawn or no longer
// exists) is recorded and acknowledged so the sender stops retrying. Any other
// failure — a transient database or filesystem fault — is recorded but
// returned, so the webhook provider re-delivers and the MX edge answers a
// temporary SMTP failure, and the human's decision is not silently lost.
func (s *Service) controlDecisionError(ctx context.Context, accountID, provider, action, requestID, verb string, err error, record func(requestID, action, outcome, reason string)) error {
	s.Store.Audit(ctx, accountID, provider+".control_"+verb+"_failed", err.Error())
	record(requestID, action, "error", err.Error())
	if isTerminalControlError(err) {
		return transport.ErrInboundIgnored
	}
	return fmt.Errorf("approval %s could not be processed: %w", verb, err)
}

// isTerminalControlError reports whether a decision error means the request can
// never be decided successfully on a retry, as opposed to a transient fault.
func isTerminalControlError(err error) bool {
	return errors.Is(err, store.ErrConflict) ||
		errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, store.ErrForbidden)
}

func storeControlRecord(msg transport.InboundMessage, inbox model.Inbox, parsed mailparse.Parsed, requestID, action, outcome, reason, subject string) store.ControlMessageRecord {
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
		Subject:            subject,
	}
}
