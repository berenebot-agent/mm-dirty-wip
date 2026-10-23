package store

import (
	"context"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// ListDefinedLabels returns additive label names created independently of any
// message. Existing message-only labels are listed separately by ListInboxLabels.
func (s *Store) ListDefinedLabels(ctx context.Context, accountID, inboxID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT name FROM inbox_defined_labels
		WHERE account_id=? AND inbox_id=? ORDER BY name COLLATE NOCASE`, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CreateDefinedLabel persists an empty additive label. The insert is
// inbox-scoped (and cannot reference an inbox of a different account).
func (s *Store) CreateDefinedLabel(ctx context.Context, p model.Principal, inboxID, name string) (string, error) {
	if !p.CanAssist(inboxID) && !p.Admin {
		return "", ErrForbidden
	}
	name = strings.TrimSpace(name)
	clean, ok := model.NormalizeLabel(name)
	if s.IsMicrosoft(ctx, p.AccountID, inboxID) {
		clean, ok = model.NormalizeProviderLabel(name)
	}
	if !ok || clean != name {
		return "", ErrInvalidSearchQuery
	}
	result, err := s.write.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_defined_labels(account_id,inbox_id,name)
		SELECT account_id,id,? FROM inboxes WHERE id=? AND account_id=? `,
		name, inboxID, p.AccountID)
	if err != nil {
		return "", err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		// Existing labels are idempotent; a missing inbox is not.
		if err != nil {
			return "", err
		}
		var exists int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=? `,
			inboxID, p.AccountID).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return "", ErrNotFound
		}
	}
	return name, nil
}
