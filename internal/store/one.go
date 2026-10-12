package store

import (
	"context"
	"database/sql"
	"time"
)

// Historical migration retained unchanged. Migration 067 removes its shared
// credentials and replaces the binding schema with inbox-owned credentials.
const migration066 = `
CREATE TABLE inbox_gmail_sync (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 history_id TEXT NOT NULL DEFAULT '', backfill_page TEXT NOT NULL DEFAULT '',
 backfill_done INTEGER NOT NULL DEFAULT 0, sync_generation TEXT NOT NULL DEFAULT '',
 backfill_offset INTEGER NOT NULL DEFAULT 0, backfill_snapshot TEXT NOT NULL DEFAULT ''
);
INSERT INTO inbox_gmail_sync(inbox_id,account_id,history_id,backfill_page,backfill_done,sync_generation)
 SELECT inbox_id,account_id,history_id,backfill_page,backfill_done,sync_generation FROM inbox_google_connections;
CREATE TABLE one_accounts (
 id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
 encrypted_key TEXT NOT NULL, fingerprint TEXT NOT NULL,
 revision INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'active',
 last_validated_at TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(account_id,owner_user_id), UNIQUE(id,account_id)
);
CREATE TABLE inbox_one_bindings (
 inbox_id TEXT PRIMARY KEY REFERENCES inboxes(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 one_account_id TEXT NOT NULL, connection_id TEXT NOT NULL, encrypted_connection_key TEXT NOT NULL,
 gmail_email TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', last_error TEXT NOT NULL DEFAULT '',
 last_checked_at TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(one_account_id,account_id) REFERENCES one_accounts(id,account_id) ON DELETE CASCADE,
 UNIQUE(one_account_id,connection_id)
);
CREATE TABLE gmail_history_pages (
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL CHECK(purpose IN ('sync','detection')),
 anchor TEXT NOT NULL, page_token TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL DEFAULT '',
 item_offset INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(inbox_id,purpose)
);
`

type OneBinding struct {
	DisplayName                                                                             string
	InboxID, AccountID, ConnectionID, EncryptedConnectionKey, GmailEmail, Status, LastError string
	EncryptedKey, KeyAAD, Fingerprint, RateGroup                                            string
	Revision                                                                                int64
	LastCheckedAt                                                                           time.Time
}

func (s *Store) OneBinding(ctx context.Context, account, inbox string) (OneBinding, error) {
	var b OneBinding
	var checked string
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,account_id,rate_group,connection_id,encrypted_connection_key,gmail_email,status,last_error,last_checked_at,encrypted_key,key_aad,fingerprint,revision FROM inbox_one_bindings WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&b.InboxID, &b.AccountID, &b.RateGroup, &b.ConnectionID, &b.EncryptedConnectionKey, &b.GmailEmail, &b.Status, &b.LastError, &checked, &b.EncryptedKey, &b.KeyAAD, &b.Fingerprint, &b.Revision)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	b.LastCheckedAt = parseTime(checked)
	return b, err
}

func (s *Store) IsOne(ctx context.Context, account, inbox string) bool {
	_, err := s.OneBinding(ctx, account, inbox)
	return err == nil
}
func (s *Store) SetOneHealth(ctx context.Context, account, inbox, status, message string, revision int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_one_bindings SET status=?,last_error=?,last_checked_at=? WHERE account_id=? AND inbox_id=? AND revision=? AND status<>'disconnected'`, status, message, nowText(), account, inbox, revision)
	return err
}

func (s *Store) GmailSync(ctx context.Context, account, inbox string) (GoogleConnection, error) {
	var c GoogleConnection
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,account_id,history_id,backfill_page,backfill_done,backfill_offset,backfill_snapshot FROM inbox_gmail_sync WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&c.InboxID, &c.AccountID, &c.HistoryID, &c.BackfillPage, &c.BackfillDone, &c.BackfillOffset, &c.BackfillSnapshot)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return c, err
}
func (s *Store) SaveGmailBackfillOffset(ctx context.Context, account, inbox string, offset int) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_gmail_sync SET backfill_offset=? WHERE account_id=? AND inbox_id=?`, offset, account, inbox)
	return err
}
func (s *Store) SaveGmailBackfillSnapshot(ctx context.Context, account, inbox, snapshot string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_gmail_sync SET backfill_snapshot=?,backfill_offset=0 WHERE account_id=? AND inbox_id=?`, snapshot, account, inbox)
	return err
}

type GmailHistoryPage struct {
	Anchor, PageToken, Payload string
	Offset                     int
}

func (s *Store) GmailHistoryPage(ctx context.Context, account, inbox, purpose string) (GmailHistoryPage, error) {
	var p GmailHistoryPage
	err := s.read.QueryRowContext(ctx, `SELECT h.anchor,h.page_token,h.payload,h.item_offset FROM gmail_history_pages h JOIN inboxes i ON i.id=h.inbox_id WHERE i.account_id=? AND h.inbox_id=? AND h.purpose=?`, account, inbox, purpose).Scan(&p.Anchor, &p.PageToken, &p.Payload, &p.Offset)
	if err == sql.ErrNoRows {
		return p, nil
	}
	return p, err
}
func (s *Store) SaveGmailHistoryPage(ctx context.Context, account, inbox, purpose string, p GmailHistoryPage) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO gmail_history_pages(inbox_id,purpose,anchor,page_token,payload,item_offset) SELECT id,?,?,?,?,? FROM inboxes WHERE account_id=? AND id=? ON CONFLICT(inbox_id,purpose) DO UPDATE SET anchor=excluded.anchor,page_token=excluded.page_token,payload=excluded.payload,item_offset=excluded.item_offset`, purpose, p.Anchor, p.PageToken, p.Payload, p.Offset, account, inbox)
	return err
}
func (s *Store) ClearGmailHistoryPage(ctx context.Context, account, inbox, purpose string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM gmail_history_pages WHERE inbox_id=? AND purpose=? AND inbox_id IN (SELECT id FROM inboxes WHERE account_id=?)`, inbox, purpose, account)
	return err
}
