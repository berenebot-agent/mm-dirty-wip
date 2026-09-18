package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/limits"
)

// DeliveryAttempt is one immutable provider send (success or failure) for an
// outbound message. It is the per-domain activity log: every retry appends a
// row, so an operator can see the full history for a domain. DomainID is a
// nullable attribution snapshot (SET NULL when the domain is deleted) and the
// provider stays as the attempt-time provider snapshot.
type DeliveryAttempt struct {
	ExternalAliasID   string    `json:"external_alias_id,omitempty"`
	ID                int64     `json:"id"`
	AccountID         string    `json:"-"`
	DomainID          string    `json:"domain_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	MessageID         string    `json:"message_id,omitempty"`
	WorkflowID        string    `json:"workflow_id,omitempty"`
	Attempt           int       `json:"attempt"`
	Status            string    `json:"status"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	ErrorText         string    `json:"error_text,omitempty"`
	FromAddress       string    `json:"from_address,omitempty"`
	To                []string  `json:"to,omitempty"`
	Subject           string    `json:"subject,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	InboxID           string    `json:"-"`
	Client            string    `json:"client,omitempty"`
}

// maxDeliveryLogPerAccount is the retention cap for the delivery log: the
// newest 5000 rows per account are always kept, and anything younger than
// deliveryLogMaxAge is kept regardless of count.
const maxDeliveryLogPerAccount = 5000

// deliveryLogMaxAge is the time-based retention floor for the delivery log.
const deliveryLogMaxAge = 30 * 24 * time.Hour

// RecordDeliveryAttempt appends one attempt row and prunes the log to the
// retention bounds. When the caller does not supply a domain, it is derived
// from the message's inbox; an attempt that cannot be attributed to a domain is
// recorded with a NULL domain rather than dropped.
func (s *Store) RecordDeliveryAttempt(ctx context.Context, a DeliveryAttempt) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	domainID, err := resolveAttemptDomainTx(ctx, tx, a.AccountID, a.DomainID, a.MessageID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,domain_id,provider,message_id,attempt,status,provider_message_id,error_text,created_at,inbox_id,from_address,to_json,subject,client_label) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.AccountID, nullString(domainID), a.Provider, nullString(a.MessageID), a.Attempt, a.Status, a.ProviderMessageID, a.ErrorText, nowText(), a.InboxID, a.FromAddress, jsonString(a.To), a.Subject, a.Client); err != nil {
		return err
	}
	if err = s.pruneDeliveryLogTx(ctx, tx, a.AccountID); err != nil {
		return err
	}
	return tx.Commit()
}

// messageExistsTx reports whether a message row still exists within the caller's
// transaction. It is used to detect a message cancelled between reading and
// writing its outcome, so outcome bookkeeping never references a deleted row.
func messageExistsTx(ctx context.Context, tx *sql.Tx, accountID, id string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// insertDeliveryAttemptTx appends one attempt row and prunes the log within an
// existing transaction, so the attempt is durable with the message state change.
func (s *Store) insertDeliveryAttemptTx(ctx context.Context, tx *sql.Tx, accountID, domainID, provider, messageID, status, providerMessageID, errorText string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,domain_id,provider,message_id,external_alias_id,attempt,status,provider_message_id,error_text,created_at,inbox_id,from_address,to_json,subject,client_label) VALUES(?,?,?,?,COALESCE((SELECT sending_external_alias_id FROM messages WHERE id=? AND account_id=?),''),(SELECT attempts FROM messages WHERE id=? AND account_id=?),?,?,?, ?,COALESCE((SELECT inbox_id FROM messages WHERE id=? AND account_id=?),''),COALESCE((SELECT from_address FROM messages WHERE id=? AND account_id=?),''),COALESCE((SELECT to_json FROM messages WHERE id=? AND account_id=?),'[]'),COALESCE((SELECT subject FROM messages WHERE id=? AND account_id=?),''),COALESCE((SELECT client_label FROM messages WHERE id=? AND account_id=?),''))`,
		accountID, nullString(domainID), provider, nullString(messageID), messageID, accountID, messageID, accountID, status, providerMessageID, errorText, nowText(), messageID, accountID, messageID, accountID, messageID, accountID, messageID, accountID, messageID, accountID); err != nil {
		return err
	}
	return s.pruneDeliveryLogTx(ctx, tx, accountID)
}

// resolveAttemptDomainTx validates an explicitly supplied domain (it must belong
// to the account) or derives one from the message's inbox. An attempt with no
// surviving message is left unattributed.
func resolveAttemptDomainTx(ctx context.Context, tx *sql.Tx, accountID, domainID, messageID string) (string, error) {
	if domainID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&n); err != nil {
			return "", err
		}
		if n != 1 {
			return "", ErrNotFound
		}
		return domainID, nil
	}
	if messageID == "" {
		return "", nil
	}
	var derived string
	err := tx.QueryRowContext(ctx, `SELECT i.domain_id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, messageID, accountID).Scan(&derived)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return derived, nil
}

// pruneDeliveryLogTx deletes delivery-log rows for an account that fall outside
// the retention bounds: a row is dropped if it is not among the newest
// maxDeliveryLogPerAccount rows OR it is older than deliveryLogMaxAge. This
// keeps the log bounded to at most 5000 rows per account and nothing older than
// 30 days.
func (s *Store) pruneDeliveryLogTx(ctx context.Context, tx *sql.Tx, accountID string) error {
	cutoff := time.Now().UTC().Add(-deliveryLogMaxAge)
	_, err := tx.ExecContext(ctx, `DELETE FROM outbound_delivery_log
		WHERE account_id=?
		  AND (id NOT IN (SELECT id FROM outbound_delivery_log WHERE account_id=? ORDER BY id DESC LIMIT ?)
		       OR created_at < ?)`, accountID, accountID, maxDeliveryLogPerAccount, timeText(cutoff))
	return err
}

// ListDomainDeliveryAttempts returns the delivery-log rows for a domain, newest
// first, using keyset pagination on the row id. beforeID of 0 means the newest
// page. The domain must belong to the account or ErrNotFound is returned. The
// history is scoped by domain, never by the domain's current config, so
// deleting or rotating a config does not hide past attempts.
func (s *Store) ListDomainDeliveryAttempts(ctx context.Context, accountID, domainID string, limit int, beforeID int64) ([]DeliveryAttempt, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&n); err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrNotFound
	}
	return s.listDeliveryAttempts(ctx, accountID, "domain_id", domainID, limit, beforeID)
}

func (s *Store) ListExternalAliasDeliveryAttempts(ctx context.Context, accountID, inboxID, aliasID string, limit int, beforeID int64) ([]DeliveryAttempt, error) {
	if _, err := s.GetExternalAlias(ctx, accountID, inboxID, aliasID); err != nil {
		return nil, err
	}
	return s.listDeliveryAttempts(ctx, accountID, "external_alias_id", aliasID, limit, beforeID)
}

// column is selected only by the two internal callers above.
func (s *Store) listDeliveryAttempts(ctx context.Context, accountID, column, targetID string, limit int, beforeID int64) ([]DeliveryAttempt, error) {
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q := `SELECT l.id,l.account_id,COALESCE(l.domain_id,''),l.external_alias_id,l.provider,COALESCE(l.message_id,''),COALESCE(l.workflow_id,''),l.attempt,l.status,l.provider_message_id,l.error_text,l.created_at,
			COALESCE(l.from_address,m.from_address,w.from_address,''),COALESCE(l.to_json,m.to_json,w.to_json,'[]'),COALESCE(l.subject,m.subject,w.subject,'')
		FROM outbound_delivery_log l
		LEFT JOIN messages m ON m.id=l.message_id
		LEFT JOIN outbound_workflow w ON w.id=l.workflow_id
		WHERE l.account_id=? AND l.` + column + `=?`
	args := []any{accountID, targetID}
	if beforeID > 0 {
		q += ` AND l.id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY l.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeliveryAttempt{}
	for rows.Next() {
		var a DeliveryAttempt
		var created, to string
		if err = rows.Scan(&a.ID, &a.AccountID, &a.DomainID, &a.ExternalAliasID, &a.Provider, &a.MessageID, &a.WorkflowID, &a.Attempt, &a.Status, &a.ProviderMessageID, &a.ErrorText, &created, &a.FromAddress, &to, &a.Subject); err != nil {
			return nil, err
		}
		a.To = decodeStrings(to)
		a.CreatedAt = parseTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}
