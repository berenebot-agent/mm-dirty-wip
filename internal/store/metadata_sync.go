package store

import (
	"context"
	"database/sql"
	"time"
)

// MetadataSyncSnapshot stores only safe operator status, never provider errors
// or credentials. Indexed is the current durable row count, not attempted fetches.
type MetadataSyncSnapshot struct {
	State, Phase, Error string
	Indexed, Total      int64
	UpdatedAt           time.Time
}

func (s *Store) MetadataSyncSnapshot(ctx context.Context, account, inbox string) (MetadataSyncSnapshot, error) {
	var out MetadataSyncSnapshot
	var updated string
	err := s.read.QueryRowContext(ctx, `SELECT state,phase,total,error,updated_at FROM inbox_metadata_sync WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&out.State, &out.Phase, &out.Total, &out.Error, &updated)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	out.UpdatedAt = parseTime(updated)
	err = s.read.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(indexed_at),'') FROM inbox_remote_messages WHERE account_id=? AND inbox_id=?`, account, inbox).Scan(&out.Indexed, &updated)
	if t := parseTime(updated); t.After(out.UpdatedAt) {
		out.UpdatedAt = t
	}
	return out, err
}

func (s *Store) SaveMetadataSyncState(ctx context.Context, account, inbox, state, phase, message string) error {
	switch state {
	case "waiting", "paused", "error", "done":
	default:
		return ErrConflict
	}
	if phase != "initial" && phase != "refresh" {
		return ErrConflict
	}
	_, err := s.write.ExecContext(ctx, `INSERT INTO inbox_metadata_sync(inbox_id,account_id,state,phase,error,updated_at) SELECT id,account_id,?,?,?,? FROM inboxes WHERE account_id=? AND id=? AND kind='standalone' ON CONFLICT(inbox_id) DO UPDATE SET state=excluded.state,phase=excluded.phase,error=excluded.error,updated_at=excluded.updated_at`, state, phase, message, nowText(), account, inbox)
	return err
}

func (s *Store) SaveMetadataSyncTotal(ctx context.Context, account, inbox string, total int64) error {
	if total < 0 {
		total = 0
	}
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_metadata_sync SET total=? WHERE account_id=? AND inbox_id=?`, total, account, inbox)
	return err
}
