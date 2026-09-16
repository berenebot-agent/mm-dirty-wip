package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"gatehouse-mail/internal/limits"
	"gatehouse-mail/internal/model"
)

func ParseCursor(v string) int64 {
	v = strings.TrimSpace(strings.TrimPrefix(v, "evt_"))
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// ParseCursorStrict parses a durable event cursor of the form "evt_<n>". It
// reports ok=false for anything malformed so a caller can reject a bad cursor
// instead of silently treating it as the start of history.
func ParseCursorStrict(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "evt_") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(v, "evt_"), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func EventCursor(id int64) string { return fmt.Sprintf("evt_%d", id) }

// LatestEventID returns the newest durable event id for the account, or 0 when
// it has no events yet. A long-poll can seed its cursor with this so it waits
// for genuinely new events instead of replaying history.
func (s *Store) LatestEventID(ctx context.Context, accountID string) (int64, error) {
	var id int64
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE account_id=?`, accountID).Scan(&id)
	return id, err
}

func scanEvent(row interface{ Scan(...any) error }) (model.Event, error) {
	var e model.Event
	var inbox sql.NullString
	var payload, created string
	err := row.Scan(&e.ID, &e.AccountID, &inbox, &e.Type, &e.EntityID, &payload, &created)
	if err != nil {
		return e, err
	}
	if inbox.Valid {
		e.InboxID = inbox.String
	}
	e.Cursor = EventCursor(e.ID)
	e.Payload = decodeMap(payload)
	e.CreatedAt = parseTime(created)
	return e, nil
}

func (s *Store) ListEvents(ctx context.Context, p model.Principal, after int64, inboxID string, limit int) ([]model.Event, error) {
	q := `SELECT id,account_id,inbox_id,type,entity_id,payload_json,created_at FROM events WHERE account_id=? AND id>?`
	args := []any{p.AccountID, after}
	if inboxID != "" {
		if !p.CanRead(inboxID) {
			return nil, ErrForbidden
		}
		q += ` AND inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Event{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if limit <= 0 || limit > limits.PageSizeMaxEvents {
		limit = limits.PageSizeDefault
	}
	q += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) GetEvent(ctx context.Context, accountID string, id int64) (model.Event, error) {
	e, err := scanEvent(s.read.QueryRowContext(ctx, `SELECT id,account_id,inbox_id,type,entity_id,payload_json,created_at FROM events WHERE account_id=? AND id=?`, accountID, id))
	if err == sql.ErrNoRows {
		return e, ErrNotFound
	}
	return e, err
}
func (s *Store) NextHermesEvent(ctx context.Context, connID string, after int64) (model.Event, error) {
	e, err := scanEvent(s.read.QueryRowContext(ctx, `SELECT e.id,e.account_id,e.inbox_id,e.type,e.entity_id,e.payload_json,e.created_at FROM events e JOIN hermes_connections h ON h.inbox_id=e.inbox_id WHERE h.id=? AND e.id>? AND e.type IN ('message.received','message.spam_state_changed') ORDER BY e.id ASC LIMIT 1`, connID, after))
	if err == sql.ErrNoRows {
		return e, ErrNotFound
	}
	return e, err
}
