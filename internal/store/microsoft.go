package store

import (
	"context"
	"database/sql"
	"time"
)

const migration061 = `
CREATE TABLE inbox_microsoft_connections (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 client_id TEXT NOT NULL, tenant TEXT NOT NULL, identity_id TEXT NOT NULL,
 mailbox TEXT NOT NULL, shared INTEGER NOT NULL, encrypted_token TEXT NOT NULL, expires_at TEXT NOT NULL
);
CREATE TABLE microsoft_message_ids (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL REFERENCES inbox_remote_messages(id) ON DELETE CASCADE,
 provider_id TEXT NOT NULL, folder_id TEXT NOT NULL, generation TEXT NOT NULL DEFAULT '', detected INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(inbox_id,provider_id), UNIQUE(message_id)
);
CREATE TABLE microsoft_delta (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 folder_id TEXT NOT NULL, cursor TEXT NOT NULL DEFAULT '', initialized INTEGER NOT NULL DEFAULT 0, generation TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(inbox_id,folder_id)
);
CREATE TABLE microsoft_oauth_attempts (
 state_hash TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 user_id TEXT NOT NULL, inbox_id TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL,
 client_id TEXT NOT NULL, tenant TEXT NOT NULL, mailbox TEXT NOT NULL, shared INTEGER NOT NULL,
 encrypted_pending TEXT NOT NULL, expires_at TEXT NOT NULL
);`

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
	StateHash, AccountID, UserID, InboxID, DisplayName, ClientID, Tenant, Mailbox, EncryptedPending string
	Shared                                                                                          bool
	ExpiresAt                                                                                       time.Time
}

func (s *Store) SaveMicrosoftAttempt(ctx context.Context, a MicrosoftAttempt) error {
	_, e := s.write.ExecContext(ctx, `DELETE FROM microsoft_oauth_attempts WHERE expires_at<?`, nowText())
	if e != nil {
		return e
	}
	_, e = s.write.ExecContext(ctx, `INSERT INTO microsoft_oauth_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.StateHash, a.AccountID, a.UserID, a.InboxID, a.DisplayName, a.ClientID, a.Tenant, a.Mailbox, a.Shared, a.EncryptedPending, a.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) ConsumeMicrosoftAttempt(ctx context.Context, hash, account, user string) (MicrosoftAttempt, error) {
	var a MicrosoftAttempt
	var expiry string
	e := s.write.QueryRowContext(ctx, `DELETE FROM microsoft_oauth_attempts WHERE state_hash=? AND account_id=? AND user_id=? AND expires_at>? RETURNING state_hash,account_id,user_id,inbox_id,display_name,client_id,tenant,mailbox,shared,encrypted_pending,expires_at`, hash, account, user, nowText()).Scan(&a.StateHash, &a.AccountID, &a.UserID, &a.InboxID, &a.DisplayName, &a.ClientID, &a.Tenant, &a.Mailbox, &a.Shared, &a.EncryptedPending, &expiry)
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
	id := "ms_" + provider + "_" + inbox
	_, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid,created_at,updated_at) SELECT ?,account_id,inbox_id,?,0,?,? FROM inbox_microsoft_connections WHERE account_id=? AND inbox_id=?`, id, "microsoft:"+provider, nowText(), nowText(), account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	now := nowText()
	_, e = tx.ExecContext(ctx, `UPDATE inbox_remote_messages SET rfc_message_id=?,in_reply_to=?,references_json=?,thread_key=?,from_name=?,from_address=?,to_json=?,cc_json=?,subject=?,snippet=?,size_bytes=?,has_attachments=?,is_read=?,is_flagged=?,is_draft=?,received_at=?,internal_date=?,indexed_at=?,updated_at=? WHERE id=? AND account_id=? AND inbox_id=?`, in.RFCMessageID, in.InReplyTo, jsonString(in.References), "microsoft:"+inbox+":"+thread, in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, in.HasAttach, in.Read, in.Flagged, in.Draft, nullStringPtr(in.ReceivedAt), nullStringPtr(in.InternalDate), now, now, id, account, inbox)
	if e != nil {
		return RemoteMessage{}, e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO microsoft_message_ids(inbox_id,message_id,provider_id,folder_id,generation) VALUES(?,?,?,?,COALESCE((SELECT generation FROM microsoft_delta WHERE inbox_id=? AND folder_id=?),'')) ON CONFLICT(inbox_id,provider_id) DO UPDATE SET folder_id=excluded.folder_id,generation=excluded.generation`, inbox, id, provider, folder, inbox, folder)
	if e != nil {
		return RemoteMessage{}, e
	}
	if e = tx.Commit(); e != nil {
		return RemoteMessage{}, e
	}
	return s.GetRemoteMessage(ctx, account, inbox, id)
}
func (s *Store) MicrosoftDelta(ctx context.Context, inbox, folder string) (string, bool, error) {
	var cursor string
	var initialized bool
	e := s.read.QueryRowContext(ctx, `SELECT cursor,initialized FROM microsoft_delta WHERE inbox_id=? AND folder_id=?`, inbox, folder).Scan(&cursor, &initialized)
	if e == sql.ErrNoRows {
		e = nil
	}
	return cursor, initialized, e
}
func (s *Store) SaveMicrosoftDelta(ctx context.Context, inbox, folder, cursor string, initialized bool) error {
	_, e := s.write.ExecContext(ctx, `INSERT INTO microsoft_delta(inbox_id,folder_id,cursor,initialized) VALUES(?,?,?,?) ON CONFLICT(inbox_id,folder_id) DO UPDATE SET cursor=excluded.cursor,initialized=excluded.initialized`, inbox, folder, cursor, initialized)
	return e
}

func (s *Store) StartMicrosoftResync(ctx context.Context, inbox, folder string) error {
	_, e := s.write.ExecContext(ctx, `INSERT INTO microsoft_delta VALUES(?,?,'',0,?) ON CONFLICT(inbox_id,folder_id) DO UPDATE SET cursor='',initialized=0,generation=excluded.generation`, inbox, folder, nowText())
	return e
}

func (s *Store) MicrosoftDetected(ctx context.Context, inbox, provider string) (bool, error) {
	var detected bool
	e := s.read.QueryRowContext(ctx, `SELECT detected FROM microsoft_message_ids WHERE inbox_id=? AND provider_id=?`, inbox, provider).Scan(&detected)
	if e == sql.ErrNoRows {
		e = nil
	}
	return detected, e
}
func (s *Store) MarkMicrosoftDetected(ctx context.Context, inbox, provider string) error {
	_, e := s.write.ExecContext(ctx, `UPDATE microsoft_message_ids SET detected=1 WHERE inbox_id=? AND provider_id=?`, inbox, provider)
	return e
}
func (s *Store) PruneMicrosoftResync(ctx context.Context, account, inbox, folder string) error {
	_, e := s.write.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id IN (SELECT m.message_id FROM microsoft_message_ids m JOIN microsoft_delta d ON d.inbox_id=m.inbox_id AND d.folder_id=m.folder_id WHERE m.inbox_id=? AND m.folder_id=? AND d.generation<>'' AND m.generation<>d.generation)`, account, inbox, inbox, folder)
	return e
}
