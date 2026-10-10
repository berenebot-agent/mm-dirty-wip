package store

import (
	"context"
	"database/sql"

	"github.com/dellarb/mailmoose/internal/model"
)

// adjustStorageTx applies a storage delta to an account and, when the owning
// inbox is known, to that inbox, inside a transaction. Positive deltas enforce
// both the account quota and the inbox's own quota (when set); negative deltas
// floor each counter at zero. An inbox whose storage_used_bytes is NULL (one
// created before migration 047) is initialized from a one-time SUM over its
// stored bytes before the delta is applied, so the counter becomes
// authoritative from the first write.
//
// inboxID may be empty for deltas that are not attributable to a single inbox
// (for example removing a whole account); the account counter is still
// adjusted.
func adjustStorageTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		var quota, used int64
		if err := tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, accountID).Scan(&quota, &used); err != nil {
			return err
		}
		if quota > 0 && used+delta > quota {
			return ErrQuota
		}
	}
	if inboxID != "" {
		if err := adjustInboxStorageTx(ctx, tx, accountID, inboxID, delta); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes+?) WHERE id=?`, delta, accountID)
	return err
}

// adjustInboxStorageTx applies a delta to one inbox's storage counter,
// enforcing the inbox quota when the delta grows usage. It assumes any account
// check already happened (or is unnecessary for a bare account adjustment).
func adjustInboxStorageTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string, delta int64) error {
	used, err := inboxUsedTx(ctx, tx, accountID, inboxID)
	if err != nil {
		return err
	}
	if delta > 0 {
		var quota sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT storage_quota_bytes FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&quota); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if quota.Valid && quota.Int64 > 0 && used+delta > quota.Int64 {
			return ErrQuota
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=MAX(0,storage_used_bytes+?) WHERE id=? AND account_id=?`, delta, inboxID, accountID)
	return err
}

// inboxUsedTx returns the inbox's maintained storage counter, initializing it
// from a one-time SUM when it is still NULL (an inbox that predates migration
// 047). The write transaction serializes callers, so the initialization and
// subsequent delta cannot interleave into a double count.
func inboxUsedTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string) (int64, error) {
	var used sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&used); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if used.Valid {
		return used.Int64, nil
	}
	total, err := recomputeInboxStorageTx(ctx, tx, accountID, inboxID)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=? WHERE id=? AND account_id=?`, total, inboxID, accountID); err != nil {
		return 0, err
	}
	return total, nil
}

// recomputeInboxStorageTx sums every byte attributable to an inbox: its message
// rows (inbound, outbound, Spam and trashed all count, matching account
// accounting), its editable draft bodies, and its draft attachment files. This
// is the same definition the account-level counter uses, narrowed to one inbox.
func recomputeInboxStorageTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string) (int64, error) {
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM messages WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&total); err != nil {
		return 0, err
	}
	var draftBodies int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(text_body AS BLOB))+LENGTH(CAST(html_body AS BLOB))),0) FROM drafts WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&draftBodies); err != nil {
		return 0, err
	}
	var draftAtts int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(da.size_bytes),0) FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id WHERE d.account_id=? AND d.inbox_id=?`, accountID, inboxID).Scan(&draftAtts); err != nil {
		return 0, err
	}
	return total + draftBodies + draftAtts, nil
}

func draftBodyBytes(d model.Draft) int64 { return int64(len(d.Text) + len(d.HTML)) }

func getDraftTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.Draft, error) {
	d, err := scanDraft(tx.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}
