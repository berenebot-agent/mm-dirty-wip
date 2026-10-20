package store

import (
	"context"
	"database/sql"
	"time"
)

// OneFailureThreshold is how many consecutive authorization failures a One
// binding may record before it is marked unavailable.
const OneFailureThreshold = 3

type OneBinding struct {
	DisplayName                                                                             string
	InboxID, AccountID, ConnectionID, EncryptedConnectionKey, GmailEmail, Status, LastError string
	EncryptedKey, KeyAAD, Fingerprint, RateGroup                                            string
	Revision                                                                                int64
	FailureCount                                                                            int
	LastCheckedAt                                                                           time.Time
}

func (s *Store) OneBinding(ctx context.Context, account, inbox string) (OneBinding, error) {
	var b OneBinding
	var checked string
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,account_id,rate_group,connection_id,encrypted_connection_key,gmail_email,status,last_error,last_checked_at,encrypted_key,key_aad,fingerprint,revision,failure_count FROM inbox_one_bindings WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&b.InboxID, &b.AccountID, &b.RateGroup, &b.ConnectionID, &b.EncryptedConnectionKey, &b.GmailEmail, &b.Status, &b.LastError, &checked, &b.EncryptedKey, &b.KeyAAD, &b.Fingerprint, &b.Revision, &b.FailureCount)
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

// RecordOneFailure counts a consecutive authorization failure and marks the
// binding unavailable once the count reaches OneFailureThreshold. It writes only
// while the binding is still active, so a late or concurrent rejection cannot
// replace an already-recorded verdict or its operator detail; only
// ClearOneFailures may change a flagged binding.
func (s *Store) RecordOneFailure(ctx context.Context, account, inbox, message string, revision int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_one_bindings SET failure_count=failure_count+1,status=CASE WHEN failure_count+1>=? THEN 'attention' ELSE status END,last_error=CASE WHEN failure_count+1>=? THEN ? ELSE last_error END,last_checked_at=? WHERE account_id=? AND inbox_id=? AND revision=? AND status='active'`, OneFailureThreshold, OneFailureThreshold, message, nowText(), account, inbox, revision)
	return err
}

// ClearOneFailures records a revalidation that One accepted: the consecutive
// failure count is wiped and the binding returns to service. This is the only
// path that may lift an "attention" verdict, so a connection that recovers
// resumes on its own instead of waiting for a manual re-save.
func (s *Store) ClearOneFailures(ctx context.Context, account, inbox string, revision int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_one_bindings SET status='active',failure_count=0,last_error='',last_checked_at=? WHERE account_id=? AND inbox_id=? AND revision=? AND status<>'disconnected'`, nowText(), account, inbox, revision)
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

// SetGmailBackfillAfter records the inbox's metadata backfill horizon. An empty
// value means unbounded (index everything). Widening an existing horizon is the
// caller's decision; this writes whatever it is given.
func (s *Store) SetGmailBackfillAfter(ctx context.Context, account, inbox, after string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_gmail_sync SET backfill_after=? WHERE account_id=? AND inbox_id=?`, after, account, inbox)
	return err
}

// GmailBackfillAfter returns the stored horizon, or "" when the inbox indexes
// its full history.
func (s *Store) GmailBackfillAfter(ctx context.Context, account, inbox string) (string, error) {
	var after string
	err := s.read.QueryRowContext(ctx, `SELECT backfill_after FROM inbox_gmail_sync WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&after)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return after, err
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
