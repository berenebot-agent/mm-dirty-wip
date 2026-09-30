package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/limits"
)

// ClientDeliveryEntry is one event's delivery history for a Webhook or Hermes
// relay client, used by the Dashboard "Log" view. Detail is a short snapshot of
// the event's message (subject or a fallback) that survives the message being
// deleted, so the log stays meaningful without joining the live message row; a
// MessageID is included only while that message still exists.
type ClientDeliveryEntry struct {
	ClientID      string    `json:"client_id"`
	ClientName    string    `json:"client_name"`
	ClientKind    string    `json:"client_kind"`
	EventID       int64     `json:"event_id"`
	EventType     string    `json:"event_type"`
	MessageID     string    `json:"message_id,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	NextAttemptAt string    `json:"next_attempt_at,omitempty"`
	At            time.Time `json:"updated_at"`
}

// recordDeliveryLog appends or updates one event's delivery outcome. updated_at
// always advances so the row sorts to the top of the log on each attempt;
// created_at is preserved from the first write. It is called inside the same
// transaction as the durable cursor update where one exists, so the log and the
// cursor never disagree.
func recordDeliveryLog(ctx context.Context, tx *sql.Tx, clientID string, eventID int64, status string, attempts int, errText, nextAttemptAt string, now string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO client_delivery_log(client_id,event_id,status,attempts,last_error,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(client_id,event_id) DO UPDATE SET
			status=excluded.status,
			attempts=excluded.attempts,
			last_error=excluded.last_error,
			next_attempt_at=excluded.next_attempt_at,
			updated_at=excluded.updated_at`,
		clientID, eventID, status, attempts, errText, nullString(nextAttemptAt), now, now)
	return err
}

// clientLogColumns joins each log row to its client and event so the log can
// show the client, the event type and a message snapshot without a second
// round-trip. The client join uses the log's account scope (the caller already
// proved ownership) and the event join is LEFT so a row survives its event
// being pruned.
const clientLogSelect = `SELECT l.client_id,c.name,c.type,l.event_id,COALESCE(e.type,'') AS event_type,
	COALESCE(m.id,'') AS message_id,COALESCE(NULLIF(m.subject,''),NULLIF(e.entity_id,''),'') AS detail,
	l.status,l.attempts,l.last_error,COALESCE(l.next_attempt_at,''),l.updated_at
	FROM client_delivery_log l
	JOIN clients c ON c.id=l.client_id
	LEFT JOIN events e ON e.id=l.event_id
	LEFT JOIN messages m ON m.account_id=c.account_id AND m.id=e.entity_id AND e.entity_id<>''`

func scanClientLog(row interface{ Scan(...any) error }) (ClientDeliveryEntry, error) {
	var e ClientDeliveryEntry
	var updated string
	err := row.Scan(&e.ClientID, &e.ClientName, &e.ClientKind, &e.EventID, &e.EventType, &e.MessageID, &e.Detail, &e.Status, &e.Attempts, &e.LastError, &e.NextAttemptAt, &updated)
	if err != nil {
		return e, err
	}
	e.At = parseTime(updated)
	return e, nil
}

// ClientDeliveryLog returns one client's delivery history, newest first. The
// client must belong to the account or ErrNotFound is returned. beforeID is a
// keyset cursor on the newest entry of the previous page (0 for the first
// page); because a single event row is updated in place, ordering is by the
// event id as the stable, monotonic proxy for recency.
func (s *Store) ClientDeliveryLog(ctx context.Context, accountID, clientID string, limit int, beforeID int64) ([]ClientDeliveryEntry, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM clients WHERE id=? AND account_id=? AND type IN ('webhook','hermes') AND revoked_at IS NULL`, clientID, accountID).Scan(&n); err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q := clientLogSelect + ` WHERE l.client_id=? AND l.event_id<? ORDER BY l.event_id DESC LIMIT ?`
	rows, err := s.read.QueryContext(ctx, q, clientID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClientDeliveryEntry{}
	for rows.Next() {
		e, err := scanClientLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneClientDeliveryLog deletes terminal delivery history older than the
// retention window. A pending row is never pruned: it represents an outstanding
// delivery the worker will still retry, so it must survive a restart even if it
// is old. Every account is pruned in one pass. It is safe to call periodically.
func (s *Store) PruneClientDeliveryLog(ctx context.Context, retention time.Duration, now time.Time) (int64, error) {
	cutoff := timeText(now.Add(-retention))
	res, err := s.write.ExecContext(ctx, `DELETE FROM client_delivery_log WHERE status<>'pending' AND updated_at<?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
