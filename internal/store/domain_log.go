package store

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/dellarb/mailmoose/internal/limits"
)

// DomainLogEntry is one row in a domain's two-way activity log. It merges the
// outbound delivery attempts (Kind "sent"/"failed") with inbound mail received
// for the domain (Kind "received"), inbound mail rejected by an inbox's
// allowed-senders rule (Kind "blocked"), and consumed approval control mail
// (Kind "approval"). The store assembles it at read time from the existing
// durable tables; nothing new is persisted.
type DomainLogEntry struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	At          time.Time `json:"created_at"`
	InboxID     string    `json:"inbox_id,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	FromAddress string    `json:"from_address,omitempty"`
	To          []string  `json:"to,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	// Client names the credential that sent an outbound message, or "Control"
	// for a consumed approval control message. It is empty for received and
	// blocked inbound mail.
	Client            string `json:"client,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	Attempt           int    `json:"attempt,omitempty"`
	Status            string `json:"status,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ErrorText         string `json:"error_text,omitempty"`
	// RequestID/Action are set for consumed approval control messages.
	RequestID string `json:"request_id,omitempty"`
	Action    string `json:"action,omitempty"`
	// MessageID is the click-through message id for delivered inbound mail and
	// for outbound attempts whose message still exists. It is empty for blocked
	// mail (which has no message row or detail view) and for attempts whose
	// message was deleted.
	MessageID string `json:"message_id,omitempty"`
}

// ListDomainReceivingLog returns the receiving side of a domain's two-way log:
// delivered inbound mail plus inbound mail blocked by the allowed-senders rule,
// newest first. The domain must belong to the account or ErrNotFound is
// returned.
func (s *Store) ListDomainReceivingLog(ctx context.Context, accountID, domainID string, limit int, before time.Time) ([]DomainLogEntry, error) {
	return s.listDomainLog(ctx, accountID, domainID, limit, before, false)
}

// ListDomainLog returns the full two-way log for a domain: outbound delivery
// attempts, delivered inbound mail, and blocked inbound mail, merged newest
// first. Pagination is a timestamp keyset: before is the At of the last row of
// the previous page (zero means the newest page).
func (s *Store) ListDomainLog(ctx context.Context, accountID, domainID string, limit int, before time.Time) ([]DomainLogEntry, error) {
	return s.listDomainLog(ctx, accountID, domainID, limit, before, true)
}

// ListAccountLog returns the account's newest durable activity across all
// domains. Message rows are optional links; the activity snapshots survive
// message deletion.
func (s *Store) ListAccountLog(ctx context.Context, accountID string, limit int) ([]DomainLogEntry, error) {
	return s.listDomainLog(ctx, accountID, "", limit, time.Time{}, true)
}

func (s *Store) listDomainLog(ctx context.Context, accountID, domainID string, limit int, before time.Time, includeOutbound bool) ([]DomainLogEntry, error) {
	var n int
	if domainID != "" {
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&n); err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, ErrNotFound
		}
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	// Fetch one extra row per source so a merge can still fill the page when
	// one source is exhausted first.
	fetch := limit + 1
	beforeText := ""
	if !before.IsZero() {
		beforeText = timeText(before)
	}

	out := make([]DomainLogEntry, 0, fetch*3)
	var err error
	if out, err = s.appendDomainReceiving(ctx, out, accountID, domainID, beforeText, fetch); err != nil {
		return nil, err
	}
	if includeOutbound {
		if out, err = s.appendDomainOutbound(ctx, out, accountID, domainID, beforeText, fetch); err != nil {
			return nil, err
		}
	}
	if out, err = s.appendDomainControl(ctx, out, accountID, domainID, beforeText, fetch); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// appendDomainReceiving adds delivered inbound messages and blocked inbound
// records whose inbox belongs to the domain.
func (s *Store) appendDomainReceiving(ctx context.Context, out []DomainLogEntry, accountID, domainID, beforeText string, limit int) ([]DomainLogEntry, error) {
	deliveredQ := `SELECT CASE WHEN EXISTS(SELECT 1 FROM messages m WHERE m.id=l.message_id AND m.account_id=l.account_id) THEN l.message_id ELSE '' END,l.inbox_id,l.provider,l.provider_message_id,l.from_address,l.to_json,l.subject,l.size_bytes,l.created_at
		FROM inbound_delivery_log l
		WHERE l.account_id=?`
	deliveredArgs := []any{accountID}
	if domainID != "" {
		deliveredQ += ` AND l.domain_id=?`
		deliveredArgs = append(deliveredArgs, domainID)
	}
	if beforeText != "" {
		deliveredQ += ` AND l.created_at < ?`
		deliveredArgs = append(deliveredArgs, beforeText)
	}
	deliveredQ += ` ORDER BY l.created_at DESC, l.id DESC LIMIT ?`
	deliveredArgs = append(deliveredArgs, limit)
	rows, err := s.read.QueryContext(ctx, deliveredQ, deliveredArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e DomainLogEntry
		var created, to string
		if err = rows.Scan(&e.ID, &e.InboxID, &e.Provider, &e.ProviderMessageID, &e.FromAddress, &to, &e.Subject, &e.SizeBytes, &created); err != nil {
			rows.Close()
			return nil, err
		}
		e.Kind = "received"
		e.Status = "received"
		if e.ID != "" {
			e.MessageID = e.ID
		}
		e.To = decodeStrings(to)
		e.At = parseTime(created)
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	blockedQ := `SELECT b.id,b.inbox_id,b.provider,b.from_address,b.to_json,b.subject,b.size_bytes,b.reason,b.created_at
		FROM blocked_messages b JOIN inboxes i ON i.id=b.inbox_id AND i.account_id=b.account_id
		WHERE b.account_id=?`
	blockedArgs := []any{accountID}
	if domainID != "" {
		blockedQ += ` AND i.domain_id=?`
		blockedArgs = append(blockedArgs, domainID)
	}
	if beforeText != "" {
		blockedQ += ` AND b.created_at < ?`
		blockedArgs = append(blockedArgs, beforeText)
	}
	blockedQ += ` ORDER BY b.created_at DESC LIMIT ?`
	blockedArgs = append(blockedArgs, limit)
	brows, err := s.read.QueryContext(ctx, blockedQ, blockedArgs...)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var e DomainLogEntry
		var created, to string
		if err = brows.Scan(&e.ID, &e.InboxID, &e.Provider, &e.FromAddress, &to, &e.Subject, &e.SizeBytes, &e.Reason, &created); err != nil {
			return nil, err
		}
		e.Kind = "blocked"
		e.Status = "blocked"
		e.To = decodeStrings(to)
		e.At = parseTime(created)
		out = append(out, e)
	}
	return out, brows.Err()
}

// appendDomainControl adds consumed approval control messages for the domain's
// inboxes, so an operator can see that a decision email was consumed.
func (s *Store) appendDomainControl(ctx context.Context, out []DomainLogEntry, accountID, domainID, beforeText string, limit int) ([]DomainLogEntry, error) {
	q := `SELECT c.id,c.inbox_id,c.provider,c.from_address,c.request_id,c.action,c.outcome,c.reason,c.subject,c.created_at
		FROM inbound_control_messages c JOIN inboxes i ON i.id=c.inbox_id AND i.account_id=c.account_id
		WHERE c.account_id=?`
	args := []any{accountID}
	if domainID != "" {
		q += ` AND i.domain_id=?`
		args = append(args, domainID)
	}
	if beforeText != "" {
		q += ` AND c.created_at < ?`
		args = append(args, beforeText)
	}
	q += ` ORDER BY c.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e DomainLogEntry
		var created, outcome, subject string
		if err := rows.Scan(&e.ID, &e.InboxID, &e.Provider, &e.FromAddress, &e.RequestID, &e.Action, &outcome, &e.Reason, &subject, &created); err != nil {
			return nil, err
		}
		e.Kind = "approval"
		e.Status = outcome
		e.Subject = ApprovalSubjectLabel(outcome, subject)
		e.Client = "Control"
		e.At = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// appendDomainOutbound adds the domain's outbound delivery attempts.
func (s *Store) appendDomainOutbound(ctx context.Context, out []DomainLogEntry, accountID, domainID, beforeText string, limit int) ([]DomainLogEntry, error) {
	q := `SELECT l.id,l.provider,CASE WHEN EXISTS(SELECT 1 FROM messages m WHERE m.id=l.message_id AND m.account_id=l.account_id) THEN COALESCE(l.message_id,'') ELSE '' END,l.attempt,l.status,l.provider_message_id,l.error_text,l.created_at,
			COALESCE(l.from_address,m.from_address,w.from_address,''),COALESCE(l.to_json,m.to_json,w.to_json,'[]'),COALESCE(l.subject,m.subject,w.subject,''),COALESCE(l.inbox_id,m.inbox_id,w.inbox_id,''),COALESCE(l.client_label,m.client_label,'')
		FROM outbound_delivery_log l
		LEFT JOIN messages m ON m.id=l.message_id
		LEFT JOIN outbound_workflow w ON w.id=l.workflow_id
		WHERE l.account_id=?`
	args := []any{accountID}
	if domainID != "" {
		q += ` AND l.domain_id=?`
		args = append(args, domainID)
	}
	if beforeText != "" {
		q += ` AND l.created_at < ?`
		args = append(args, beforeText)
	}
	q += ` ORDER BY l.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e DomainLogEntry
		var id int64
		var created, to string
		if err = rows.Scan(&id, &e.Provider, &e.MessageID, &e.Attempt, &e.Status, &e.ProviderMessageID, &e.ErrorText, &created, &e.FromAddress, &to, &e.Subject, &e.InboxID, &e.Client); err != nil {
			return nil, err
		}
		e.Kind = e.Status
		e.ID = strconv.FormatInt(id, 10)
		e.To = decodeStrings(to)
		e.At = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}
