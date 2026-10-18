package store

import (
	"context"
	"database/sql"

	"github.com/dellarb/mailmoose/internal/messagepreview"
	"github.com/dellarb/mailmoose/internal/model"
)

func cacheLocalPreview(ctx context.Context, tx *sql.Tx, id, text, markup string) error {
	_, err := tx.ExecContext(ctx, `UPDATE messages SET snippet=? WHERE id=?`, messagepreview.FromBodies(text, markup), id)
	return err
}

// QueueRemotePreview queues only a newly classified ordinary IMAP arrival. No
// scan of historic messages is performed, and refreshing headers cannot queue it.
func (s *Store) QueueRemotePreview(ctx context.Context, account, inbox, id string) error {
	_, err := s.QueueRemotePreviewRows(ctx, account, inbox, id)
	return err
}

// QueueRemotePreviewRows queues a remote preview and reports how many rows the
// update matched, so a caller can tell "queued" from "declined" (the row was
// missing, already had a snippet, or carried no remote UID). A decline is a
// legitimate outcome — a refresh re-write, a re-detection — but silently
// swallowing it hid a real gap: every IMAP arrival was declined and no caller
// could see it.
func (s *Store) QueueRemotePreviewRows(ctx context.Context, account, inbox, id string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_messages SET snippet_pending=1 WHERE account_id=? AND inbox_id=? AND id=? AND snippet='' AND remote_uid>0`, account, inbox, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) PendingRemotePreviews(ctx context.Context, limit int) ([]RemoteMessage, error) {
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+remoteMessageColumns+` FROM inbox_remote_messages WHERE snippet_pending=1 ORDER BY created_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RemoteMessage
	for rows.Next() {
		m, err := s.scanRemoteMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CompleteRemotePreview is supplementary metadata: a failed/empty attempt leaves
// the API fallback in place. Quota accounting follows every other remote write.
func (s *Store) CompleteRemotePreview(ctx context.Context, account, inbox, id, preview string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := remoteMetadataBytesTx(ctx, tx, account, inbox)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_remote_messages SET snippet=?,snippet_pending=0 WHERE account_id=? AND inbox_id=? AND id=? AND snippet_pending=1`, messagepreview.Text(preview), account, inbox, id)
	if err != nil {
		return err
	}
	if err := syncRemoteMetadataStorageTx(ctx, tx, account, inbox, before); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordedRemoteDirection uses confirmed provider IDs or APPENDUID locators,
// never caller-controlled From/Message-ID headers, to recognize our own sends.
func (s *Store) RecordedRemoteDirection(ctx context.Context, rec RemoteMessage, providerID string) string {
	var known bool
	if providerID != "" {
		err := s.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE account_id=? AND inbox_id=? AND internal=0 AND direction='outbound' AND status='sent' AND provider_message_id=?)`, rec.AccountID, rec.InboxID, providerID).Scan(&known)
		if err == nil && known {
			return model.DirectionOutbound
		}
	}
	if rec.UID != 0 && rec.UIDValidity != 0 {
		err := s.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM remote_sent_copies c JOIN messages m ON m.id=c.message_id AND m.account_id=c.account_id AND m.inbox_id=c.inbox_id WHERE c.account_id=? AND c.inbox_id=? AND c.state='copied' AND c.folder_path=? AND c.remote_uid_validity=? AND c.remote_uid=? AND m.direction='outbound' AND m.status='sent')`, rec.AccountID, rec.InboxID, rec.FolderPath, rec.UIDValidity, rec.UID).Scan(&known)
		if err == nil && known {
			return model.DirectionOutbound
		}
	}
	return ""
}
