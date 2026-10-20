package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) MicrosoftMessageFolder(ctx context.Context, account, inbox, id string) (string, error) {
	var folder string
	e := s.read.QueryRowContext(ctx, `SELECT g.folder_id FROM microsoft_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.message_id=?`, account, inbox, id).Scan(&folder)
	return folder, e
}

type MicrosoftConnection struct {
	AccountID, InboxID, ClientID, Tenant, IdentityID, Mailbox, EncryptedToken string
	Shared                                                                    bool
	ExpiresAt                                                                 time.Time
}

func (s *Store) MicrosoftConnection(ctx context.Context, account, inbox string) (MicrosoftConnection, error) {
	var c MicrosoftConnection
	var expiry string
	e := s.read.QueryRowContext(ctx, `SELECT account_id,inbox_id,client_id,tenant,identity_id,mailbox,shared,encrypted_token,expires_at FROM inbox_microsoft_connections WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&c.AccountID, &c.InboxID, &c.ClientID, &c.Tenant, &c.IdentityID, &c.Mailbox, &c.Shared, &c.EncryptedToken, &expiry)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	c.ExpiresAt = parseTime(expiry)
	return c, e
}
func (s *Store) IsMicrosoft(ctx context.Context, account, inbox string) bool {
	_, e := s.MicrosoftConnection(ctx, account, inbox)
	return e == nil
}
func (s *Store) SaveMicrosoftConnection(ctx context.Context, c MicrosoftConnection) error {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, `INSERT INTO inbox_microsoft_connections SELECT id,account_id,?,?,?,?,?,?,? FROM inboxes WHERE id=? AND account_id=? AND kind='standalone' ON CONFLICT(inbox_id) DO UPDATE SET encrypted_token=excluded.encrypted_token,expires_at=excluded.expires_at WHERE inbox_microsoft_connections.identity_id=excluded.identity_id AND inbox_microsoft_connections.mailbox=excluded.mailbox AND inbox_microsoft_connections.client_id=excluded.client_id AND inbox_microsoft_connections.tenant=excluded.tenant AND inbox_microsoft_connections.shared=excluded.shared`, c.ClientID, c.Tenant, c.IdentityID, c.Mailbox, c.Shared, c.EncryptedToken, c.ExpiresAt.UTC().Format(time.RFC3339Nano), c.InboxID, c.AccountID)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `UPDATE inboxes SET remote_configured=1,remote_host='graph.microsoft.com',remote_username=address,remote_security='tls',remote_sent_copy_enabled=0 WHERE id=? AND account_id=? AND kind='standalone'`, c.InboxID, c.AccountID)
	if e != nil {
		return e
	}
	return tx.Commit()
}

type MicrosoftAttempt struct {
	StateHash, AccountID, UserID, InboxID, DisplayName, ClientID, Tenant, Mailbox, EncryptedPending, EncryptedToken string
	Shared                                                                                                          bool
	ExpiresAt                                                                                                       time.Time
}

func (s *Store) SaveMicrosoftAttempt(ctx context.Context, a MicrosoftAttempt) error {
	_, e := s.write.ExecContext(ctx, `DELETE FROM microsoft_oauth_attempts WHERE expires_at<?`, nowText())
	if e != nil {
		return e
	}
	_, e = s.write.ExecContext(ctx, `INSERT INTO microsoft_oauth_attempts(state_hash,account_id,user_id,inbox_id,display_name,client_id,tenant,mailbox,shared,encrypted_pending,expires_at,encrypted_token) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, a.StateHash, a.AccountID, a.UserID, a.InboxID, a.DisplayName, a.ClientID, a.Tenant, a.Mailbox, a.Shared, a.EncryptedPending, a.ExpiresAt.UTC().Format(time.RFC3339Nano), a.EncryptedToken)
	return e
}

// PeekMicrosoftAttempt reads an unexpired attempt without consuming it, so a
// rejected shared-mailbox selection can be retried with the same sign-in. The
// caller must still ConsumeMicrosoftAttempt before acting, which is the
// single-use gate.
func (s *Store) PeekMicrosoftAttempt(ctx context.Context, hash, account, user string) (MicrosoftAttempt, error) {
	var a MicrosoftAttempt
	var expiry string
	e := s.read.QueryRowContext(ctx, `SELECT state_hash,account_id,user_id,inbox_id,display_name,client_id,tenant,mailbox,shared,encrypted_pending,expires_at,encrypted_token FROM microsoft_oauth_attempts WHERE state_hash=? AND account_id=? AND user_id=? AND expires_at>?`, hash, account, user, nowText()).Scan(&a.StateHash, &a.AccountID, &a.UserID, &a.InboxID, &a.DisplayName, &a.ClientID, &a.Tenant, &a.Mailbox, &a.Shared, &a.EncryptedPending, &expiry, &a.EncryptedToken)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	a.ExpiresAt = parseTime(expiry)
	return a, e
}

func (s *Store) ConsumeMicrosoftAttempt(ctx context.Context, hash, account, user string) (MicrosoftAttempt, error) {
	var a MicrosoftAttempt
	var expiry string
	e := s.write.QueryRowContext(ctx, `DELETE FROM microsoft_oauth_attempts WHERE state_hash=? AND account_id=? AND user_id=? AND expires_at>? RETURNING state_hash,account_id,user_id,inbox_id,display_name,client_id,tenant,mailbox,shared,encrypted_pending,expires_at,encrypted_token`, hash, account, user, nowText()).Scan(&a.StateHash, &a.AccountID, &a.UserID, &a.InboxID, &a.DisplayName, &a.ClientID, &a.Tenant, &a.Mailbox, &a.Shared, &a.EncryptedPending, &expiry, &a.EncryptedToken)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	a.ExpiresAt = parseTime(expiry)
	return a, e
}
func (s *Store) MicrosoftMessage(ctx context.Context, account, inbox, id string) (string, error) {
	var provider string
	e := s.read.QueryRowContext(ctx, `SELECT g.provider_id FROM microsoft_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.message_id=?`, account, inbox, id).Scan(&provider)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	return provider, e
}
func (s *Store) MicrosoftLocalID(ctx context.Context, account, inbox, provider string) (string, error) {
	var id string
	e := s.read.QueryRowContext(ctx, `SELECT g.message_id FROM microsoft_message_ids g JOIN inbox_remote_messages m ON m.id=g.message_id WHERE m.account_id=? AND g.inbox_id=? AND g.provider_id=?`, account, inbox, provider).Scan(&id)
	if e == sql.ErrNoRows {
		e = ErrNotFound
	}
	return id, e
}

// Provider identity is independent of folder membership; moves preserve local IDs.
func (s *Store) UpsertMicrosoftMessage(ctx context.Context, account, inbox, provider, thread, folder string, in RemoteMessageInput) (RemoteMessage, error) {
	tx, e := s.write.BeginTx(ctx, nil)
	if e != nil {
		return RemoteMessage{}, e
	}
	defer tx.Rollback()
	cache := &MicrosoftDeltaCache{ProviderID: provider, Thread: thread, FolderID: folder, Input: in}
	id, e := microsoftResolveRowTx(ctx, tx, account, inbox, cache)
	if e != nil {
		return RemoteMessage{}, e
	}
	oldBytes, _, e := remoteMessageBytesTx(ctx, tx, account, inbox, id)
	if e != nil {
		return RemoteMessage{}, e
	}
	// A new Microsoft message is a genuinely new metadata row: refuse it when a
	// cap is set and there is no room. An existing row updates in place.
	accountRoom, accountLimited, inboxRoom, inboxLimited, e := remoteMetadataHeadroomTx(ctx, tx, account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	exists, e := microsoftMessageRowExistsTx(ctx, tx, account, inbox, id)
	if e != nil {
		return RemoteMessage{}, e
	}
	if !exists && (accountLimited || inboxLimited) && !roomForRemoteMetadata(accountRoom, accountLimited, inboxRoom, inboxLimited, remoteMessageInputBytes(in)) {
		return RemoteMessage{}, ErrRemoteMetadataQuota
	}
	if e = upsertMicrosoftMessageTx(ctx, tx, account, inbox, id, cache); e != nil {
		return RemoteMessage{}, e
	}
	newBytes, _, e := remoteMessageBytesTx(ctx, tx, account, inbox, id)
	if e != nil {
		return RemoteMessage{}, e
	}
	if e = applyRemoteMetadataDeltaTx(ctx, tx, account, inbox, newBytes-oldBytes); e != nil {
		return RemoteMessage{}, e
	}
	if e = tx.Commit(); e != nil {
		return RemoteMessage{}, e
	}
	return s.GetRemoteMessage(ctx, account, inbox, id)
}
func (s *Store) MicrosoftDelta(ctx context.Context, inbox, folder string) (string, bool, string, error) {
	var cursor string
	var initialized bool
	var generation string
	e := s.read.QueryRowContext(ctx, `SELECT cursor,initialized,generation FROM microsoft_delta WHERE inbox_id=? AND folder_id=?`, inbox, folder).Scan(&cursor, &initialized, &generation)
	if e == sql.ErrNoRows {
		e = nil
	}
	return cursor, initialized, generation, e
}

// ErrStaleCursor reports that a persisted provider cursor no longer matches the
// generation a caller read it from: a resync reset the folder while the
// caller's page was in flight.
var ErrStaleCursor = errors.New("provider cursor superseded by a resync")

// SaveMicrosoftDelta advances a folder's delta cursor, but only when the row's
// generation still matches what the caller read: a concurrent resync (a
// generation bump) makes the caller's page part of a superseded sweep, and
// saving its cursor would corrupt the new sweep's baseline. It returns
// ErrStaleCursor in that case; the caller drops the page without advancing.
//
// A page with local effects (metadata, deletions, arrivals) must go through
// ApplyMicrosoftDeltaPage instead: only that path writes the effects and the
// cursor in one guarded transaction, so a stale page can never leave a partial
// sweep behind. This cursor-only form exists for a caller with nothing to write.
func (s *Store) SaveMicrosoftDelta(ctx context.Context, inbox, folder, cursor string, initialized bool, generation string) error {
	account, e := s.inboxAccount(ctx, inbox)
	if e != nil {
		return e
	}
	_, e = s.ApplyMicrosoftDeltaPage(ctx, MicrosoftDeltaPage{AccountID: account, InboxID: inbox, FolderID: folder, Cursor: cursor, Initialized: initialized, Generation: generation})
	return e
}

// inboxAccount resolves an inbox's owning account. It is used by cursor-only
// callers that have no other reason to already know the account.
func (s *Store) inboxAccount(ctx context.Context, inboxID string) (string, error) {
	var account string
	err := s.read.QueryRowContext(ctx, `SELECT account_id FROM inboxes WHERE id=?`, inboxID).Scan(&account)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return account, err
}

func (s *Store) StartMicrosoftResync(ctx context.Context, inbox, folder string) error {
	_, e := s.write.ExecContext(ctx, `INSERT INTO microsoft_delta VALUES(?,?,'',0,?) ON CONFLICT(inbox_id,folder_id) DO UPDATE SET cursor='',initialized=0,generation=excluded.generation`, inbox, folder, nowText())
	return e
}
