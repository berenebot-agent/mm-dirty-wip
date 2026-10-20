package store

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/dellarb/mailmoose/internal/model"
)

type inboxLogContextKey struct{}

type inboxLogContext struct {
	source    string
	operation string
	reason    string
	principal model.Principal
}

// SetInboxLogger configures lifecycle logging once, before the store is used
// concurrently. A nil logger falls back to the application's default logger.
func (s *Store) SetInboxLogger(log *slog.Logger) {
	s.inboxLogger = log
}

// WithInboxLogActor attaches trusted attribution at an application entry point.
// Only stable principal IDs are logged, never credentials or request content.
// An empty source preserves any existing source (for calls through app services).
func WithInboxLogActor(ctx context.Context, source string, p model.Principal) context.Context {
	m, _ := ctx.Value(inboxLogContextKey{}).(inboxLogContext)
	if source != "" {
		m.source = source
	}
	m.principal = p
	return context.WithValue(ctx, inboxLogContextKey{}, m)
}

// WithInboxLogOperation identifies a lifecycle flow without losing its actor.
func WithInboxLogOperation(ctx context.Context, operation string) context.Context {
	m, _ := ctx.Value(inboxLogContextKey{}).(inboxLogContext)
	m.operation = operation
	return context.WithValue(ctx, inboxLogContextKey{}, m)
}

// WithInboxLogReason distinguishes setup cleanup from requested deletion.
func WithInboxLogReason(ctx context.Context, reason string) context.Context {
	m, _ := ctx.Value(inboxLogContextKey{}).(inboxLogContext)
	m.reason = reason
	return context.WithValue(ctx, inboxLogContextKey{}, m)
}

func (s *Store) logInbox(ctx context.Context, event string, box model.Inbox, operation, reason string) {
	m, _ := ctx.Value(inboxLogContextKey{}).(inboxLogContext)
	if m.source == "" {
		m.source = "internal"
	}
	if m.operation != "" {
		operation = m.operation
	}
	if m.reason != "" {
		reason = m.reason
	}
	actor := "unknown"
	if m.principal.APIKeyID != "" {
		actor = "api_key"
	} else if m.principal.UserID != "" {
		actor = "user"
	} else if m.source == "internal" {
		actor = "system"
	}
	attrs := []any{"account_id", box.AccountID, "inbox_id", box.ID, "address", box.Address,
		"inbox_kind", box.Kind, "source", m.source, "operation", operation, "actor_type", actor}
	if box.DomainID != "" {
		attrs = append(attrs, "domain_id", box.DomainID)
	}
	if m.principal.UserID != "" {
		attrs = append(attrs, "user_id", m.principal.UserID)
	}
	if m.principal.APIKeyID != "" {
		attrs = append(attrs, "api_key_id", m.principal.APIKeyID)
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	log := s.inboxLogger
	if log == nil {
		log = slog.Default()
	}
	log.InfoContext(ctx, event, attrs...)
}

// Read deletion labels inside the deleting transaction, before rows disappear.
func inboxDeletionLabels(ctx context.Context, tx *sql.Tx, accountID, column, id string) ([]model.Inbox, error) {
	// column is supplied only by the two internal purge methods, not by callers.
	rows, err := tx.QueryContext(ctx, `SELECT i.id,i.account_id,i.kind,COALESCE(i.domain_id,''),CASE WHEN i.kind='standalone' THEN i.address ELSE i.local_part||'@'||d.name END FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND i.`+column+`=? ORDER BY i.id`, accountID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var boxes []model.Inbox
	for rows.Next() {
		var box model.Inbox
		if err := rows.Scan(&box.ID, &box.AccountID, &box.Kind, &box.DomainID, &box.Address); err != nil {
			return nil, err
		}
		boxes = append(boxes, box)
	}
	return boxes, rows.Err()
}
