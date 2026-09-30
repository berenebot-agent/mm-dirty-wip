package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
)

type WebhookClient struct {
	ID, AccountID, InboxID, Name, URL, Mode, AuthMode string
	SecretEncrypted                                   string `json:"-"`
	Enabled                                           bool
	LastAckEventID                                    int64
	LastSuccessAt                                     *time.Time
	LastError                                         string
	CreatedAt                                         time.Time
	EventID                                           int64
	EntityID                                          string
}

type PendingWebhookDelivery struct {
	Client  WebhookClient
	EventID int64
	// Type, EntityID and Cursor identify the event. EntityID is the message id
	// for both delivered event types.
	Type, EntityID, Cursor string
	// Payload is the durable event payload. The worker consults is_spam/new to
	// decide whether a currently-Spam message must be skipped.
	Payload []byte
}

func (s *Store) CreateWebhookClient(ctx context.Context, accountID, inboxID, name, targetURL, mode, authMode, encrypted string) (WebhookClient, error) {
	id := idgen.New("whk")
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return WebhookClient{}, err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
		return WebhookClient{}, ErrForbidden
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,?,'webhook',?,?)`, id, accountID, name, now); err != nil {
		return WebhookClient{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO client_push(client_id,inbox_id,url,payload_mode,auth_mode,signing_secret_encrypted) VALUES(?,?,?,?,?,?)`, id, inboxID, targetURL, mode, authMode, encrypted); err != nil {
		return WebhookClient{}, err
	}
	if err = tx.Commit(); err != nil {
		return WebhookClient{}, err
	}
	return WebhookClient{ID: id, AccountID: accountID, InboxID: inboxID, Name: name, URL: targetURL, Mode: mode, AuthMode: authMode, SecretEncrypted: encrypted, Enabled: true, CreatedAt: parseTime(now)}, nil
}

func scanWebhook(row interface{ Scan(...any) error }) (WebhookClient, error) {
	var c WebhookClient
	var enabled int
	var created, success sql.NullString
	err := row.Scan(&c.ID, &c.AccountID, &c.InboxID, &c.Name, &c.URL, &c.Mode, &c.AuthMode, &c.SecretEncrypted, &enabled, &c.LastAckEventID, &success, &c.LastError, &created)
	c.Enabled = enabled != 0
	if created.Valid {
		c.CreatedAt = parseTime(created.String)
	}
	c.LastSuccessAt = nullableTime(success)
	return c, err
}

const webhookColumns = `c.id,c.account_id,p.inbox_id,c.name,p.url,p.payload_mode,p.auth_mode,p.signing_secret_encrypted,p.enabled,p.last_ack_event_id,p.last_success_at,p.last_error,c.created_at`

func (s *Store) ListWebhookClients(ctx context.Context, accountID string) ([]WebhookClient, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+webhookColumns+` FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.account_id=? AND c.type='webhook' AND c.revoked_at IS NULL ORDER BY c.created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WebhookClient{}
	for rows.Next() {
		c, e := scanWebhook(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetWebhookClient(ctx context.Context, accountID, id string) (WebhookClient, error) {
	c, err := scanWebhook(s.read.QueryRowContext(ctx, `SELECT `+webhookColumns+` FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.account_id=? AND c.id=? AND c.type='webhook' AND c.revoked_at IS NULL`, accountID, id))
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) UpdateWebhookClient(ctx context.Context, accountID, id, name, targetURL, mode, authMode string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE clients SET name=? WHERE id=? AND account_id=? AND type='webhook' AND revoked_at IS NULL`, name, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE client_push SET url=?,payload_mode=?,auth_mode=? WHERE client_id=?`, targetURL, mode, authMode, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RotateWebhookSecret(ctx context.Context, accountID, id, encrypted string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE client_push SET signing_secret_encrypted=? WHERE client_id=? AND EXISTS (SELECT 1 FROM clients c WHERE c.id=client_push.client_id AND c.account_id=? AND c.type='webhook' AND c.revoked_at IS NULL)`, encrypted, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetWebhookEnabled(ctx context.Context, accountID, id string, enabled bool) error {
	res, err := s.write.ExecContext(ctx, `UPDATE client_push SET enabled=? WHERE client_id=? AND EXISTS (SELECT 1 FROM clients c WHERE c.id=client_push.client_id AND c.account_id=? AND c.type='webhook' AND c.revoked_at IS NULL)`, boolInt(enabled), id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteWebhookClient(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE clients SET revoked_at=? WHERE id=? AND account_id=? AND type='webhook' AND revoked_at IS NULL`, nowText(), id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) NextWebhookDelivery(ctx context.Context, now time.Time) (PendingWebhookDelivery, error) {
	var d PendingWebhookDelivery
	var enabled int
	var created, success sql.NullString
	var payload string
	err := s.read.QueryRowContext(ctx, `SELECT c.id,c.account_id,p.inbox_id,c.name,p.url,p.payload_mode,p.auth_mode,p.signing_secret_encrypted,p.enabled,p.last_ack_event_id,p.last_success_at,p.last_error,c.created_at,e.id,e.type,e.entity_id,e.payload_json FROM clients c JOIN client_push p ON p.client_id=c.id JOIN events e ON e.id=(SELECT e2.id FROM events e2 WHERE e2.inbox_id=p.inbox_id AND e2.id>p.last_ack_event_id AND e2.type IN ('message.received','message.spam_state_changed') AND NOT EXISTS (SELECT 1 FROM webhook_deliveries wd WHERE wd.client_id=c.id AND wd.event_id=e2.id AND wd.status IN ('delivered','failed','skipped')) ORDER BY e2.id LIMIT 1) WHERE c.type='webhook' AND c.revoked_at IS NULL AND p.enabled=1 AND NOT EXISTS (SELECT 1 FROM webhook_deliveries wd WHERE wd.client_id=c.id AND wd.event_id=e.id AND wd.status='pending' AND wd.next_attempt_at>?) ORDER BY e.id LIMIT 1`, retryTimeText(now)).Scan(&d.Client.ID, &d.Client.AccountID, &d.Client.InboxID, &d.Client.Name, &d.Client.URL, &d.Client.Mode, &d.Client.AuthMode, &d.Client.SecretEncrypted, &enabled, &d.Client.LastAckEventID, &success, &d.Client.LastError, &created, &d.EventID, &d.Type, &d.EntityID, &payload)
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Payload = []byte(payload)
	d.Client.Enabled = enabled != 0
	d.Client.CreatedAt = parseTime(created.String)
	d.Client.LastSuccessAt = nullableTime(success)
	d.Cursor = EventCursor(d.EventID)
	d.Client.EventID = d.EventID
	d.Client.EntityID = d.EntityID
	return d, nil
}

// RecordWebhookSkipped terminally marks an event as skipped (its message is
// currently Spam, internal, or has been deleted) and advances the client
// cursor exactly like a delivered or failed delivery would, so the head of the
// queue moves on without any network call. It is idempotent: a delivery already
// recorded as delivered/failed/skipped is left alone.
func (s *Store) RecordWebhookSkipped(ctx context.Context, clientID string, eventID int64) error {
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(client_id,event_id,attempts,next_attempt_at,last_error,status,created_at) VALUES(?,?,0,?,?,?,?) ON CONFLICT(client_id,event_id) DO UPDATE SET status='skipped',last_error=excluded.last_error,next_attempt_at=excluded.next_attempt_at WHERE webhook_deliveries.status='pending'`, clientID, eventID, now, "skipped: message not deliverable", "skipped", now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE client_push SET last_ack_event_id=MAX(last_ack_event_id,?) WHERE client_id=? AND EXISTS (SELECT 1 FROM webhook_deliveries wd WHERE wd.client_id=? AND wd.event_id=? AND wd.status IN ('delivered','failed','skipped'))`, eventID, clientID, clientID, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// retryTimeText renders a retry instant at whole-second precision so stored
// values compare correctly with a lexicographic TEXT comparison, which
// RFC3339Nano's trimmed fractional seconds do not guarantee.
func retryTimeText(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func (s *Store) WebhookAttemptCount(ctx context.Context, clientID string, eventID int64, out *int) error {
	return s.read.QueryRowContext(ctx, `SELECT attempts FROM webhook_deliveries WHERE client_id=? AND event_id=?`, clientID, eventID).Scan(out)
}

func (s *Store) RecordWebhookDelivery(ctx context.Context, clientID string, eventID int64, success bool, errText string, retryAt time.Time, expireAt time.Time) error {
	status := "pending"
	if success {
		status = "delivered"
	}
	if !success && !retryAt.Before(expireAt) {
		status = "failed"
	}
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_deliveries(client_id,event_id,attempts,next_attempt_at,last_error,status,created_at) VALUES(?,?,1,?,?,?,?) ON CONFLICT(client_id,event_id) DO UPDATE SET attempts=webhook_deliveries.attempts+1,next_attempt_at=excluded.next_attempt_at,last_error=excluded.last_error,status=excluded.status`, clientID, eventID, retryTimeText(retryAt), errText, status, now); err != nil {
		return err
	}
	if status != "pending" {
		if _, err = tx.ExecContext(ctx, `UPDATE client_push SET last_ack_event_id=MAX(last_ack_event_id,?),last_success_at=CASE WHEN ? THEN ? ELSE last_success_at END,last_error=? WHERE client_id=?`, eventID, boolInt(success), now, errText, clientID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
