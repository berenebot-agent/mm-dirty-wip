package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

const migration078 = `
CREATE TABLE empty_trash_operations (
 action_id TEXT PRIMARY KEY REFERENCES inbox_message_actions(id) ON DELETE CASCADE,
 phase TEXT NOT NULL DEFAULT 'discovering', folder TEXT NOT NULL DEFAULT '',
 cursor TEXT NOT NULL DEFAULT '', uid_validity INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE empty_trash_targets (
 operation_id TEXT NOT NULL REFERENCES empty_trash_operations(action_id) ON DELETE CASCADE,
 target_id TEXT NOT NULL, target_json TEXT NOT NULL, child_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(operation_id,target_id)
);
CREATE INDEX idx_empty_trash_children ON empty_trash_targets(child_id);`

// TrashTarget is a frozen purge locator. It does not require a metadata cache
// row, so cache quotas/backfill cannot silently limit provider-side emptying.
type TrashTarget struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Folder      string `json:"folder,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	UIDValidity uint32 `json:"uid_validity,omitempty"`
}

type EmptyTrashProgress struct {
	Phase       string `json:"phase"`
	Folder      string `json:"-"`
	Cursor      string `json:"-"`
	UIDValidity uint32 `json:"-"`
	Total       int    `json:"total"`
	Purged      int    `json:"purged"`
	Failed      int    `json:"failed"`
	Ambiguous   int    `json:"ambiguous"`
	Queued      int    `json:"queued"`
}

// EnqueueEmptyTrash coalesces repeated submissions and persists the parent in
// the existing action queue. No provider calls or per-message writes run in HTTP.
func (s *Store) EnqueueEmptyTrash(ctx context.Context, p model.Principal, inbox string) (MessageAction, error) {
	if !p.CanAssist(inbox) {
		return MessageAction{}, ErrForbidden
	}
	if _, err := s.GetInbox(ctx, p, inbox); err != nil {
		return MessageAction{}, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return MessageAction{}, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT a.id FROM inbox_message_actions a JOIN empty_trash_operations o ON o.action_id=a.id WHERE a.account_id=? AND a.inbox_id=? AND a.dismissed=0 AND a.state IN ('pending','running','ambiguous','failed') ORDER BY a.rowid LIMIT 1`, p.AccountID, inbox).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return MessageAction{}, err
	}
	if err == sql.ErrNoRows {
		var pending int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_message_actions WHERE account_id=? AND state IN ('pending','running','ambiguous')`, p.AccountID).Scan(&pending); err != nil {
			return MessageAction{}, err
		}
		if pending >= 10000 {
			return MessageAction{}, ErrConflict
		}
		id, err = idgen.New("act"), nil
		now := nowText()
		_, err = tx.ExecContext(ctx, `INSERT INTO inbox_message_actions(id,account_id,inbox_id,message_id,input_json,next_attempt_at,created_at) VALUES(?,?,?,?,'{"kind":"empty_trash"}',?,?)`, id, p.AccountID, inbox, "empty-trash:"+inbox, now, now)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO empty_trash_operations(action_id) VALUES(?)`, id)
		}
	}
	if err != nil {
		return MessageAction{}, err
	}
	if err = tx.Commit(); err != nil {
		return MessageAction{}, err
	}
	return s.GetMessageAction(ctx, p, id)
}

func (s *Store) EmptyTrashProgress(ctx context.Context, action string) (EmptyTrashProgress, error) {
	var p EmptyTrashProgress
	err := s.read.QueryRowContext(ctx, `SELECT phase,folder,cursor,uid_validity FROM empty_trash_operations WHERE action_id=?`, action).Scan(&p.Phase, &p.Folder, &p.Cursor, &p.UIDValidity)
	if err != nil {
		return p, err
	}
	err = s.read.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(a.state='done'),0),COALESCE(sum(a.state='failed'),0),COALESCE(sum(a.state='ambiguous'),0),COALESCE(sum(t.child_id<>''),0) FROM empty_trash_targets t LEFT JOIN inbox_message_actions a ON a.id=t.child_id WHERE t.operation_id=?`, action).Scan(&p.Total, &p.Purged, &p.Failed, &p.Ambiguous, &p.Queued)
	return p, err
}

// SaveTrashDiscovery commits one page and its resume cursor together. No child
// purge is eligible until discovery has finished, so our deletions cannot shift
// provider pagination. Replayed discovery pages deduplicate by stable locator.
func (s *Store) SaveTrashDiscovery(ctx context.Context, action, folder, cursor string, validity uint32, targets []TrashTarget, complete bool) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var phase string
	if err = tx.QueryRowContext(ctx, `SELECT phase FROM empty_trash_operations WHERE action_id=?`, action).Scan(&phase); err != nil {
		return err
	}
	if phase != "discovering" {
		return ErrConflict
	}
	for _, target := range targets {
		raw, e := json.Marshal(target)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO empty_trash_targets(operation_id,target_id,target_json) VALUES(?,?,?)`, action, target.Kind+":"+target.ID, string(raw)); err != nil {
			return err
		}
	}
	if complete {
		phase = "purging"
	}
	_, err = tx.ExecContext(ctx, `UPDATE empty_trash_operations SET phase=?,folder=?,cursor=?,uid_validity=? WHERE action_id=?`, phase, folder, cursor, validity, action)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SnapshotLocalTrash selects the local target set in one SQL operation, without
// materializing the inbox or creating thousands of per-message transactions.
func (s *Store) SnapshotLocalTrash(ctx context.Context, a MessageAction, complete bool) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO empty_trash_targets(operation_id,target_id,target_json) SELECT ?, 'local:'||id,json_object('kind','local','id',id) FROM messages WHERE account_id=? AND inbox_id=? AND deleted_at IS NOT NULL AND internal=0 AND EXISTS (SELECT 1 FROM empty_trash_operations WHERE action_id=? AND phase='discovering')`, a.ID, a.AccountID, a.InboxID, a.ID)
	if err != nil {
		return err
	}
	if complete {
		_, err = tx.ExecContext(ctx, `UPDATE empty_trash_operations SET phase='purging' WHERE action_id=? AND phase='discovering'`, a.ID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueueTrashBatch atomically links a bounded set of targets to ordinary purge
// actions. A crash cannot enqueue a second purge for an already-linked target.
func (s *Store) QueueTrashBatch(ctx context.Context, a MessageAction) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM empty_trash_targets t JOIN inbox_message_actions a ON a.id=t.child_id WHERE t.operation_id=? AND a.state IN ('pending','running')`, a.ID).Scan(&active); err != nil {
		return 0, err
	}
	if active >= 100 {
		return 0, nil
	}
	var accountPending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_message_actions WHERE account_id=? AND state IN ('pending','running','ambiguous')`, a.AccountID).Scan(&accountPending); err != nil {
		return 0, err
	}
	batch := 100 - active
	if room := 10000 - accountPending; room < batch {
		batch = room
	}
	if batch <= 0 {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT target_id,target_json FROM empty_trash_targets WHERE operation_id=? AND child_id='' AND EXISTS (SELECT 1 FROM empty_trash_operations WHERE action_id=? AND phase='purging') ORDER BY target_id LIMIT ?`, a.ID, a.ID, batch)
	if err != nil {
		return 0, err
	}
	type entry struct{ key, raw string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.key, &e.raw); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		var target TrashTarget
		if err = json.Unmarshal([]byte(e.raw), &target); err != nil {
			return 0, err
		}
		input, _ := json.Marshal(MessageActionInput{Kind: "purge", TrashTarget: &target})
		id, now := idgen.New("act"), nowText()
		messageID := target.ID
		if target.Kind == "microsoft" {
			messageID = "ms_" + target.ID + "_" + a.InboxID
		} else if target.Kind == "imap" {
			messageID = "trash-target:" + e.key
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO inbox_message_actions(id,account_id,inbox_id,message_id,input_json,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,?)`, id, a.AccountID, a.InboxID, messageID, string(input), now, now)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE empty_trash_targets SET child_id=? WHERE operation_id=? AND target_id=?`, id, a.ID, e.key); err != nil {
			return 0, err
		}
	}
	return len(entries), tx.Commit()
}
