package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
)

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

type OneAccount struct {
	ID, AccountID, OwnerUserID, EncryptedKey, Fingerprint, Status string
	Revision                                                      int64
	LastValidatedAt                                               time.Time
}

type OneBinding struct {
	InboxID, AccountID, OneAccountID, ConnectionID, EncryptedConnectionKey, GmailEmail, Status, LastError string
	LastCheckedAt                                                                                         time.Time
}

func (s *Store) OneAccount(ctx context.Context, account, owner string) (OneAccount, error) {
	return s.oneAccount(ctx, `account_id=? AND owner_user_id=?`, account, owner)
}

func (s *Store) OneAccountByID(ctx context.Context, account, id string) (OneAccount, error) {
	return s.oneAccount(ctx, `account_id=? AND id=?`, account, id)
}

func (s *Store) oneAccount(ctx context.Context, where string, args ...any) (OneAccount, error) {
	var a OneAccount
	var validated string
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,owner_user_id,encrypted_key,fingerprint,status,revision,last_validated_at FROM one_accounts WHERE `+where, args...).Scan(&a.ID, &a.AccountID, &a.OwnerUserID, &a.EncryptedKey, &a.Fingerprint, &a.Status, &a.Revision, &validated)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	a.LastValidatedAt = parseTime(validated)
	return a, err
}

// SaveOneAccount uses optimistic concurrency so validation cannot overwrite a
// concurrent rotation/removal. The store receives ciphertext only.
func (s *Store) SaveOneAccount(ctx context.Context, a OneAccount, expected int64, bindings []OneBinding) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected == 0 {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `INSERT INTO one_accounts(id,account_id,owner_user_id,encrypted_key,fingerprint,status,last_validated_at,created_at,updated_at) SELECT ?,account_id,id,?,?,'active',?,?,? FROM users WHERE id=? AND account_id=? AND is_admin=1`, a.ID, a.EncryptedKey, a.Fingerprint, nowText(), nowText(), nowText(), a.OwnerUserID, a.AccountID)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrForbidden
			}
		}
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE one_accounts SET encrypted_key=?,fingerprint=?,status='active',revision=revision+1,last_validated_at=?,updated_at=? WHERE id=? AND account_id=? AND owner_user_id=? AND revision=?`, a.EncryptedKey, a.Fingerprint, nowText(), nowText(), a.ID, a.AccountID, a.OwnerUserID, expected)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrConflict
			}
		}
	}
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if _, err = tx.ExecContext(ctx, `UPDATE inbox_one_bindings SET encrypted_connection_key=?,last_checked_at=? WHERE inbox_id=? AND account_id=? AND one_account_id=? AND connection_id=? AND gmail_email=?`, b.EncryptedConnectionKey, nowText(), b.InboxID, a.AccountID, a.ID, b.ConnectionID, b.GmailEmail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) OneBinding(ctx context.Context, account, inbox string) (OneBinding, error) {
	var b OneBinding
	var checked string
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,account_id,one_account_id,connection_id,encrypted_connection_key,gmail_email,status,last_error,last_checked_at FROM inbox_one_bindings WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&b.InboxID, &b.AccountID, &b.OneAccountID, &b.ConnectionID, &b.EncryptedConnectionKey, &b.GmailEmail, &b.Status, &b.LastError, &checked)
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

func (s *Store) OneBindings(ctx context.Context, account, id string) ([]OneBinding, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT inbox_id FROM inbox_one_bindings WHERE account_id=? AND one_account_id=? ORDER BY inbox_id`, account, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []OneBinding{}
	for _, inbox := range ids {
		b, err := s.OneBinding(ctx, account, inbox)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ImportOne creates identity, encrypted binding and shared Gmail state in one
// transaction. Duplicate addresses are conflicts, never implicit migrations.
func (s *Store) ImportOne(ctx context.Context, a OneAccount, b OneBinding, history string) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM one_accounts WHERE id=? AND account_id=? AND owner_user_id=? AND status='active'`, a.ID, a.AccountID, a.OwnerUserID).Scan(&revision)
	if err != nil || revision != a.Revision {
		return "", ErrConflict
	}
	address, err := ValidateStandaloneAddress(b.GmailEmail)
	if err != nil {
		return "", err
	}
	var existing, existingEmail string
	err = tx.QueryRowContext(ctx, `SELECT inbox_id,gmail_email FROM inbox_one_bindings WHERE one_account_id=? AND connection_id=?`, a.ID, b.ConnectionID).Scan(&existing, &existingEmail)
	if err == nil {
		if existingEmail != address {
			return "", ErrConflict
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	var collision int
	err = tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM inboxes WHERE account_id=? AND address=? COLLATE NOCASE)
 +(SELECT count(*) FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND (i.local_part||'@'||d.name)=? COLLATE NOCASE)
 +(SELECT count(*) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.account_id=? AND (a.local_part||'@'||d.name)=? COLLATE NOCASE)`, a.AccountID, address, a.AccountID, address, a.AccountID, address).Scan(&collision)
	if err != nil {
		return "", err
	}
	if collision != 0 {
		return "", ErrConflict
	}
	if b.InboxID == "" || b.EncryptedConnectionKey == "" || b.ConnectionID == "" {
		return "", ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,kind,domain_id,local_part,address,display_name,namespace,remote_host,remote_username,remote_security,remote_configured,remote_sent_copy_enabled,remote_poll_seconds,delivery_trigger,storage_used_bytes,created_at) VALUES(?,?,'standalone',NULL,'',?,?,'INBOX','api.withone.ai',?,'tls',1,0,300,?,0,?)`, b.InboxID, a.AccountID, address, address+" · Gmail via One", address, DeliveryTriggerDefault, nowText())
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_credentials(inbox_id,account_id,revision,created_at,updated_at) VALUES(?,?,0,?,?)`, b.InboxID, a.AccountID, nowText(), nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_one_bindings(inbox_id,account_id,one_account_id,connection_id,encrypted_connection_key,gmail_email,last_checked_at) VALUES(?,?,?,?,?,?,?)`, b.InboxID, a.AccountID, a.ID, b.ConnectionID, b.EncryptedConnectionKey, address, nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_gmail_sync(inbox_id,account_id,history_id) VALUES(?,?,?)`, b.InboxID, a.AccountID, history); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO google_detection(inbox_id,history_id) VALUES(?,?)`, b.InboxID, history); err != nil {
		return "", err
	}
	return b.InboxID, tx.Commit()
}

func NewOneID() string { return idgen.New("one") }

func (s *Store) SetOneHealth(ctx context.Context, account, inbox, status, message string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_one_bindings SET status=?,last_error=?,last_checked_at=? WHERE account_id=? AND inbox_id=? AND status<>'disconnected'`, status, message, nowText(), account, inbox)
	return err
}

// Removing credentials pauses bindings but preserves local metadata and drafts.
// A tombstone preserves Gmail routing; it can never fall back to IMAP or send.
func (s *Store) RemoveOneAccount(ctx context.Context, account, owner string, confirmed bool) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM one_accounts WHERE account_id=? AND owner_user_id=?`, account, owner).Scan(&id); err != nil {
		return ErrNotFound
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_one_bindings WHERE one_account_id=?`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 && !confirmed {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE one_accounts SET encrypted_key='',fingerprint='',status='removed',revision=revision+1,updated_at=? WHERE id=?`, nowText(), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_one_bindings SET encrypted_connection_key='',status='disconnected',last_error='One credentials removed; local metadata and drafts retained' WHERE one_account_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET remote_configured=0 WHERE id IN (SELECT inbox_id FROM inbox_one_bindings WHERE one_account_id=?)`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetOneConnected(ctx context.Context, account, owner, inbox string, connected bool) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status, configured := "disconnected", 0
	if connected {
		status, configured = "active", 1
	}
	res, err := tx.ExecContext(ctx, `UPDATE inbox_one_bindings SET status=?,last_error='' WHERE account_id=? AND inbox_id=? AND one_account_id IN (SELECT id FROM one_accounts WHERE account_id=? AND owner_user_id=? AND status='active')`, status, account, inbox, account, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET remote_configured=? WHERE id=? AND account_id=?`, configured, inbox, account); err != nil {
		return err
	}
	return tx.Commit()
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
