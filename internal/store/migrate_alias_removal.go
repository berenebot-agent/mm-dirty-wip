package store

import (
	"context"
	"database/sql"
	"fmt"
)

// dropExternalAliases removes the external sending-alias feature in one
// transaction. External aliases let an inbox send from an address the account
// does not control; the feature is removed entirely, leaving managed-domain
// aliases (inbox_aliases) untouched.
//
// Retirement rules (matching the feature's own delete semantics):
//
//   - An external-alias-specific *unsent* draft and its attachment files are
//     discarded, along with any send request and queued approval workflow that
//     referenced it. The account and inbox storage counters are refunded the
//     draft body bytes and attachment bytes.
//   - An external-alias-specific *queued* (never-sent) outbound message, its
//     raw MIME file, its idempotency reservation, its FTS entry, its
//     attachments and its delivery-log rows are discarded, and the message
//     bytes are refunded to the account and inbox counters.
//   - SENT history is preserved: a message with status='sent' keeps its row and
//     its from_address attribution, and its delivery-log rows are kept (the
//     dropped external_alias_id column becomes empty, but from_address, to_json
//     and subject retain the attribution).
//   - Ordinary mail and managed aliases are untouched.
//   - Threads left with no remaining message are removed.
//
// No file is unlinked inside the transaction: every raw path to retire is
// recorded in pending_file_cleanup (committed with the row deletions) and swept
// by store.sweepPendingFileCleanup after the migration commits, so a rollback
// never leaves a live row pointing at a missing file.
func dropExternalAliases(ctx context.Context, tx *sql.Tx) error {
	// Guard against a hand-repaired database where 028 never ran: if the alias
	// table and every alias column are already absent, there is nothing to do.
	// Detect this by inspecting the schema, not by trusting a single marker, so a
	// partially-applied state is still cleaned up rather than skipped.
	hasTable, err := txHasTable(ctx, tx, "external_aliases")
	if err != nil {
		return err
	}
	hasMsgCol, err := txHasColumn(ctx, tx, "messages", "sending_external_alias_id")
	if err != nil {
		return err
	}
	hasDraftCol, err := txHasColumn(ctx, tx, "drafts", "from_external_alias_id")
	if err != nil {
		return err
	}
	hasLogCol, err := txHasColumn(ctx, tx, "outbound_delivery_log", "external_alias_id")
	if err != nil {
		return err
	}
	if !hasTable && !hasMsgCol && !hasDraftCol && !hasLogCol {
		return nil
	}

	// Queue every raw MIME file of a queued external-alias send and every
	// attachment file of an external-alias draft for post-commit unlink.
	if hasMsgCol {
		if err = queueRawPaths(ctx, tx, `SELECT COALESCE(raw_path,'') FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent'`); err != nil {
			return err
		}
		if err = queueRawPaths(ctx, tx, `SELECT COALESCE(raw_path,'') FROM outbound_workflow WHERE id IN (
			SELECT approval_workflow_id FROM draft_send_requests WHERE draft_id IN (SELECT id FROM drafts WHERE from_external_alias_id<>'') AND approval_workflow_id<>'')`); err != nil {
			return err
		}
	}
	if hasDraftCol {
		if err = queueRawPaths(ctx, tx, `SELECT COALESCE(da.raw_path,'') FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id WHERE d.from_external_alias_id<>''`); err != nil {
			return err
		}
	}

	// Refund the account and inbox counters for the discarded queued messages:
	// message bytes only. Extracted attachments are already part of a message's
	// size_bytes (the raw MIME size charged at enqueue), so they are not refunded
	// separately.
	if hasMsgCol {
		if err = refundDiscardedTx(ctx, tx, `SELECT m.account_id,m.inbox_id,COALESCE(SUM(m.size_bytes),0)
				FROM messages m WHERE m.sending_external_alias_id<>'' AND m.status<>'sent'
				GROUP BY m.account_id,m.inbox_id`); err != nil {
			return err
		}
	}
	// Refund the draft body and attachment bytes of the discarded drafts, per
	// inbox. Drafts are charged to the inbox's storage counter only (the account
	// counter mirrors it), matching draftBodyBytes/attachment accounting.
	if hasDraftCol {
		type inboxDelta struct {
			account string
			inbox   string
			delta   int64
		}
		var deltas []inboxDelta
		rows, qerr := tx.QueryContext(ctx, `SELECT id,inbox_id,text_body,html_body FROM drafts WHERE from_external_alias_id<>''`)
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var id, inboxID, text, html string
			if qerr = rows.Scan(&id, &inboxID, &text, &html); qerr != nil {
				rows.Close()
				return qerr
			}
			var account string
			if qerr = tx.QueryRowContext(ctx, `SELECT account_id FROM drafts WHERE id=?`, id).Scan(&account); qerr != nil {
				rows.Close()
				return qerr
			}
			deltas = append(deltas, inboxDelta{account: account, inbox: inboxID, delta: -(int64(len(text) + len(html)))})
		}
		rows.Close()
		if qerr = rows.Err(); qerr != nil {
			return qerr
		}
		attRows, aerr := tx.QueryContext(ctx, `SELECT d.account_id,d.inbox_id,COALESCE(SUM(da.size_bytes),0)
				FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id
				WHERE d.from_external_alias_id<>'' GROUP BY d.account_id,d.inbox_id`)
		if aerr != nil {
			return aerr
		}
		for attRows.Next() {
			var d inboxDelta
			if aerr = attRows.Scan(&d.account, &d.inbox, &d.delta); aerr != nil {
				attRows.Close()
				return aerr
			}
			d.delta = -d.delta
			deltas = append(deltas, d)
		}
		attRows.Close()
		if aerr = attRows.Err(); aerr != nil {
			return aerr
		}
		for _, d := range deltas {
			if d.delta == 0 {
				continue
			}
			if err = adjustStorageTx(ctx, tx, d.account, d.inbox, d.delta); err != nil {
				return err
			}
		}
	}

	// Discard external-alias-specific unsent drafts and their send requests and
	// queued approval workflows, then the queued outbound messages. SENT history
	// (messages.status='sent') and its delivery-log rows are deliberately kept.
	stmts := []string{}
	if hasDraftCol {
		stmts = append(stmts,
			`DELETE FROM outbound_workflow WHERE id IN (
				SELECT approval_workflow_id FROM draft_send_requests
				WHERE draft_id IN (SELECT id FROM drafts WHERE from_external_alias_id<>'') AND approval_workflow_id<>'')`,
			`DELETE FROM draft_send_requests WHERE draft_id IN (SELECT id FROM drafts WHERE from_external_alias_id<>'')`,
			`DELETE FROM draft_attachments WHERE draft_id IN (SELECT id FROM drafts WHERE from_external_alias_id<>'')`,
			`DELETE FROM drafts WHERE from_external_alias_id<>''`,
		)
	}
	if hasMsgCol {
		stmts = append(stmts,
			// Delivery-log rows only for the discarded queued messages; sent-alias
			// history logs stay.
			`DELETE FROM outbound_delivery_log WHERE message_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM outbound_workflow WHERE request_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM outbound_idempotency WHERE message_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM events WHERE entity_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM message_fts WHERE message_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM attachments WHERE message_id IN (SELECT id FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent')`,
			`DELETE FROM messages WHERE sending_external_alias_id<>'' AND status<>'sent'`,
		)
	}
	// A default/from-address that named an external alias (never a surviving
	// managed address) is cleared: the sender no longer exists.
	stmts = append(stmts, `UPDATE inboxes SET default_sender='' WHERE default_sender<>'' AND default_sender NOT IN (
			SELECT i.local_part||'@'||d.name FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=inboxes.id
			UNION
			SELECT a.local_part||'@'||d.name FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.inbox_id=inboxes.id)`)
	if hasTable {
		stmts = append(stmts, `DELETE FROM external_aliases`)
	}
	for _, q := range stmts {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("alias removal: %w", err)
		}
	}

	// Remove threads left with no messages at all (ordinary mail keeps its
	// thread; a thread only ever populated by discarded alias mail is orphaned).
	if _, err = tx.ExecContext(ctx, `DELETE FROM threads WHERE id NOT IN (SELECT DISTINCT thread_id FROM messages)`); err != nil {
		return err
	}

	// Drop the now-dead columns and their indexes.
	ddl := []string{}
	if hasMsgCol {
		ddl = append(ddl, `DROP INDEX IF EXISTS idx_messages_external_alias`, `ALTER TABLE messages DROP COLUMN sending_external_alias_id`)
	}
	if hasDraftCol {
		ddl = append(ddl, `ALTER TABLE drafts DROP COLUMN from_external_alias_id`)
	}
	if hasLogCol {
		ddl = append(ddl, `DROP INDEX IF EXISTS idx_outbound_log_external_alias`, `ALTER TABLE outbound_delivery_log DROP COLUMN external_alias_id`)
	}
	if hasTable {
		ddl = append(ddl, `DROP TABLE external_aliases`)
	}
	for _, q := range ddl {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("alias removal ddl: %w", err)
		}
	}
	return nil
}

// queueRawPaths records every non-empty relative path returned by query in the
// durable pending_file_cleanup queue, so files are unlinked after the
// transaction commits.
func queueRawPaths(ctx context.Context, tx *sql.Tx, query string) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	now := nowText()
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return err
		}
		if p == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO pending_file_cleanup(rel_path,created_at) VALUES(?,?)`, p, now); err != nil {
			return err
		}
	}
	return rows.Err()
}

// refundDiscardedTx refunds message bytes (account + inbox) for discarded rows.
func refundDiscardedTx(ctx context.Context, tx *sql.Tx, query string) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	type row struct {
		account, inbox string
		bytes          int64
	}
	var refunds []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.account, &r.inbox, &r.bytes); err != nil {
			rows.Close()
			return err
		}
		refunds = append(refunds, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, r := range refunds {
		if r.bytes == 0 {
			continue
		}
		if err = adjustStorageTx(ctx, tx, r.account, r.inbox, -r.bytes); err != nil {
			return err
		}
	}
	return nil
}

// txHasTable reports whether a table or view exists within the transaction.
func txHasTable(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type IN ('table','view') AND name=?`, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// txHasColumn reports whether a table has a named column within the transaction.
func txHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	var n int
	q := fmt.Sprintf(`SELECT count(*) FROM pragma_table_info('%s') WHERE name=?`, table)
	if err := tx.QueryRowContext(ctx, q, column).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// dropExternalAliasesMigration wires the removal to a transaction.
func dropExternalAliasesMigration() func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		return dropExternalAliases(ctx, tx)
	}
}
