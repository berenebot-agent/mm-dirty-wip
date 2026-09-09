package store

import (
	"context"
	"database/sql"
	"time"
)

// DeliveryAttempt is one immutable provider send (success or failure) for an
// outbound message. It is the per-provider activity log: every retry appends a
// row, so an operator can see the full history for a credential.
type DeliveryAttempt struct {
	ID                int64     `json:"id"`
	AccountID         string    `json:"-"`
	CredentialID      string    `json:"credential_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	MessageID         string    `json:"message_id,omitempty"`
	Attempt           int       `json:"attempt"`
	Status            string    `json:"status"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	ErrorText         string    `json:"error_text,omitempty"`
	FromAddress       string    `json:"from_address,omitempty"`
	To                []string  `json:"to,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// maxDeliveryLogPerAccount is the retention cap for the delivery log: the
// newest 5000 rows per account are always kept, and anything younger than
// deliveryLogMaxAge is kept regardless of count.
const maxDeliveryLogPerAccount = 5000

// deliveryLogMaxAge is the time-based retention floor for the delivery log.
const deliveryLogMaxAge = 30 * 24 * time.Hour

// RecordDeliveryAttempt appends one attempt row and prunes the log to the
// retention bounds. It is called from the same transaction that marks the
// message sent or failed, so the attempt and the message state are durable
// together.
func (s *Store) RecordDeliveryAttempt(ctx context.Context, a DeliveryAttempt) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,credential_id,provider,message_id,attempt,status,provider_message_id,error_text,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		a.AccountID, nullString(a.CredentialID), a.Provider, nullString(a.MessageID), a.Attempt, a.Status, a.ProviderMessageID, a.ErrorText, nowText()); err != nil {
		return err
	}
	if err = s.pruneDeliveryLogTx(ctx, tx, a.AccountID); err != nil {
		return err
	}
	return tx.Commit()
}

// insertDeliveryAttemptTx appends one attempt row and prunes the log within an
// existing transaction, so the attempt is durable with the message state change.
func (s *Store) insertDeliveryAttemptTx(ctx context.Context, tx *sql.Tx, accountID, credID, provider, messageID, status, providerMessageID, errorText string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,credential_id,provider,message_id,attempt,status,provider_message_id,error_text,created_at) VALUES(?,?,?,?,(SELECT attempts FROM messages WHERE id=? AND account_id=?),?,?,?,?)`,
		accountID, nullString(credID), provider, nullString(messageID), messageID, accountID, status, providerMessageID, errorText, nowText()); err != nil {
		return err
	}
	return s.pruneDeliveryLogTx(ctx, tx, accountID)
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

// ListDeliveryAttempts returns the delivery-log rows for a credential, newest
// first, using keyset pagination on the row id. beforeID of 0 means the newest
// page. The credential must belong to the account or ErrNotFound is returned.
func (s *Store) ListDeliveryAttempts(ctx context.Context, accountID, credentialID string, limit int, beforeID int64) ([]DeliveryAttempt, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM outbound_credentials WHERE id=? AND account_id=?`, credentialID, accountID).Scan(&n); err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT l.id,l.account_id,COALESCE(l.credential_id,''),l.provider,COALESCE(l.message_id,''),l.attempt,l.status,l.provider_message_id,l.error_text,l.created_at,COALESCE(m.from_address,''),COALESCE(m.to_json,'[]')
		FROM outbound_delivery_log l
		LEFT JOIN messages m ON m.id=l.message_id
		WHERE l.account_id=? AND l.credential_id=?`
	args := []any{accountID, credentialID}
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
	var out []DeliveryAttempt
	for rows.Next() {
		var a DeliveryAttempt
		var created, to string
		if err = rows.Scan(&a.ID, &a.AccountID, &a.CredentialID, &a.Provider, &a.MessageID, &a.Attempt, &a.Status, &a.ProviderMessageID, &a.ErrorText, &created, &a.FromAddress, &to); err != nil {
			return nil, err
		}
		a.To = decodeStrings(to)
		a.CreatedAt = parseTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}
