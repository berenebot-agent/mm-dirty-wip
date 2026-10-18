package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file owns the one generation-guarded write path for a Microsoft folder
// delta page. A page's local effects (metadata upserts, local deletions, arrival
// records and detected marks), its cursor advance and its final prune all commit
// or roll back as a single transaction whose first act is the generation check.
// A resync (a generation bump) between the page fetch and this call therefore
// leaves nothing behind: the page is a superseded sweep and every one of its
// effects, not just its cursor, is dropped. Provider I/O (the Graph delta fetch,
// the live-message lookup for a removal) stays outside the transaction; the
// caller resolves it first and passes the results in.

// MicrosoftDeltaCache records one delta entry's metadata to cache.
type MicrosoftDeltaCache struct {
	// ProviderID is the Graph message id (immutable id).
	ProviderID string
	// Thread is the provider conversation id the local thread key is derived
	// from (ConversationID).
	Thread string
	// FolderID is the folder id the cached row belongs to (the provider's
	// ParentFolderID), recorded on the provider-id mapping.
	FolderID string
	// Input is the normalized header metadata.
	Input RemoteMessageInput
	// Labels is the entry's full current label set, replacing the row's labels
	// (an empty or nil set clears them), mirroring the provider's categories.
	Labels []string
}

// MicrosoftDeltaEffect is one delta entry's local write for a page.
type MicrosoftDeltaEffect struct {
	// ProviderID is the Graph message id the effect addresses.
	ProviderID string
	// Cache is the metadata upsert for a present (or moved-back) message. Nil
	// when the entry only deletes or detects.
	Cache *MicrosoftDeltaCache
	// Delete removes the local metadata row of a message the provider reports as
	// removed and no longer serves.
	Delete bool
	// Detect records the arrival (when the message was not already detected) and
	// marks the row detected. Only the caller decides *which* entries are
	// detectable (a new arrival in the Inbox of an already-initialised sweep);
	// the store owns the "already detected" read, so the check and the mark can
	// never be interleaved by a concurrent page.
	Detect bool
	// Arrival is the arrival payload used when Detect is set.
	Arrival RemoteArrivalInput
}

// MicrosoftDeltaPage is one folder delta page's writes. The caller has already
// read the folder's current cursor and generation and fetched the page; every
// value here is applied atomically, or not at all.
type MicrosoftDeltaPage struct {
	AccountID string
	InboxID   string
	FolderID  string
	// Generation is the generation the caller read the cursor from. The page is
	// refused with ErrStaleCursor when the row's generation has since moved.
	Generation string
	// Initialized is the value to persist alongside the cursor when the page is
	// applied.
	Initialized bool
	// Cursor is the cursor to persist (never empty: a page without a next or
	// delta link is a caller error).
	Cursor  string
	Effects []MicrosoftDeltaEffect
	// Prune removes the folder's superseded-generation rows, and only when this
	// page is the final page of a complete sweep (the caller sets it from the
	// absence of a NextLink). It runs in the same transaction as the cursor
	// advance, so a resync can never interleave between the advance and the prune.
	Prune bool
}

// MicrosoftDeltaPageResult reports what one applied page actually persisted.
type MicrosoftDeltaPageResult struct {
	// Arrived is the number of newly inserted arrivals.
	Arrived int
	// Pruned is the number of superseded-generation metadata rows removed.
	Pruned int
	// Arrivals is retained for diagnostics/tests; publication is owned by the
	// remote worker, which drains arrivals and emits their events once.
	Arrivals []RemoteArrival
}

// ApplyMicrosoftDeltaPage applies a delta page's local effects, cursor advance
// and final prune as one generation-guarded transaction.
//
// ErrStaleCursor: the folder's generation no longer matches the page's, so a
// resync superseded the sweep the page belongs to. Nothing is written — no
// metadata, no deletion, no arrival, no detected mark, no cursor, no prune — and
// the caller must report the page incomplete rather than advance.
//
// ErrRemoteMetadataQuota: a new metadata row did not fit under the account or
// inbox cap. The whole page rolls back for the same reason (the next pass
// replays it from the unchanged cursor), and the caller stops indexing.
func (s *Store) ApplyMicrosoftDeltaPage(ctx context.Context, page MicrosoftDeltaPage) (MicrosoftDeltaPageResult, error) {
	var result MicrosoftDeltaPageResult
	if page.Cursor == "" {
		return result, errors.New("microsoft delta page requires a cursor")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// The generation check is the transaction's first act: everything below runs
	// only for the sweep that is still current.
	rowExists, generation, err := microsoftDeltaGenerationTx(ctx, tx, page.InboxID, page.FolderID)
	if err != nil {
		return result, err
	}
	if !rowExists && page.Generation != "" {
		return result, ErrStaleCursor
	}
	if rowExists && generation != page.Generation {
		return result, ErrStaleCursor
	}
	metaDelta := int64(0)
	accountRoom, accountLimited, inboxRoom, inboxLimited, err := remoteMetadataHeadroomTx(ctx, tx, page.AccountID, page.InboxID)
	if err != nil {
		return result, err
	}
	usedThisBatch := int64(0)
	for _, e := range page.Effects {
		if e.Detect {
			detected, err := microsoftDetectedTx(ctx, tx, page.InboxID, e.ProviderID)
			if err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			if !detected {
				arrival, inserted, err := recordRemoteArrivalTx(ctx, tx, page.AccountID, page.InboxID, e.Arrival)
				if err != nil {
					return MicrosoftDeltaPageResult{}, err
				}
				if inserted {
					// The detection event is NOT written here: the caller
					// publishes arrivals through RecordRemoteArrivalEvent, the
					// same path every other provider uses, so the event is
					// emitted once and by its owning component.
					result.Arrivals = append(result.Arrivals, arrival)
					result.Arrived++
				}
			}
		}
		if e.Cache != nil {
			id := microsoftMessageID(e.Cache.ProviderID, page.InboxID)
			exists, err := microsoftMessageRowExistsTx(ctx, tx, page.AccountID, page.InboxID, id)
			if err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			add := remoteMessageInputBytes(e.Cache.Input)
			if !exists && (accountLimited || inboxLimited) && !roomForRemoteMetadata(accountRoom, accountLimited, inboxRoom, inboxLimited, usedThisBatch+add) {
				// The page's metadata is a derivative cache: a refusal stops the
				// page (the caller pauses the sync) rather than failing it, and
				// rolling the page back keeps cursor and metadata consistent.
				return MicrosoftDeltaPageResult{}, ErrRemoteMetadataQuota
			}
			if !exists {
				usedThisBatch += add
			}
			oldBytes, _, err := remoteMessageBytesTx(ctx, tx, page.AccountID, page.InboxID, id)
			if err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			oldLabels, err := remoteLabelBytesTx(ctx, tx, page.AccountID, page.InboxID, id)
			if err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			if err := upsertMicrosoftMessageTx(ctx, tx, page.AccountID, page.InboxID, e.Cache); err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			// Labels are the entry's full current set, exactly as the provider
			// reports them, so a removed category is removed locally too.
			cleanLabels := normalizeRemoteLabels(e.Cache.Labels)
			if err := setRemoteMessageLabelsTx(ctx, tx, page.AccountID, page.InboxID, id, cleanLabels); err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			newBytes, _, err := remoteMessageBytesTx(ctx, tx, page.AccountID, page.InboxID, id)
			if err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
			metaDelta += newBytes - oldBytes + remoteLabelsForMessageBytes(cleanLabels) - oldLabels
		}
		if e.Delete {
			id, err := microsoftLocalIDTx(ctx, tx, page.AccountID, page.InboxID, e.ProviderID)
			switch {
			case errors.Is(err, ErrNotFound):
				// The page names a provider id this inbox never mapped (or a
				// concurrent sweep already removed): nothing local to delete.
			case err != nil:
				return MicrosoftDeltaPageResult{}, err
			default:
				messageBytes, _, berr := remoteMessageBytesTx(ctx, tx, page.AccountID, page.InboxID, id)
				if berr != nil {
					return MicrosoftDeltaPageResult{}, berr
				}
				labelBytes, berr := remoteLabelBytesTx(ctx, tx, page.AccountID, page.InboxID, id)
				if berr != nil {
					return MicrosoftDeltaPageResult{}, berr
				}
				if err := deleteRemoteMessageTx(ctx, tx, page.AccountID, page.InboxID, id); err != nil {
					return MicrosoftDeltaPageResult{}, err
				}
				metaDelta -= messageBytes + labelBytes
			}
		}
		if e.Cache != nil {
			// Every cached message is marked detected, not only the ones this
			// page raises an arrival for: the mark is what stops a later sweep
			// from treating a message seen during the silent baseline as new
			// mail. The arrival itself is only written when the caller sets
			// Detect (a message genuinely new to an initialised sweep).
			if err := markMicrosoftDetectedTx(ctx, tx, page.InboxID, e.ProviderID); err != nil {
				return MicrosoftDeltaPageResult{}, err
			}
		}
	}
	if !rowExists {
		_, err = tx.ExecContext(ctx, `INSERT INTO microsoft_delta(inbox_id,folder_id,cursor,initialized,generation) VALUES(?,?,?,?,'')`, page.InboxID, page.FolderID, page.Cursor, page.Initialized)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE microsoft_delta SET cursor=?,initialized=? WHERE inbox_id=? AND folder_id=?`, page.Cursor, page.Initialized, page.InboxID, page.FolderID)
	}
	if err != nil {
		return MicrosoftDeltaPageResult{}, err
	}
	if page.Prune {
		pruned, prunedBytes, err := pruneMicrosoftResyncTx(ctx, tx, page.AccountID, page.InboxID, page.FolderID)
		if err != nil {
			return MicrosoftDeltaPageResult{}, err
		}
		result.Pruned = pruned
		metaDelta -= prunedBytes
	}
	if err = applyRemoteMetadataDeltaTx(ctx, tx, page.AccountID, page.InboxID, metaDelta); err != nil {
		return MicrosoftDeltaPageResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return MicrosoftDeltaPageResult{}, err
	}
	return result, nil
}

// microsoftDeltaGenerationTx reads a folder's delta row generation. rowExists is
// false when the folder has no delta row yet (a first sweep).
func microsoftDeltaGenerationTx(ctx context.Context, tx *sql.Tx, inbox, folder string) (rowExists bool, generation string, err error) {
	var gen sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT generation FROM microsoft_delta WHERE inbox_id=? AND folder_id=?`, inbox, folder).Scan(&gen)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, gen.String, nil
}

// microsoftDetectedTx reports whether a provider message id is already marked
// detected. An unknown id is not detected.
func microsoftDetectedTx(ctx context.Context, tx *sql.Tx, inbox, provider string) (bool, error) {
	var detected bool
	err := tx.QueryRowContext(ctx, `SELECT detected FROM microsoft_message_ids WHERE inbox_id=? AND provider_id=?`, inbox, provider).Scan(&detected)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return detected, err
}

// markMicrosoftDetectedTx sets a provider message id's detected mark.
func markMicrosoftDetectedTx(ctx context.Context, tx *sql.Tx, inbox, provider string) error {
	_, err := tx.ExecContext(ctx, `UPDATE microsoft_message_ids SET detected=1 WHERE inbox_id=? AND provider_id=?`, inbox, provider)
	return err
}

// microsoftLocalIDTx resolves a provider message id to its local metadata row id.
func microsoftLocalIDTx(ctx context.Context, tx *sql.Tx, account, inbox, provider string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT m.id FROM microsoft_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.provider_id=?`, account, inbox, provider).Scan(&id)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return id, err
}

// microsoftMessageRowExistsTx reports whether a metadata row id is present.
func microsoftMessageRowExistsTx(ctx context.Context, tx *sql.Tx, account, inbox, id string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_remote_messages WHERE id=? AND account_id=? AND inbox_id=?`, id, account, inbox).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// microsoftMessageID is the local metadata row id derived from a provider id.
func microsoftMessageID(provider, inbox string) string {
	return "ms_" + provider + "_" + inbox
}

// upsertMicrosoftMessageTx writes one Microsoft message's metadata row and its
// provider-id mapping inside the caller's transaction. The caller owns the
// metadata cap check and the storage-counter reconciliation, because a delta page
// charges both once for the whole page.
func upsertMicrosoftMessageTx(ctx context.Context, tx *sql.Tx, account, inbox string, c *MicrosoftDeltaCache) error {
	id := microsoftMessageID(c.ProviderID, inbox)
	// The cache row is created only for a connected inbox. The caller has already
	// verified the connection, so a missing row here is a real fault rather than
	// an acceptable partial write, and a silent failure would leave the UPDATE
	// below matching nothing.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid,created_at,updated_at) SELECT ?,account_id,inbox_id,?,0,?,? FROM inbox_microsoft_connections WHERE account_id=? AND inbox_id=?`, id, "microsoft:"+c.ProviderID, nowText(), nowText(), account, inbox); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_remote_messages WHERE id=? AND account_id=? AND inbox_id=?`, id, account, inbox).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return ErrNotFound
	}
	in := c.Input
	now := nowText()
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_remote_messages SET rfc_message_id=?,in_reply_to=?,references_json=?,thread_key=?,from_name=?,from_address=?,to_json=?,cc_json=?,subject=?,snippet=?,size_bytes=?,has_attachments=?,is_read=?,is_flagged=?,is_draft=?,received_at=?,internal_date=?,indexed_at=?,updated_at=? WHERE id=? AND account_id=? AND inbox_id=?`, in.RFCMessageID, in.InReplyTo, jsonString(in.References), "microsoft:"+inbox+":"+c.Thread, in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, in.HasAttach, in.Read, in.Flagged, in.Draft, nullStringPtr(in.ReceivedAt), nullStringPtr(in.InternalDate), now, now, id, account, inbox); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO microsoft_message_ids(inbox_id,message_id,provider_id,folder_id,generation) VALUES(?,?,?,?,COALESCE((SELECT generation FROM microsoft_delta WHERE inbox_id=? AND folder_id=?),'')) ON CONFLICT(inbox_id,provider_id) DO UPDATE SET folder_id=excluded.folder_id,generation=excluded.generation`, inbox, id, c.ProviderID, c.FolderID, inbox, c.FolderID)
	return err
}

// microsoftPruneIDs selects the cached message ids a superseded Microsoft sweep
// left behind.
const microsoftPruneIDs = `SELECT m.message_id FROM microsoft_message_ids m JOIN microsoft_delta d ON d.inbox_id=m.inbox_id AND d.folder_id=m.folder_id WHERE m.inbox_id=? AND m.folder_id=? AND d.generation<>'' AND m.generation<>d.generation`

// pruneMicrosoftResyncTx removes a folder's cached rows whose recorded generation
// no longer matches the folder's current generation, i.e. the rows a superseded
// sweep left behind. A folder that was never resynced (generation ”) prunes
// nothing. It returns the number of rows removed and the metadata bytes they
// occupied.
func pruneMicrosoftResyncTx(ctx context.Context, tx *sql.Tx, account, inbox, folder string) (int, int64, error) {
	var bytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteMessageBytesSQL+`),0) FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id IN (`+microsoftPruneIDs+`)`, account, inbox, inbox, folder).Scan(&bytes); err != nil {
		return 0, 0, err
	}
	var labelBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+remoteLabelBytesSQL+`),0) FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? AND remote_message_id IN (`+microsoftPruneIDs+`)`, account, inbox, inbox, folder).Scan(&labelBytes); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id IN (`+microsoftPruneIDs+`)`, account, inbox, inbox, folder)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), bytes + labelBytes, nil
}
