package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-agent-inbox/open-agent-inbox/internal/model"
	_ "github.com/open-agent-inbox/open-agent-inbox/internal/sqlite3driver"
)

type Store struct {
	write *sql.DB
	read  *sql.DB
	path  string
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "inbox.db")
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL", path)
	w, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	r, err := sql.Open("sqlite3", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(6)
	r.SetMaxIdleConns(2)
	r.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{write: w, read: r, path: path}
	if err := s.migrate(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { _ = s.read.Close(); return s.write.Close() }
func (s *Store) Path() string { return s.path }
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.write.ExecContext(ctx, migration001); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	_, err := s.write.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES('001',?)`, nowText())
	return err
}

func nowText() string              { return time.Now().UTC().Format(time.RFC3339Nano) }
func timeText(t time.Time) string  { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
func nullableTime(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t := parseTime(v.String)
	return &t
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func decodeStrings(v string) []string {
	var out []string
	_ = json.Unmarshal([]byte(v), &out)
	if out == nil {
		out = []string{}
	}
	return out
}
func decodeMap(v string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(v), &out)
	return out
}

var ErrNotFound = errors.New("not found")
var ErrForbidden = errors.New("forbidden")
var ErrConflict = errors.New("conflict")
var ErrQuota = errors.New("storage quota exceeded")

func normalizeAddress(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
func normalizeDomain(v string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
}
func normalizeLocal(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func (s *Store) Audit(ctx context.Context, accountID, kind, detail string) {
	_, _ = s.write.ExecContext(ctx, `INSERT INTO audit_log(account_id,kind,detail,created_at) VALUES(?,?,?,?)`, nullString(accountID), kind, detail, nowText())
}
func nullString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func principalInboxIDs(p model.Principal) []string {
	ids := make([]string, 0, len(p.MailboxRoles))
	for id := range p.MailboxRoles {
		ids = append(ids, id)
	}
	return ids
}
