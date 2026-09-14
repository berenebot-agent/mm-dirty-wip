package store

import (
	"context"
	"database/sql"

	"gatehouse-mail/internal/model"
)

// adjustStorageTx applies a storage delta to an account inside a transaction.
// Positive deltas enforce the account quota; negative deltas floor at zero.
func adjustStorageTx(ctx context.Context, tx *sql.Tx, accountID string, delta int64) error {
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
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes+?) WHERE id=?`, delta, accountID)
	return err
}

func draftBodyBytes(d model.Draft) int64 { return int64(len(d.Text) + len(d.HTML)) }

func getDraftTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.Draft, error) {
	d, err := scanDraft(tx.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,from_external_alias_id,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}
