package store

import (
	"context"
	"database/sql"
)

// reconcileIdempotency backfills the mailbox scope on existing idempotency rows
// and resolves legacy in-flight reservations left by the previous
// delivery-time completion scheme.
func reconcileIdempotency(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE outbound_idempotency SET inbox_id=COALESCE((SELECT m.inbox_id FROM messages m WHERE m.id=outbound_idempotency.message_id),'') WHERE inbox_id='' AND message_id!=''`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id,idem_key FROM outbound_idempotency WHERE status='pending'`)
	if err != nil {
		return err
	}
	type pendingKey struct{ account, idem string }
	var pending []pendingKey
	for rows.Next() {
		var k pendingKey
		if err := rows.Scan(&k.account, &k.idem); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range pending {
		var id, inbox string
		err := tx.QueryRowContext(ctx, `SELECT id,inbox_id FROM messages WHERE account_id=? AND idem_key=? ORDER BY created_at LIMIT 1`, k.account, k.idem).Scan(&id, &inbox)
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND idem_key=?`, k.account, k.idem); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_idempotency SET message_id=?,inbox_id=?,status='done',result_json='{}' WHERE account_id=? AND idem_key=?`, id, inbox, k.account, k.idem); err != nil {
			return err
		}
	}
	return nil
}

// backfillDraftStorage charges existing draft bodies and attachment files to
// their account, so the new quota accounting starts from a correct baseline.
func backfillDraftStorage(ctx context.Context, tx *sql.Tx) error {
	usage := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT account_id,text_body,html_body FROM drafts`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var account, text, html string
		if err := rows.Scan(&account, &text, &html); err != nil {
			rows.Close()
			return err
		}
		usage[account] += int64(len(text) + len(html))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT d.account_id,da.size_bytes FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var account string
		var size int64
		if err := rows.Scan(&account, &size); err != nil {
			rows.Close()
			return err
		}
		usage[account] += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for account, n := range usage {
		if n == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=storage_used_bytes+? WHERE id=?`, n, account); err != nil {
			return err
		}
	}
	return nil
}
