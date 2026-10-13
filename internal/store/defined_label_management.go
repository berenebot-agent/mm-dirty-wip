package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// ManageDefinedLabel renames or deletes an additive native label in this inbox.
// Existing message-only labels work even if no explicit catalogue row exists.
// A single transaction updates the catalogue, memberships and per-message events.
func (s *Store) ManageDefinedLabel(ctx context.Context, p model.Principal, inboxID, oldName, newName, operation string) ([]model.Event, error) {
	if !p.CanAssist(inboxID) && !p.Admin {
		return nil, ErrForbidden
	}
	oldName = strings.TrimSpace(oldName)
	if clean, ok := model.NormalizeLabel(oldName); !ok || clean != oldName {
		return nil, fmt.Errorf("%w: invalid label", ErrConflict)
	}
	if operation != "rename" && operation != "delete" {
		return nil, fmt.Errorf("%w: invalid operation", ErrConflict)
	}
	if operation == "rename" {
		newName = strings.TrimSpace(newName)
		if clean, ok := model.NormalizeLabel(newName); !ok || clean != newName {
			return nil, fmt.Errorf("%w: invalid label name", ErrConflict)
		}
	}
	if _, err := s.GetInbox(ctx, p, inboxID); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var defined int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_defined_labels
		WHERE account_id=? AND inbox_id=? AND name=? COLLATE NOCASE`,
		p.AccountID, inboxID, oldName).Scan(&defined); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.id,m.thread_id FROM message_labels ml
		JOIN messages m ON m.id=ml.message_id WHERE m.account_id=? AND m.inbox_id=?
		AND ml.label=? COLLATE NOCASE`, p.AccountID, inboxID, oldName)
	if err != nil {
		return nil, err
	}
	type item struct{ id, threadID string }
	messages := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.threadID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		messages = append(messages, v)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if defined == 0 && len(messages) == 0 {
		return nil, ErrNotFound
	}
	if operation == "rename" {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_defined_labels(account_id,inbox_id,name) VALUES(?,?,?)`,
			p.AccountID, inboxID, newName); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_defined_labels
		WHERE account_id=? AND inbox_id=? AND name=? COLLATE NOCASE`, p.AccountID, inboxID, oldName); err != nil {
		return nil, err
	}
	events := []model.Event{}
	for _, m := range messages {
		if operation == "rename" && !strings.EqualFold(oldName, newName) {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id,label,created_at) VALUES(?,?,?)`,
				m.id, newName, nowText()); err != nil {
				return nil, err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=? AND label=? COLLATE NOCASE`, m.id, oldName); err != nil {
			return nil, err
		}
		if operation == "rename" && strings.EqualFold(oldName, newName) {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id,label,created_at) VALUES(?,?,?)`,
				m.id, newName, nowText()); err != nil {
				return nil, err
			}
		}
		rows, e := tx.QueryContext(ctx, `SELECT label FROM message_labels WHERE message_id=? ORDER BY label COLLATE NOCASE`, m.id)
		if e != nil {
			return nil, e
		}
		names := []string{}
		for rows.Next() {
			var value string
			if e = rows.Scan(&value); e != nil {
				_ = rows.Close()
				return nil, e
			}
			names = append(names, value)
		}
		e = rows.Err()
		_ = rows.Close()
		if e != nil {
			return nil, e
		}
		ev, e := insertEventTx(ctx, tx, p.AccountID, inboxID, model.EventMessageLabelsChanged, m.id,
			map[string]any{"message_id": m.id, "inbox_id": inboxID, "thread_id": m.threadID, "labels": names})
		if e != nil {
			return nil, e
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}
