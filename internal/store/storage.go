package store

import (
	"context"
	"database/sql"

	"github.com/dellarb/mailmoose/internal/model"
)

// adjustStorageTx applies a storage delta to an account and, when the owning
// inbox is known, to that inbox, inside a transaction. Positive deltas enforce
// both the account quota and the inbox's own quota (when set); negative deltas
// floor each counter at zero.
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

// inboxUsedTx returns the inbox's maintained storage counter. The baseline
// schema keeps storage_used_bytes NOT NULL DEFAULT 0, so no lazy initialization
// is needed.
func inboxUsedTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string) (int64, error) {
	var used int64
	err := tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&used)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return used, err
}

// Remote metadata counts toward storage exactly like a stored message does: the
// bytes we actually persist for a standalone (remote) inbox. A remote body is
// never archived, so a row's size_bytes column is a claim about the remote
// server and is deliberately excluded; only the header/thread/folder/label text
// we store is charged.
//
// The SQL expressions below are the authoritative definition (they read the
// stored values, so an update to any stored field is reflected immediately).
// storedRemoteMessageBytes is a Go mirror used only for the best-effort cap
// admission estimate; it need not match the SQL exactly, because the committed
// counter is always reconciled from the SQL sum. The per-row overheads cover the
// integer ids, timestamps and small flag columns that are not summed explicitly.
const (
	// remoteMessageRowOverhead is the fixed per-message-row allowance baked into
	// remoteMessageBytesSQL (integer ids, timestamps, small flag columns).
	remoteMessageRowOverhead = 96
	remoteLabelRowOverhead   = 32

	// remoteMessageBytesSQL is the authoritative byte footprint of one cached
	// message row. Individual mutations read the old and new expression for the
	// affected row and apply the difference, so a small update never scans the
	// whole mailbox.
	remoteMessageBytesSQL = `(
		LENGTH(CAST(folder_path AS BLOB)) + LENGTH(CAST(rfc_message_id AS BLOB)) +
		LENGTH(CAST(in_reply_to AS BLOB)) + LENGTH(CAST(references_json AS BLOB)) +
		LENGTH(CAST(thread_key AS BLOB)) + LENGTH(CAST(from_name AS BLOB)) +
		LENGTH(CAST(from_address AS BLOB)) + LENGTH(CAST(to_json AS BLOB)) +
		LENGTH(CAST(cc_json AS BLOB)) + LENGTH(CAST(subject AS BLOB)) +
		LENGTH(CAST(snippet AS BLOB)) + LENGTH(CAST(flags_json AS BLOB)) + 96)`

	remoteFolderBytesSQL = `(
		LENGTH(CAST(path AS BLOB)) + LENGTH(CAST(name AS BLOB)) +
		LENGTH(CAST(parent_path AS BLOB)) + LENGTH(CAST(role AS BLOB)) +
		LENGTH(CAST(remote_metadata_json AS BLOB)) + 64)`

	remoteLabelBytesSQL = `(LENGTH(CAST(label AS BLOB)) + 32)` // remoteLabelRowOverhead

	// The metadata sum uses the same row expressions, so incremental deltas and
	// the authoritative check share one definition.
	remoteMetadataMessageBytesSQL = remoteMessageBytesSQL
	remoteMetadataFolderBytesSQL  = remoteFolderBytesSQL
	remoteMetadataLabelBytesSQL   = remoteLabelBytesSQL
)

// storedRemoteMessageBytes is the Go mirror of remoteMetadataMessageBytesSQL: the
// local footprint of one inbox_remote_messages row from its stored text fields.
func storedRemoteMessageBytes(folderPath, rfcMessageID, inReplyTo, referencesJSON, threadKey, fromName, fromAddress, toJSON, ccJSON, subject, snippet, flagsJSON string) int64 {
	return int64(len(folderPath)+len(rfcMessageID)+len(inReplyTo)+len(referencesJSON)+len(threadKey)+
		len(fromName)+len(fromAddress)+len(toJSON)+len(ccJSON)+len(subject)+len(snippet)+len(flagsJSON)) +
		remoteMessageRowOverhead
}

// remoteMessageInputBytes estimates the stored footprint of a message about to be
// written. ThreadKey is resolved by the caller and may differ from in.ThreadKey,
// so this is an estimate used only for the best-effort cap check; the committed
// counter is always reconciled from the authoritative SQL sum.
func remoteMessageInputBytes(in RemoteMessageInput) int64 {
	return storedRemoteMessageBytes(in.FolderPath, in.RFCMessageID, in.InReplyTo,
		jsonString(in.References), in.ThreadKey, in.FromName, in.FromAddress,
		jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, jsonString(in.Flags))
}

// remoteMetadataBytesTx sums the stored bytes of an inbox's remote metadata: its
// cached header/thread rows, its remote-owned folders and its local labels.
func remoteMetadataBytesTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string) (int64, error) {
	var messageBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteMetadataMessageBytesSQL+`),0) FROM inbox_remote_messages WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&messageBytes); err != nil {
		return 0, err
	}
	var folderBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteMetadataFolderBytesSQL+`),0) FROM inbox_folders WHERE account_id=? AND inbox_id=? AND origin='remote'`, accountID, inboxID).Scan(&folderBytes); err != nil {
		return 0, err
	}
	var labelBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteMetadataLabelBytesSQL+`),0) FROM inbox_remote_labels WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&labelBytes); err != nil {
		return 0, err
	}
	return messageBytes + folderBytes + labelBytes, nil
}

// RemoteMetadataBytes returns the authoritative stored-byte total of an inbox's
// remote metadata (messages, remote-owned folders and labels). Normal mutations
// apply per-row deltas; this is the check/repair reference for drift and for
// tests that compare the incremental counters with the stored truth.
func (s *Store) RemoteMetadataBytes(ctx context.Context, accountID, inboxID string) (int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	return remoteMetadataBytesTx(ctx, tx, accountID, inboxID)
}

// remoteMessageBytesTx returns one cached message row's stored bytes and whether
// it exists.
func remoteMessageBytesTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, id string) (int64, bool, error) {
	var b int64
	err := tx.QueryRowContext(ctx, `SELECT `+remoteMessageBytesSQL+` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, id).Scan(&b)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return b, true, nil
}

// remoteMessageBytesByUIDTx is remoteMessageBytesTx keyed by the message's
// remote locator, the identity an upsert conflicts on.
func remoteMessageBytesByUIDTx(ctx context.Context, tx *sql.Tx, inboxID, folderPath string, uidValidity, uid uint32) (int64, bool, error) {
	var b int64
	err := tx.QueryRowContext(ctx, `SELECT `+remoteMessageBytesSQL+` FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? AND remote_uid_validity=? AND remote_uid=?`, inboxID, folderPath, uidValidity, uid).Scan(&b)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return b, true, nil
}

// remoteFolderBytesByIDTx returns one folder row's stored bytes, matched by id.
func remoteFolderBytesByIDTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, folderID string) (int64, error) {
	var b int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteFolderBytesSQL+`),0) FROM inbox_folders WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, folderID).Scan(&b)
	return b, err
}

// remoteLabelBytesTx returns the stored bytes of one message's local labels.
func remoteLabelBytesTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, messageID string) (int64, error) {
	var b int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteLabelBytesSQL+`),0) FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? AND remote_message_id=?`, accountID, inboxID, messageID).Scan(&b)
	return b, err
}

// remoteLabelsForMessageBytes is the Go mirror of remoteLabelBytesSQL for a
// replacement label set that has not been written yet.
func remoteLabelsForMessageBytes(labels []string) int64 {
	var total int64
	for _, label := range labels {
		total += int64(len(label)) + remoteLabelRowOverhead
	}
	return total
}

// rewriteThreadKeyTx moves every row in one inbox from one thread key to
// another and returns the byte delta the rename applied to the metadata
// counters. Thread-key length is part of the authoritative footprint, so a
// merge must adjust the counters even though only the key changed.
func rewriteThreadKeyTx(ctx context.Context, q threadQueryer, inboxID, from, to string) (int64, error) {
	if from == to {
		return 0, nil
	}
	var n int
	var oldBytes int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(thread_key AS BLOB))),0) FROM inbox_remote_messages WHERE inbox_id=? AND thread_key=?`, inboxID, from).Scan(&n, &oldBytes); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if _, err := q.ExecContext(ctx, `UPDATE inbox_remote_messages SET thread_key=? WHERE inbox_id=? AND thread_key=?`, to, inboxID, from); err != nil {
		return 0, err
	}
	return int64(n)*int64(len(to)) - oldBytes, nil
}

// remoteMetadataHeadroomTx reports how many remote-metadata bytes an inbox can
// still take before either the account or its own cap is reached. A room value is
// only meaningful when its limited flag is true; when false the corresponding cap
// is absent (NULL, 0 = unlimited) and growth is unbounded.
func remoteMetadataHeadroomTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string) (accountRoom int64, accountLimited bool, inboxRoom int64, inboxLimited bool, err error) {
	var accountQuota, accountUsed int64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, accountID).Scan(&accountQuota, &accountUsed); err != nil {
		return 0, false, 0, false, err
	}
	if accountQuota > 0 {
		accountLimited = true
		accountRoom = accountQuota - accountUsed
	}
	var inboxQuota, inboxUsed sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&inboxQuota, &inboxUsed); err != nil {
		return 0, false, 0, false, err
	}
	if inboxQuota.Valid && inboxQuota.Int64 > 0 {
		inboxLimited = true
		inboxRoom = inboxQuota.Int64 - inboxUsed.Int64
	}
	return accountRoom, accountLimited, inboxRoom, inboxLimited, nil
}

// roomForRemoteMetadata reports whether extra more bytes fit under both caps. A
// room value is only consulted when its cap is present; a negative room (already
// over cap) refuses growth, so an over-cap inbox stays over but never grows.
func roomForRemoteMetadata(accountRoom int64, accountLimited bool, inboxRoom int64, inboxLimited bool, extra int64) bool {
	return (!accountLimited || extra <= accountRoom) && (!inboxLimited || extra <= inboxRoom)
}

// applyRemoteMetadataDeltaTx applies a remote-metadata byte delta to the account
// and inbox storage counters. Unlike adjustStorageTx it never fails on cap:
// remote metadata is a derivative cache, so growth past the cap is recorded and
// the caller stops indexing rather than erroring. Negative deltas refund, floored
// at zero.
func applyRemoteMetadataDeltaTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string, delta int64) error {
	if delta == 0 {
		return nil
	}
	if inboxID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=MAX(0,storage_used_bytes+?) WHERE id=? AND account_id=?`, delta, inboxID, accountID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes+?) WHERE id=?`, delta, accountID)
	return err
}

// syncRemoteMetadataStorageTx reconciles the storage counters for an inbox's
// remote metadata after a mutation. before is the byte total captured at the
// start of the transaction; the difference is applied to the counters. This is
// self-correcting: the counters track the authoritative stored sum every time a
// remote mutation commits, so drift from a missed write is corrected on the next
// one. Callers that need a hard cap check fetch headroom separately.
func syncRemoteMetadataStorageTx(ctx context.Context, tx *sql.Tx, accountID, inboxID string, before int64) error {
	if inboxID == "" {
		return nil
	}
	after, err := remoteMetadataBytesTx(ctx, tx, accountID, inboxID)
	if err != nil {
		return err
	}
	return applyRemoteMetadataDeltaTx(ctx, tx, accountID, inboxID, after-before)
}

// existingRemoteUIDsTx returns the set of UIDs already cached for a folder
// generation. It is used only when a storage cap is in play, so the reconcile
// can admit updates to rows that already exist (which replace bytes rather than
// grow usage) while refusing brand-new rows once the cap is reached. It is a
// single indexed read scoped to the folder and generation.
func existingRemoteUIDsTx(ctx context.Context, tx *sql.Tx, inboxID, folderPath string, uidValidity uint32) (map[uint32]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT remote_uid FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? AND remote_uid_validity=?`, inboxID, folderPath, uidValidity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]bool{}
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

func draftBodyBytes(d model.Draft) int64 { return int64(len(d.Text) + len(d.HTML)) }

func getDraftTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.Draft, error) {
	d, err := scanDraft(tx.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}
