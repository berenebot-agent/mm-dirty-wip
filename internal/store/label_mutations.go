package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dellarb/mammothmail/internal/model"
)

// ModifyMessageLabel applies a single additive mutation in one transaction.
// Unlike replacing a snapshot of the full label array, queued add/add and
// add/remove operations cannot silently erase other pending changes.
func (s *Store) ModifyMessageLabel(ctx context.Context, p model.Principal, id, raw, operation string, providerLabel bool) (*model.Event, error) {
	normalize := model.NormalizeLabel
	if providerLabel {
		normalize = model.NormalizeProviderLabel
	}
	label, ok := normalize(raw)
	if !ok || (operation != "add" && operation != "remove") {
		return nil, fmt.Errorf("%w: invalid label action", ErrConflict)
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	m, err := s.getMessageTx(ctx, tx, p.AccountID, id)
	if err != nil {
		return nil, err
	}
	if !p.CanAssist(m.InboxID) {
		return nil, ErrForbidden
	}
	if m.Internal {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT label FROM message_labels WHERE message_id=?`, id)
	if err != nil {
		return nil, err
	}
	var names []string
	exists := false
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if strings.EqualFold(name, label) {
			exists = true
		}
		names = append(names, name)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if (exists && operation == "add") || (!exists && operation == "remove") {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if operation == "add" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO message_labels(message_id,label,created_at) VALUES(?,?,?)`, id, label, nowText()); err != nil {
			return nil, err
		}
		names = append(names, label)
	} else {
		if _, err = tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=? AND label=? COLLATE NOCASE`, id, label); err != nil {
			return nil, err
		}
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if !strings.EqualFold(name, label) {
				kept = append(kept, name)
			}
		}
		names = kept
	}
	sort.Strings(names)
	ev, err := insertEventTx(ctx, tx, p.AccountID, m.InboxID, model.EventMessageLabelsChanged, id,
		map[string]any{"message_id": id, "inbox_id": m.InboxID, "thread_id": m.ThreadID, "labels": names})
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}
