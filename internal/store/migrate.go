package store

import (
	"context"
	"database/sql"
	"fmt"
)

// applyState describes how much of a migration's schema work is already present
// on an existing database. It lets the runner reconcile a database whose
// migration was interrupted before its version marker was recorded.
type applyState int

const (
	stateNotApplied applyState = iota
	stateApplied
	statePartial
)

// migration is one versioned schema change. sql and run are applied together
// with the version marker in a single transaction, so an interrupted upgrade is
// never left half-recorded.
type migration struct {
	version string
	sql     string
	run     func(context.Context, *sql.Tx) error
	fkOff   bool
	detect  func(context.Context, *sql.Conn) (applyState, error)
}

// migrations returns every migration after the 001 baseline, in order. dataDir
// is the on-disk root used by migrations that must read files (for example the
// attachment content-hash backfill).
func migrations(dataDir string) []migration {
	return []migration{
		{version: "002", sql: migration002, detect: stateOf(columnAdded("accounts", "active_outbound_credential_id"))},
		{version: "003", sql: migration003, detect: allOf(columnAdded("inboxes", "allowed_senders_json"), tableExists("blocked_messages"))},
		{version: "004", sql: migration004, detect: stateOf(columnAdded("outbound_idempotency", "status"))},
		{version: "005", sql: migration005, detect: allOf(columnAdded("messages", "status"), tableExists("draft_attachments"))},
		{version: "006", sql: migration006, detect: stateOf(tableExists("outbound_delivery_log"))},
		{version: "007", sql: migration007, detect: stateOf(columnAdded("domains", "outbound_credential_id"))},
		{version: "008", sql: migration008, fkOff: true, detect: stateOf(columnMissing("accounts", "active_outbound_credential_id"))},
		{version: "009", sql: migration009, fkOff: true, detect: allOf(
			columnAdded("domains", "inbound_credential_id"),
			columnAdded("messages", "envelope_recipient"),
			columnAdded("blocked_messages", "envelope_recipient"),
			tableExists("inbound_credentials"),
		)},
		{version: "010", sql: migration010, run: reconcileIdempotency, detect: stateOf(columnAdded("outbound_idempotency", "inbox_id"))},
		{version: "011", sql: migration011, detect: stateOf(columnAdded("messages", "claim_owner"))},
		{version: "012", run: backfillDraftStorage},
		{version: "013", sql: migration013, fkOff: true, detect: allOf(
			tableExists("domain_sending_configs"),
			tableExists("domain_receiving_configs"),
			columnAdded("outbound_delivery_log", "domain_id"),
			columnMissing("outbound_delivery_log", "credential_id"),
			columnMissing("domains", "outbound_credential_id"),
			columnMissing("domains", "inbound_credential_id"),
			columnMissing("inboxes", "outbound_credential_id"),
			tableMissing("outbound_credentials"),
			tableMissing("inbound_credentials"),
		)},
		{version: "014", sql: migration014, detect: allOf(
			columnAdded("drafts", "status"),
			tableExists("draft_send_requests"),
		)},
		{version: "015", sql: migration015, run: backfillAttachmentHashes(dataDir), detect: allOf(
			columnAdded("draft_send_requests", "token_hash"),
			columnAdded("inboxes", "approver_email"),
			columnAdded("draft_attachments", "content_hash"),
			tableExists("inbound_control_messages"),
		)},
		{version: "016", sql: migration016, detect: stateOf(columnAdded("inboxes", "sender_restricted"))},
	}
}

// runMigration applies one migration and records its marker in a single
// transaction on the pinned connection.
func runMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	if m.fkOff {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return err
		}
		defer func() { _, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON") }()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if m.sql != "" {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
	}
	if m.run != nil {
		if err := m.run(ctx, tx); err != nil {
			return err
		}
	}
	if m.fkOff {
		if err := foreignKeyCheck(ctx, tx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version,applied_at) VALUES(?,?)`, m.version, nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("foreign key check failed after migration")
	}
	return rows.Err()
}

func migrationMarker(ctx context.Context, conn *sql.Conn, version string) (bool, error) {
	var n int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, version).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

type detector func(context.Context, *sql.Conn) (bool, error)

func stateOf(d detector) func(context.Context, *sql.Conn) (applyState, error) {
	return func(ctx context.Context, conn *sql.Conn) (applyState, error) {
		ok, err := d(ctx, conn)
		if err != nil {
			return stateNotApplied, err
		}
		if ok {
			return stateApplied, nil
		}
		return stateNotApplied, nil
	}
}

// allOf reports Applied only when every detector is satisfied and Partial when
// only some are, which is how the runner detects a half-finished migration.
func allOf(ds ...detector) func(context.Context, *sql.Conn) (applyState, error) {
	return func(ctx context.Context, conn *sql.Conn) (applyState, error) {
		applied := 0
		for _, d := range ds {
			ok, err := d(ctx, conn)
			if err != nil {
				return stateNotApplied, err
			}
			if ok {
				applied++
			}
		}
		switch {
		case applied == len(ds):
			return stateApplied, nil
		case applied == 0:
			return stateNotApplied, nil
		default:
			return statePartial, nil
		}
	}
}

func columnAdded(table, column string) detector {
	return func(ctx context.Context, conn *sql.Conn) (bool, error) {
		var n int
		q := fmt.Sprintf(`SELECT count(*) FROM pragma_table_info('%s') WHERE name=?`, table)
		if err := conn.QueryRowContext(ctx, q, column).Scan(&n); err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

func columnMissing(table, column string) detector {
	added := columnAdded(table, column)
	return func(ctx context.Context, conn *sql.Conn) (bool, error) {
		ok, err := added(ctx, conn)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
}

func tableExists(name string) detector {
	return func(ctx context.Context, conn *sql.Conn) (bool, error) {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name=?`, name).Scan(&n); err != nil {
			return false, err
		}
		return n > 0, nil
	}
}

func tableMissing(name string) detector {
	exists := tableExists(name)
	return func(ctx context.Context, conn *sql.Conn) (bool, error) {
		ok, err := exists(ctx, conn)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}
}
