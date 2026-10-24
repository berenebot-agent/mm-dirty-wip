// Package dbschema owns the public database lineage and its versioned schema.
package dbschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"time"
)

// Baseline creates the complete public schema, without historical migrations.
//
//go:embed baseline.sql
var Baseline string

// Format identifies the supported public database lineage.
const Format = 1

// BaselineHash pins all application DDL, including FTS5 shadow structures.
const BaselineHash = "9873715c69e1355ddc1c005075051e23db30be7feaae5522fd21538dd157ab37"

// FolderLabelMapping is the public migration that records which additive label
// a converted custom folder became. A domain mailbox organises mail with
// labels, but the folder record and the filed mail's folder membership are
// preserved: the mapping keeps the conversion reversible and idempotent, so a
// rerun reuses the same label instead of deriving a new one.
const FolderLabelMapping = `CREATE TABLE inbox_folder_labels (
  inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
  folder_id TEXT NOT NULL REFERENCES inbox_folders(id) ON DELETE CASCADE,
  label TEXT NOT NULL COLLATE NOCASE,
  created_at TEXT NOT NULL,
  PRIMARY KEY(folder_id)
);`

// FolderLabelMappingHash pins the schema after the folder-label mapping.
const FolderLabelMappingHash = "b78427c46676da829537b76c9a54845b2deaf8882b822603a98535728c4dfbc5"

// SentPlacementBackfill files completed outbound mail that predates explicit
// Sent filing in its inbox's Sent folder, the placement every new send records
// (see MarkSent's filing clause, which picks the same folder of either inbox
// kind). Only mail in an inbox that already holds a Sent folder is touched; an
// inbox whose folder tree was never listed keeps its NULL placements, which the
// Sent-folder reads still widen to the role. Re-applying it is safe: it changes
// data only, so the pinned schema hash never moves.
const SentPlacementBackfill = `UPDATE messages
SET mailbox_id = (SELECT f.id FROM inbox_folders f
                  WHERE f.inbox_id = messages.inbox_id
                    AND f.role = 'sent'
                  ORDER BY f.path LIMIT 1)
WHERE direction = 'outbound'
  AND status = 'sent'
  AND mailbox_id IS NULL
  AND EXISTS (SELECT 1 FROM inbox_folders f
              WHERE f.inbox_id = messages.inbox_id
                AND f.role = 'sent');`

// SentPlacementBackfillHash pins the schema after the Sent placement backfill.
// The backfill is data-only, so the pinned schema stays the previous lineage's.
const SentPlacementBackfillHash = FolderLabelMappingHash

// Queryer is the read surface shared by a pinned connection and a transaction.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type migration struct {
	version string
	sql     string
	hash    string
}

func migrations() []migration {
	return []migration{
		{"001", Baseline, BaselineHash},
		{"002", FolderLabelMapping, FolderLabelMappingHash},
		{"003", SentPlacementBackfill, SentPlacementBackfillHash},
	}
}

// Migrate accepts only an empty database or an exact supported public schema.
func Migrate(ctx context.Context, conn *sql.Conn) error {
	return migrate(ctx, conn, migrations())
}

func migrate(ctx context.Context, conn *sql.Conn, migrations []migration) error {
	if len(migrations) == 0 {
		return fmt.Errorf("no public baseline registered")
	}
	for i, m := range migrations {
		if m.version != fmt.Sprintf("%03d", i+1) || len(m.hash) != 64 {
			return fmt.Errorf("invalid registered migration %q", m.version)
		}
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var format, objects int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema`).Scan(&objects); err != nil {
		return err
	}
	applied := 0
	if objects != 0 || format != 0 {
		if format != Format {
			return fmt.Errorf("unsupported database lineage %d: use a matching application release and a consistent backup; no automatic conversion is supported", format)
		}
		applied, err = checkMarkers(ctx, tx, migrations)
		if err != nil {
			return err
		}
		if err := CheckHash(ctx, tx, migrations[applied-1].hash); err != nil {
			return err
		}
	}
	for _, m := range migrations[applied:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %s: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, m.version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if err := CheckHash(ctx, tx, m.hash); err != nil {
			return fmt.Errorf("migration %s: %w", m.version, err)
		}
	}
	if applied != len(migrations) {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", Format)); err != nil {
			return err
		}
		if err := ForeignKeyCheck(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func checkMarkers(ctx context.Context, q Queryer, migrations []migration) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY CAST(version AS INTEGER),version`)
	if err != nil {
		return 0, fmt.Errorf("invalid migration metadata: %w", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return 0, err
		}
		if i == len(migrations) || version != migrations[i].version {
			return 0, fmt.Errorf("unsupported or incomplete migration sequence at %q", version)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if i == 0 {
		return 0, fmt.Errorf("public database has no baseline marker")
	}
	return i, nil
}

// Validate checks the current public schema without changing it.
func Validate(ctx context.Context, q Queryer) error {
	var format int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&format); err != nil {
		return err
	}
	if format != Format {
		return fmt.Errorf("unsupported database lineage %d", format)
	}
	current := migrations()
	applied, err := checkMarkers(ctx, q, current)
	if err != nil {
		return err
	}
	if applied != len(current) {
		return fmt.Errorf("database requires a supported public migration")
	}
	return CheckHash(ctx, q, current[len(current)-1].hash)
}

// CheckHash rejects schema drift, including changed constraints or index predicates.
func CheckHash(ctx context.Context, q Queryer, expected string) error {
	got, err := Fingerprint(ctx, q)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("database schema differs from the supported schema (got %s, expected %s); no automatic repair is permitted", got, expected)
	}
	return nil
}

// Fingerprint covers object kinds, names, owners and complete DDL, not data.
func Fingerprint(ctx context.Context, q Queryer) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE substr(name,1,7)<>'sqlite_' ORDER BY type,name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var kind, name, table, ddl string
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil {
			return "", err
		}
		for _, value := range []string{kind, name, table, sqlTokens(ddl)} {
			fmt.Fprintf(h, "%d:", len(value))
			io.WriteString(h, value)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), rows.Err()
}

// sqlTokens ignores formatting/comments but preserves quoted values and token boundaries.
func sqlTokens(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		start := i
		switch {
		case strings.ContainsRune(" \t\r\n\f", rune(s[i])):
			i++
			continue
		case strings.HasPrefix(s[i:], "--"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return s
			}
			i += end + 4
			continue
		case strings.ContainsRune("'\"`[", rune(s[i])):
			end := s[i]
			if end == '[' {
				end = ']'
			}
			i++
			for i < len(s) {
				if s[i] != end {
					i++
					continue
				}
				i++
				if i < len(s) && s[i] == end && end != ']' {
					i++
					continue
				}
				break
			}
		default:
			i++
			if word(s[start]) {
				for i < len(s) && word(s[i]) {
					i++
				}
			}
		}
		fmt.Fprintf(&out, "%d:%s", i-start, s[start:i])
	}
	return out.String()
}

func word(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 128
}

// ForeignKeyCheck must run before committing schema changes.
func ForeignKeyCheck(ctx context.Context, q Queryer) error {
	rows, err := q.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("database foreign key check failed")
	}
	return rows.Err()
}
