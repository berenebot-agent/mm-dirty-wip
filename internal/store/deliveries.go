package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// DeliveryTriggerAny and DeliveryTriggerAll are the accepted values for an
// inbox's delivery trigger. "any" fires the auto-actions on the first
// connector delivery; "all" waits until every connector that existed when the
// message arrived has delivered.
const (
	DeliveryTriggerAny = "any"
	DeliveryTriggerAll = "all"
)

// inboxAutoActions is the delivery-driven policy an inbox carries.
type inboxAutoActions struct {
	MarkRead   bool
	TrashHours *int
	Trigger    string
}

func validDeliveryTrigger(t string) bool {
	return t == DeliveryTriggerAny || t == DeliveryTriggerAll
}

// SetInboxAutoActions updates an inbox's delivery-triggered auto-actions. A nil
// pointer leaves the matching field unchanged; a non-nil TrashHours of 0 is
// rejected (it disables the sweep, which nil already expresses). The trigger,
// when non-nil, must be "any" or "all".
func (s *Store) SetInboxAutoActions(ctx context.Context, accountID, inboxID string, markRead *bool, trashHours *int, trigger *string) error {
	var sets []string
	var args []any
	if markRead != nil {
		sets = append(sets, "auto_mark_read_on_delivery=?")
		args = append(args, boolInt(*markRead))
	}
	if trashHours != nil {
		if *trashHours <= 0 {
			return ErrForbidden
		}
		sets = append(sets, "auto_trash_after_delivery_hours=?")
		args = append(args, *trashHours)
	}
	if trigger != nil {
		t := string(*trigger)
		if !validDeliveryTrigger(t) {
			return ErrForbidden
		}
		sets = append(sets, "delivery_trigger=?")
		args = append(args, t)
	}
	if len(sets) == 0 {
		return nil
	}
	q := "UPDATE inboxes SET " + joinComma(sets) + " WHERE id=? AND account_id=?"
	args = append(args, inboxID, accountID)
	res, err := s.write.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearInboxAutoTrash disables the auto-trash window for an inbox, leaving the
// other auto-action fields unchanged.
func (s *Store) ClearInboxAutoTrash(ctx context.Context, accountID, inboxID string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET auto_trash_after_delivery_hours=NULL WHERE id=? AND account_id=?`, inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// recordEventDeliveryTx resolves a durable event to its message and inbox and
// records a delivery by clientID for it, then evaluates the inbox's
// delivery auto-actions. It is called inside the acknowledgement transaction so
// the cursor advance, the delivery row and any action it enables commit
// together. A non-message event (for example a spam-state event whose entity is
// not a message) is ignored.
func recordEventDeliveryTx(ctx context.Context, tx *sql.Tx, clientID string, eventID int64, now string) error {
	var inboxID, entity string
	err := tx.QueryRowContext(ctx, `SELECT inbox_id,entity_id FROM events WHERE id=?`, eventID).Scan(&inboxID, &entity)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if entity == "" {
		return nil
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=? AND inbox_id=?`, entity, inboxID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_deliveries(message_id,client_id,delivered_at) VALUES(?,?,?)`, entity, clientID, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	return applyDeliveryActionsTx(ctx, tx, inboxID, entity, parseTime(now))
}

// RecordDelivery durably records that one connector has delivered a message.
// It is idempotent (a repeated ack for the same pair is a no-op) and returns
// whether the row was newly inserted. It then evaluates the inbox's delivery
// auto-actions against the updated delivery set. It is best-effort at the call
// sites: a failure to record a delivery must never block or fail an
// acknowledgement.
func (s *Store) RecordDelivery(ctx context.Context, inboxID, clientID, messageID string, at time.Time) (bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_deliveries(message_id,client_id,delivered_at) VALUES(?,?,?)`, messageID, clientID, timeText(at))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, tx.Commit()
	}
	if err = applyDeliveryActionsTx(ctx, tx, inboxID, messageID, at); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// applyDeliveryActionsTx stamps messages.delivery_action_due_at and/or marks the
// message read once the inbox's trigger is satisfied. It runs in the same
// transaction that recorded the delivery, so the delivery row and the action
// it enabled commit together. It acts on inbound, non-spam, non-internal,
// non-trashed mail only. A message whose due-at is already set is left alone,
// so a later connector ack cannot extend its window.
func applyDeliveryActionsTx(ctx context.Context, tx *sql.Tx, inboxID, messageID string, deliveredAt time.Time) error {
	var markRead int
	var trashHours sql.NullInt64
	var trigger string
	err := tx.QueryRowContext(ctx, `SELECT auto_mark_read_on_delivery,auto_trash_after_delivery_hours,delivery_trigger FROM inboxes WHERE id=?`, inboxID).Scan(&markRead, &trashHours, &trigger)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if markRead == 0 && !trashHours.Valid {
		return nil
	}
	var isRead, isSpam, internal int
	var deleted, dueAt sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT is_read,is_spam,internal,deleted_at,delivery_action_due_at FROM messages WHERE id=? AND inbox_id=?`, messageID, inboxID).Scan(&isRead, &isSpam, &internal, &deleted, &dueAt); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if internal != 0 || isSpam != 0 || deleted.Valid {
		return nil
	}
	qualified, err := deliveryTriggerSatisfiedTx(ctx, tx, inboxID, messageID, trigger)
	if err != nil {
		return err
	}
	if !qualified {
		return nil
	}
	if markRead == 1 && isRead == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET is_read=1 WHERE id=?`, messageID); err != nil {
			return err
		}
	}
	// The due-at instant is set once, from the delivery that first satisfied the
	// trigger. A message already carrying one keeps its original window.
	if trashHours.Valid && !dueAt.Valid {
		due := deliveredAt.Add(time.Duration(trashHours.Int64) * time.Hour)
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET delivery_action_due_at=? WHERE id=? AND delivery_action_due_at IS NULL`, timeText(due), messageID); err != nil {
			return err
		}
	}
	return nil
}

// deliveryTriggerSatisfiedTx reports whether the inbox's trigger is met for a
// message. "any" is satisfied by the delivery just recorded; "all" requires
// every connector bound to the inbox that existed when the message arrived to
// have a delivery row. A connector added after the message arrived is not
// required, so old mail is never pinned by a newly added connector.
func deliveryTriggerSatisfiedTx(ctx context.Context, tx *sql.Tx, inboxID, messageID, trigger string) (bool, error) {
	if trigger != DeliveryTriggerAll {
		return true, nil
	}
	var received string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(received_at,created_at) FROM messages WHERE id=?`, messageID).Scan(&received); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	var outstanding int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients c JOIN client_push p ON p.client_id=c.id WHERE p.inbox_id=? AND c.type IN (`+relayKindsSQL+`,'webhook') AND c.revoked_at IS NULL AND c.created_at<=? AND NOT EXISTS (SELECT 1 FROM message_deliveries d WHERE d.message_id=? AND d.client_id=c.id)`, inboxID, received, messageID).Scan(&outstanding)
	if err != nil {
		return false, err
	}
	return outstanding == 0, nil
}

// TrashDeliveredDue moves every live message whose delivery auto-trash window
// has elapsed to Trash, returning the durable events to publish. Only inboxes
// with auto_trash_after_delivery_hours set contribute; a message is eligible
// once delivery_action_due_at is non-null and no later than now. The message's
// row, raw MIME and accounting are retained by the Trash model.
func (s *Store) TrashDeliveredDue(ctx context.Context, now time.Time) ([]model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.account_id,m.inbox_id,m.thread_id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.delivery_action_due_at IS NOT NULL AND m.delivery_action_due_at<=? AND m.deleted_at IS NULL AND m.internal=0 AND m.is_spam=0 AND i.auto_trash_after_delivery_hours IS NOT NULL`, timeText(now))
	if err != nil {
		return nil, err
	}
	type due struct {
		id, accountID, inboxID, threadID string
	}
	var pending []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.id, &d.accountID, &d.inboxID, &d.threadID); err != nil {
			rows.Close()
			return nil, err
		}
		pending = append(pending, d)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	events := make([]model.Event, 0, len(pending))
	nowTextValue := nowText()
	for _, d := range pending {
		res, uerr := tx.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id=? AND deleted_at IS NULL`, nowTextValue, d.id)
		if uerr != nil {
			return nil, uerr
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		ev, eerr := insertEventTx(ctx, tx, d.accountID, d.inboxID, model.EventMessageTrashed, d.id, map[string]any{"message_id": d.id, "inbox_id": d.inboxID, "thread_id": d.threadID, "reason": "auto_trash_after_delivery"})
		if eerr != nil {
			return nil, eerr
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// joinComma joins SQL set fragments with a comma. It is local so the store does
// not depend on strings for this one call site.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
