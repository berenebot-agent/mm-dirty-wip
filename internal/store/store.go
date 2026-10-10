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

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/safepath"
	_ "github.com/dellarb/mailmoose/internal/sqlite3driver"
)

type Store struct {
	write *sql.DB
	read  *sql.DB
	path  string
	// standaloneSender resolves a standalone inbox's own sending configuration
	// (its remote SMTP binding). It is set once at startup; when nil, a standalone
	// send resolves to ErrNoProvider and is queued and held.
	standaloneSender StandaloneSenderResolver
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
	// Retire any raw files a migration queued for post-commit unlink. This runs
	// outside the migration transaction, so a rollback never orphans a live row.
	s.sweepPendingFileCleanup(context.Background())
	return s, nil
}

// sweepPendingFileCleanup unlinks the files recorded in pending_file_cleanup and
// clears the queue. It is best-effort: a file already gone is fine, and an
// interrupted sweep simply retries on the next start. It runs after migrations
// commit, never inside a transaction.
func (s *Store) sweepPendingFileCleanup(ctx context.Context) {
	dir := filepath.Dir(s.path)
	rows, err := s.read.QueryContext(ctx, `SELECT id,rel_path FROM pending_file_cleanup ORDER BY id`)
	if err != nil {
		return
	}
	type item struct {
		id   int64
		path string
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.path) != nil {
			rows.Close()
			return
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if p, jerr := safepath.Join(dir, it.path); jerr == nil {
			_ = os.Remove(p)
		}
		if _, derr := s.write.ExecContext(ctx, `DELETE FROM pending_file_cleanup WHERE id=?`, it.id); derr != nil {
			return
		}
	}
}
func (s *Store) Close() error { _ = s.read.Close(); return s.write.Close() }
func (s *Store) Path() string { return s.path }

// Checkpoint folds the write-ahead log back into the main database file. It is
// used when a database file must be copied (test fixtures, backup helpers)
// without the -wal and -shm side files.
func (s *Store) Checkpoint() error {
	_, err := s.write.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// VacuumInto writes a consistent snapshot of the database to dest, producing a
// single self-contained file with no -wal or -shm companion. dest must not
// already exist.
func (s *Store) VacuumInto(dest string) error {
	_, err := s.write.ExecContext(context.Background(), `VACUUM INTO ?`, dest)
	return err
}
func (s *Store) migrate(ctx context.Context) error {
	// Pin the single writer connection so the foreign_keys pragma and the
	// migration transaction run on the same connection.
	conn, err := s.write.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// The 001 baseline creates the schema_migrations table the rest of the
	// runner depends on. It must only run on a genuinely fresh (or pre-runner
	// legacy) database: it is all CREATE TABLE IF NOT EXISTS, so running it on
	// an already-migrated database would resurrect tables that a later
	// migration dropped (e.g. the retired credential tables).
	if err := bootstrapBaseline(ctx, conn); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, m := range migrations(filepath.Dir(s.path)) {
		applied, err := migrationMarker(ctx, conn, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if m.detect != nil {
			state, err := m.detect(ctx, conn)
			if err != nil {
				return fmt.Errorf("migrate %s: inspect schema: %w", m.version, err)
			}
			switch state {
			case stateApplied:
				// The schema work committed but its marker was lost (an
				// interrupted upgrade under the old runner). Record it.
				if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(?,?)`, m.version, nowText()); err != nil {
					return err
				}
				continue
			case statePartial:
				return fmt.Errorf("migrate %s: schema is partially applied; restore the database from backup or repair it manually before starting", m.version)
			}
		}
		if err := runMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("migrate %s: %w", m.version, err)
		}
	}
	return nil
}

// bootstrapBaseline applies the 001 baseline only when the database has no
// schema_migrations table yet. A database that already carries the runner's
// marker table has completed the baseline at some point in its life, so
// re-running the idempotent baseline would recreate dropped tables (the
// migration-013 baseline-resurrection hazard). A fresh database and a
// pre-runner legacy database both lack the marker table and are bootstrapped.
func bootstrapBaseline(ctx context.Context, conn *sql.Conn) error {
	hasMarkerTable, err := tableExists("schema_migrations")(ctx, conn)
	if err != nil {
		return err
	}
	if hasMarkerTable {
		return nil
	}
	return runMigration(ctx, conn, migration{version: "001", sql: migration001})
}

func nowText() string              { return time.Now().UTC().Format(time.RFC3339Nano) }
func timeText(t time.Time) string  { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t }
func timePtr(t time.Time) *time.Time {
	return &t
}
func nullableTime(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t := parseTime(v.String)
	return &t
}
func nullTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return timeText(*t)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func decodeStrings(v string) []string {
	out := []string{}
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
var ErrSenderNotAllowed = errors.New("sender not allowed")
var ErrConflict = errors.New("conflict")
var ErrQuota = errors.New("storage quota exceeded")
var ErrNoProvider = errors.New("no provider configured")
var ErrSystemAdmin = errors.New("system administrator credentials are managed by the deployment configuration")
var ErrInviteExpired = errors.New("invitation is no longer valid")

func normalizeAddress(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
func normalizeDomain(v string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
}
func normalizeLocal(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func (s *Store) Audit(ctx context.Context, accountID, kind, detail string) {
	_, _ = s.write.ExecContext(ctx, `INSERT INTO audit_log(account_id,kind,detail,created_at) VALUES(?,?,?,?)`, nullString(accountID), kind, detail, nowText())
}

// CountAudit returns the number of audit rows for an account and kind. It is
// used to verify coalescing/rate-limiting behavior.
func (s *Store) CountAudit(ctx context.Context, accountID, kind string) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE account_id=? AND kind=?`, accountID, kind).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountAuditKind returns the number of audit rows of a kind across all accounts.
// Unrouted deliveries have no resolvable account, so their audit rows carry a
// NULL account_id.
func (s *Store) CountAuditKind(ctx context.Context, kind string) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE kind=?`, kind).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
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
