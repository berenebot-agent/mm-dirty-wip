package store

import (
	"context"
	"database/sql"
	"time"
)

type MicrosoftPush struct {
	AccountID, InboxID, SubscriptionID, EncryptedState, StateHash, CallbackURL string
	Status, Lifecycle, ClientID, Tenant, EncryptedToken, RequestedSource       string
	ExpiresAt, RetryAt                                                         time.Time
	Requested, Completed                                                       int64
}

func (s *Store) MicrosoftPushActive(ctx context.Context, account, inbox string) bool {
	p, err := s.MicrosoftPush(ctx, account, inbox)
	return err == nil && p.Status == "push active" && p.ExpiresAt.After(time.Now())
}

func scanMicrosoftPush(row interface{ Scan(...any) error }) (MicrosoftPush, error) {
	var p MicrosoftPush
	var expiry, retry string
	err := row.Scan(&p.AccountID, &p.InboxID, &p.SubscriptionID, &p.EncryptedState, &p.StateHash, &p.CallbackURL, &expiry, &retry, &p.Status, &p.Requested, &p.Completed, &p.Lifecycle, &p.ClientID, &p.Tenant, &p.EncryptedToken, &p.RequestedSource)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	p.ExpiresAt, p.RetryAt = parseTime(expiry), parseTime(retry)
	return p, err
}

const microsoftPushColumns = `account_id,inbox_id,subscription_id,encrypted_state,state_hash,callback_url,expires_at,retry_at,status,requested,completed,lifecycle,client_id,tenant,encrypted_token,requested_source`

func (s *Store) MicrosoftPush(ctx context.Context, account, inbox string) (MicrosoftPush, error) {
	return scanMicrosoftPush(s.read.QueryRowContext(ctx, `SELECT `+microsoftPushColumns+` FROM microsoft_push WHERE account_id=? AND inbox_id=?`, account, inbox))
}

func (s *Store) ListMicrosoftPush(ctx context.Context) ([]MicrosoftPush, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+microsoftPushColumns+` FROM microsoft_push ORDER BY inbox_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MicrosoftPush
	for rows.Next() {
		p, err := scanMicrosoftPush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveMicrosoftPush(ctx context.Context, p MicrosoftPush) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO microsoft_push (`+microsoftPushColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(inbox_id) DO UPDATE SET subscription_id=excluded.subscription_id,encrypted_state=excluded.encrypted_state,state_hash=excluded.state_hash,callback_url=excluded.callback_url,expires_at=excluded.expires_at,retry_at=excluded.retry_at,status=excluded.status,client_id=excluded.client_id,tenant=excluded.tenant,encrypted_token=excluded.encrypted_token WHERE microsoft_push.account_id=excluded.account_id`, p.AccountID, p.InboxID, p.SubscriptionID, p.EncryptedState, p.StateHash, p.CallbackURL, p.ExpiresAt.UTC().Format(time.RFC3339Nano), p.RetryAt.UTC().Format(time.RFC3339Nano), p.Status, p.Requested, p.Completed, p.Lifecycle, p.ClientID, p.Tenant, p.EncryptedToken, p.RequestedSource)
	return err
}

// Match both the upstream ID and hashed secret; callbacks cannot select an inbox.
func (s *Store) QueueMicrosoftPush(ctx context.Context, subscription, hash, lifecycle, source string) (accountID, inboxID string, matched bool, err error) {
	err = s.write.QueryRowContext(ctx, `UPDATE microsoft_push SET requested=requested+1,lifecycle=CASE WHEN ?='' THEN lifecycle ELSE ? END,requested_source=CASE WHEN ?='' THEN requested_source ELSE ? END,retry_at='' WHERE subscription_id=? AND state_hash=? AND EXISTS(SELECT 1 FROM inbox_microsoft_connections c JOIN inboxes i ON i.id=c.inbox_id WHERE c.inbox_id=microsoft_push.inbox_id AND c.account_id=microsoft_push.account_id AND c.shared=0 AND i.remote_configured=1 AND i.enabled=1) RETURNING account_id,inbox_id`, lifecycle, lifecycle, source, source, subscription, hash).Scan(&accountID, &inboxID)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return accountID, inboxID, err == nil, err
}

// A generation prevents a notification arriving during a pass from being lost.
func (s *Store) CompleteMicrosoftPush(ctx context.Context, account, inbox string, generation int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE microsoft_push SET completed=?,lifecycle=CASE WHEN requested=? THEN '' ELSE lifecycle END,requested_source=CASE WHEN requested=? THEN '' ELSE requested_source END WHERE account_id=? AND inbox_id=?`, generation, generation, generation, account, inbox)
	return err
}

func (s *Store) DeleteMicrosoftPush(ctx context.Context, account, inbox string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM microsoft_push WHERE account_id=? AND inbox_id=?`, account, inbox)
	return err
}

func (s *Store) ClearMicrosoftPushLifecycle(ctx context.Context, account, inbox string, generation int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE microsoft_push SET lifecycle='' WHERE account_id=? AND inbox_id=? AND requested=?`, account, inbox, generation)
	return err
}
