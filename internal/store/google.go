package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type GoogleConnection struct {
	InboxID, AccountID, ClientID, EncryptedSecret, EncryptedToken string
	ExpiresAt                                                     time.Time
	HistoryID, BackfillPage                                       string
	BackfillDone                                                  bool
	BackfillOffset                                                int
	BackfillSnapshot                                              string
}

func (s *Store) GoogleConnection(ctx context.Context, accountID, inboxID string) (GoogleConnection, error) {
	var c GoogleConnection
	var expiry string
	e := s.read.QueryRowContext(ctx, `SELECT c.inbox_id,c.account_id,c.client_id,c.encrypted_secret,c.encrypted_token,c.expires_at,g.history_id,g.backfill_page,g.backfill_done FROM inbox_google_connections c JOIN inbox_gmail_sync g ON g.inbox_id=c.inbox_id WHERE c.account_id=? AND c.inbox_id=?`, accountID, inboxID).Scan(&c.InboxID, &c.AccountID, &c.ClientID, &c.EncryptedSecret, &c.EncryptedToken, &expiry, &c.HistoryID, &c.BackfillPage, &c.BackfillDone)
	if e == sql.ErrNoRows {
		return c, ErrNotFound
	}
	c.ExpiresAt = parseTime(expiry)
	return c, e
}
func (s *Store) IsGoogle(ctx context.Context, accountID, inboxID string) bool {
	_, e := s.GoogleConnection(ctx, accountID, inboxID)
	return e == nil || s.IsOne(ctx, accountID, inboxID)
}

func (s *Store) BeginGoogleRefresh(ctx context.Context, account, inbox string) error {
	_, e := s.write.ExecContext(ctx, `UPDATE inboxes SET remote_index_status='partial' WHERE account_id=? AND id=?`, account, inbox)
	return e
}
func (s *Store) SaveGoogleConnection(ctx context.Context, c GoogleConnection) error {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var one int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_one_bindings WHERE inbox_id=?`, c.InboxID).Scan(&one); e != nil {
		return e
	}
	if one != 0 {
		return ErrConflict
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO inbox_google_connections(inbox_id,account_id,client_id,encrypted_secret,encrypted_token,expires_at) SELECT id,account_id,?,?,?,? FROM inboxes WHERE id=? AND account_id=? AND kind='standalone' ON CONFLICT(inbox_id) DO UPDATE SET client_id=excluded.client_id,encrypted_secret=excluded.encrypted_secret,encrypted_token=excluded.encrypted_token,expires_at=excluded.expires_at`, c.ClientID, c.EncryptedSecret, c.EncryptedToken, c.ExpiresAt.UTC().Format(time.RFC3339Nano), c.InboxID, c.AccountID)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_gmail_sync(inbox_id,account_id) SELECT id,account_id FROM inboxes WHERE id=? AND account_id=?`, c.InboxID, c.AccountID); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE inboxes SET remote_configured=1,remote_host='gmail.googleapis.com',remote_username=address,remote_security='tls',remote_sent_copy_enabled=0 WHERE id=? AND account_id=? AND kind='standalone'`, c.InboxID, c.AccountID)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) SaveGoogleProgress(ctx context.Context, accountID, inboxID, history, page string, done bool) error {
	_, e := s.write.ExecContext(ctx, `UPDATE inbox_gmail_sync SET history_id=?,backfill_offset=CASE WHEN backfill_page<>? OR ? THEN 0 ELSE backfill_offset END,backfill_snapshot=CASE WHEN backfill_page<>? OR ? THEN '' ELSE backfill_snapshot END,backfill_page=?,backfill_done=? WHERE account_id=? AND inbox_id=?`, history, page, done, page, done, page, done, accountID, inboxID)
	return e
}

type GoogleAttempt struct {
	StateHash, AccountID, UserID, InboxID, DisplayName, ClientID, RedirectURI, EncryptedPending string
	ExpiresAt                                                                                   time.Time
}

func (s *Store) SaveGoogleAttempt(ctx context.Context, a GoogleAttempt) error {
	_, e := s.write.ExecContext(ctx, `DELETE FROM google_oauth_attempts WHERE expires_at<?`, nowText())
	if e != nil {
		return e
	}
	_, e = s.write.ExecContext(ctx, `INSERT INTO google_oauth_attempts VALUES(?,?,?,?,?,?,?,?,?)`, a.StateHash, a.AccountID, a.UserID, a.InboxID, a.DisplayName, a.ClientID, a.RedirectURI, a.EncryptedPending, a.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) ConsumeGoogleAttempt(ctx context.Context, hash, account, user string) (GoogleAttempt, error) {
	var a GoogleAttempt
	var expiry string
	e := s.write.QueryRowContext(ctx, `DELETE FROM google_oauth_attempts WHERE state_hash=? AND account_id=? AND user_id=? AND expires_at>? RETURNING state_hash,account_id,user_id,inbox_id,display_name,client_id,redirect_uri,encrypted_pending,expires_at`, hash, account, user, nowText()).Scan(&a.StateHash, &a.AccountID, &a.UserID, &a.InboxID, &a.DisplayName, &a.ClientID, &a.RedirectURI, &a.EncryptedPending, &expiry)
	if e == sql.ErrNoRows {
		return a, ErrNotFound
	}
	a.ExpiresAt = parseTime(expiry)
	return a, e
}
func (s *Store) GoogleMessage(ctx context.Context, account, inbox, id string) (provider, thread string, labels []string, err error) {
	var raw string
	err = s.read.QueryRowContext(ctx, `SELECT g.provider_id,g.thread_id,g.labels_json FROM google_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.message_id=?`, account, inbox, id).Scan(&provider, &thread, &raw)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	labels = decodeStrings(raw)
	return
}
func (s *Store) GoogleLocalID(ctx context.Context, account, inbox, provider string) (string, error) {
	var id string
	e := s.read.QueryRowContext(ctx, `SELECT g.message_id FROM google_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.provider_id=?`, account, inbox, provider).Scan(&id)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	return id, e
}

func (s *Store) GoogleDetection(ctx context.Context, inbox string) (string, error) {
	var h string
	e := s.read.QueryRowContext(ctx, `SELECT history_id FROM google_detection WHERE inbox_id=?`, inbox).Scan(&h)
	if e == sql.ErrNoRows {
		return "", nil
	}
	return h, e
}
func (s *Store) SaveGoogleDetection(ctx context.Context, inbox, history string) error {
	_, e := s.write.ExecContext(ctx, `INSERT INTO google_detection VALUES(?,?) ON CONFLICT(inbox_id) DO UPDATE SET history_id=excluded.history_id`, inbox, history)
	return e
}

// UpsertGoogleMessage keeps one cache record per provider message, independently
// of its many label memberships. IMAP UID fields remain unused for Google.
func (s *Store) UpsertGoogleMessage(ctx context.Context, account, inbox, provider, thread string, labels []string, in RemoteMessageInput) (RemoteMessage, error) {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return RemoteMessage{}, e
	}
	defer tx.Rollback()
	metaBefore, e := remoteMetadataBytesTx(ctx, tx, account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	accountRoom, accountLimited, inboxRoom, inboxLimited, e := remoteMetadataHeadroomTx(ctx, tx, account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	var id string
	e = tx.QueryRowContext(ctx, `SELECT message_id FROM google_message_ids WHERE inbox_id=? AND provider_id=?`, inbox, provider).Scan(&id)
	if e != nil && e != sql.ErrNoRows {
		return RemoteMessage{}, e
	}
	if e == sql.ErrNoRows {
		// A new Gmail message is a genuinely new metadata row: refuse it when a
		// cap is set and there is no room, so indexing stops rather than growing
		// past the cap. An existing message (e == nil) updates in place.
		if (accountLimited || inboxLimited) && !roomForRemoteMetadata(accountRoom, accountLimited, inboxRoom, inboxLimited, remoteMessageInputBytes(in)) {
			return RemoteMessage{}, ErrRemoteMetadataQuota
		}
		// The legacy unique IMAP tuple must not alias distinct Gmail messages.
		// An opaque provider-specific path namespaces it; label views use the
		// membership table, never this storage locator.
		id = "gm_" + provider + "_" + inbox
		_, e = tx.ExecContext(ctx, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid,created_at,updated_at) VALUES(?,?,?,?,0,?,?)`, id, account, inbox, "gmail:"+provider, nowText(), nowText())
		if e != nil {
			return RemoteMessage{}, e
		}
	}
	now := nowText()
	_, e = tx.ExecContext(ctx, `UPDATE inbox_remote_messages SET rfc_message_id=?,in_reply_to=?,references_json=?,thread_key=?,from_name=?,from_address=?,to_json=?,cc_json=?,subject=?,snippet=?,size_bytes=?,has_attachments=?,is_read=?,is_flagged=?,is_draft=?,received_at=?,internal_date=?,indexed_at=?,created_at=CASE WHEN created_at='' THEN ? ELSE created_at END,updated_at=? WHERE id=? AND account_id=? AND inbox_id=?`, in.RFCMessageID, in.InReplyTo, jsonString(in.References), "gmail:"+inbox+":"+thread, in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, in.HasAttach, in.Read, in.Flagged, in.Draft, nullStringPtr(in.ReceivedAt), nullStringPtr(in.InternalDate), now, now, now, id, account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	b, _ := json.Marshal(labels)
	_, e = tx.ExecContext(ctx, `INSERT INTO google_message_ids(inbox_id,message_id,provider_id,thread_id,labels_json,sync_generation) VALUES(?,?,?,?,?,COALESCE((SELECT sync_generation FROM inbox_gmail_sync WHERE inbox_id=?),'')) ON CONFLICT(inbox_id,provider_id) DO UPDATE SET labels_json=excluded.labels_json,thread_id=excluded.thread_id,sync_generation=excluded.sync_generation`, inbox, id, provider, thread, string(b), inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	_, e = tx.ExecContext(ctx, `DELETE FROM google_label_memberships WHERE message_id=?`, id)
	if e != nil {
		return RemoteMessage{}, e
	}
	for _, l := range labels {
		_, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO google_label_memberships VALUES(?,?,?)`, id, inbox, l)
		if e != nil {
			return RemoteMessage{}, e
		}
	}
	if e = syncRemoteMetadataStorageTx(ctx, tx, account, inbox, metaBefore); e != nil {
		return RemoteMessage{}, e
	}
	if e = tx.Commit(); e != nil {
		return RemoteMessage{}, e
	}
	return s.GetRemoteMessage(ctx, account, inbox, id)
}

func (s *Store) StartGoogleResync(ctx context.Context, account, inbox, history string) error {
	_, e := s.write.ExecContext(ctx, `UPDATE inbox_gmail_sync SET history_id=?,backfill_page='',backfill_done=0,backfill_offset=0,backfill_snapshot='',sync_generation=? WHERE account_id=? AND inbox_id=?`, history, nowText(), account, inbox)
	return e
}

// Prune only after a complete enumeration. An interrupted resync retains old
// metadata and partial completeness, rather than deleting unseen messages.
func (s *Store) PruneGoogleResync(ctx context.Context, account, inbox string) error {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	metaBefore, e := remoteMetadataBytesTx(ctx, tx, account, inbox)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id IN (SELECT g.message_id FROM google_message_ids g JOIN inbox_gmail_sync c ON c.inbox_id=g.inbox_id WHERE g.inbox_id=? AND c.sync_generation<>'' AND g.sync_generation<>c.sync_generation)`, account, inbox, inbox); e != nil {
		return e
	}
	if e = syncRemoteMetadataStorageTx(ctx, tx, account, inbox, metaBefore); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) RefreshGoogleLabelNames(ctx context.Context, account, inbox string) error {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	metaBefore, e := remoteMetadataBytesTx(ctx, tx, account, inbox)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `DELETE FROM inbox_remote_labels WHERE account_id=? AND inbox_id=?`, account, inbox); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO inbox_remote_labels(id,account_id,inbox_id,remote_message_id,label,created_at) SELECT 'gl_'||g.message_id||'_'||g.label_id,?,?,g.message_id,f.name,? FROM google_label_memberships g JOIN inbox_folders f ON f.inbox_id=g.inbox_id AND f.path=g.label_id WHERE g.inbox_id=? AND f.role='folder'`, account, inbox, nowText(), inbox)
	if e != nil {
		return e
	}
	if e = syncRemoteMetadataStorageTx(ctx, tx, account, inbox, metaBefore); e != nil {
		return e
	}
	return tx.Commit()
}
