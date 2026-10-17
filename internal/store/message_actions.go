package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

const migration064 = `CREATE TABLE inbox_message_actions (
 id TEXT PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 inbox_id TEXT NOT NULL REFERENCES inboxes(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL, input_json TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending',
 attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 next_attempt_at TEXT NOT NULL, claim_owner TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, settled_at TEXT NOT NULL DEFAULT '', dismissed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_message_actions_due ON inbox_message_actions(state,next_attempt_at);
CREATE INDEX idx_message_actions_order ON inbox_message_actions(account_id,inbox_id,message_id,created_at);`

// MessageActionInput describes a single ordered message mutation. Pointer fields
// distinguish omitted state from an explicit false or empty label list.
type MessageActionInput struct {
	Kind        string    `json:"kind"`
	Destination string    `json:"destination,omitempty"`
	Read        *bool     `json:"read,omitempty"`
	Spam        *bool     `json:"spam,omitempty"`
	Labels      *[]string `json:"labels,omitempty"`
	Label       string    `json:"label,omitempty"`
	LabelOp     string    `json:"label_op,omitempty"`
	// ProviderLabel marks Label as a provider-owned name (a Gmail user label or
	// an Outlook category), which may be hierarchical. It relaxes name validation
	// to the provider's own rule instead of the MailMoose-native rule that
	// rejects path separators.
	ProviderLabel bool `json:"provider_label,omitempty"`
	// TrashTarget is populated only by the durable Empty Trash coordinator.
	TrashTarget *TrashTarget `json:"trash_target,omitempty"`
}

type MessageAction struct {
	ID         string              `json:"id"`
	AccountID  string              `json:"-"`
	InboxID    string              `json:"inbox_id"`
	MessageID  string              `json:"message_id"`
	Input      MessageActionInput  `json:"input"`
	State      string              `json:"state"`
	Attempts   int                 `json:"attempts"`
	LastError  string              `json:"error,omitempty"`
	CreatedAt  string              `json:"created_at"`
	EmptyTrash *EmptyTrashProgress `json:"empty_trash,omitempty"`
}

const actionSelect = `SELECT id,account_id,inbox_id,message_id,input_json,state,attempts,last_error,created_at FROM inbox_message_actions`

func scanAction(row interface{ Scan(...any) error }) (MessageAction, error) {
	var a MessageAction
	var input string
	err := row.Scan(&a.ID, &a.AccountID, &a.InboxID, &a.MessageID, &input, &a.State, &a.Attempts, &a.LastError, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(input), &a.Input)
	return a, err
}

func (s *Store) EnqueueMessageAction(ctx context.Context, p model.Principal, inboxID, messageID string, in MessageActionInput) (MessageAction, error) {
	if !p.CanAssist(inboxID) {
		return MessageAction{}, ErrForbidden
	}
	if _, err := s.GetInbox(ctx, p, inboxID); err != nil {
		return MessageAction{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return MessageAction{}, err
	}
	id, now := idgen.New("act"), nowText()
	var pending int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inbox_message_actions WHERE account_id=? AND state IN ('pending','running','ambiguous')`, p.AccountID).Scan(&pending); err != nil {
		return MessageAction{}, err
	}
	if pending >= 10000 {
		return MessageAction{}, ErrConflict
	}
	_, err = s.write.ExecContext(ctx, `INSERT INTO inbox_message_actions(id,account_id,inbox_id,message_id,input_json,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,?)`, id, p.AccountID, inboxID, messageID, string(raw), now, now)
	if err != nil {
		return MessageAction{}, err
	}
	return s.GetMessageAction(ctx, p, id)
}

func (s *Store) GetMessageAction(ctx context.Context, p model.Principal, id string) (MessageAction, error) {
	a, err := scanAction(s.read.QueryRowContext(ctx, actionSelect+` WHERE account_id=? AND id=?`, p.AccountID, id))
	if err == nil && !p.CanRead(a.InboxID) {
		return MessageAction{}, ErrForbidden
	}
	if err == nil && a.Input.Kind == "empty_trash" {
		progress, e := s.EmptyTrashProgress(ctx, a.ID)
		if e != nil {
			return MessageAction{}, e
		}
		a.EmptyTrash = &progress
	}
	return a, err
}

func (s *Store) ListMessageActions(ctx context.Context, p model.Principal) ([]MessageAction, error) {
	rows, err := s.read.QueryContext(ctx, actionSelect+` WHERE account_id=? AND dismissed=0 ORDER BY CASE WHEN json_extract(input_json,'$.kind')='empty_trash' THEN 0 ELSE 1 END,CASE WHEN state IN ('pending','running','ambiguous','failed') THEN 0 ELSE 1 END,rowid DESC LIMIT 200`, p.AccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		if p.CanRead(a.InboxID) {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].Input.Kind == "empty_trash" {
			progress, err := s.EmptyTrashProgress(ctx, out[i].ID)
			if err != nil {
				return nil, err
			}
			out[i].EmptyTrash = &progress
		}
	}
	return out, nil
}

// ClaimMessageAction serializes mutations per message, including across backoff.
// An uncertain operation blocks its successors until explicitly resolved.
func (s *Store) ClaimMessageAction(ctx context.Context, owner string) (MessageAction, bool, error) {
	return s.claimMessageAction(ctx, owner, " ORDER BY rowid LIMIT 1")
}

// ClaimMessageActionAfterInbox selects one action from the next eligible inbox
// in ID order, wrapping after the last inbox. Within that inbox, the oldest
// eligible action wins. Delayed retries and blocked messages do not waste turns.
// The single action worker supplies its last claimed inbox as an in-memory
// cursor; restarting resets rotation without changing durable action state.
func (s *Store) ClaimMessageActionAfterInbox(ctx context.Context, owner, afterInbox string) (MessageAction, bool, error) {
	return s.claimMessageAction(ctx, owner, " ORDER BY CASE WHEN inbox_id>? THEN 0 ELSE 1 END,inbox_id,rowid LIMIT 1", afterInbox)
}

func (s *Store) claimMessageAction(ctx context.Context, owner, order string, orderArgs ...any) (MessageAction, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return MessageAction{}, false, err
	}
	defer tx.Rollback()
	args := append([]any{nowText()}, orderArgs...)
	a, err := scanAction(tx.QueryRowContext(ctx, actionSelect+` WHERE state='pending' AND claim_owner='' AND next_attempt_at<=? AND NOT EXISTS (SELECT 1 FROM inbox_message_actions prior WHERE prior.account_id=inbox_message_actions.account_id AND prior.inbox_id=inbox_message_actions.inbox_id AND prior.message_id=inbox_message_actions.message_id AND prior.rowid<inbox_message_actions.rowid AND prior.state IN ('pending','running','ambiguous'))`+order, args...))
	if err == ErrNotFound {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_message_actions SET state='running',claim_owner=?,attempts=attempts+1 WHERE id=?`, owner, a.ID)
	if err != nil {
		return a, false, err
	}
	err = tx.Commit()
	a.Attempts++
	return a, err == nil, err
}

func (s *Store) SettleMessageAction(ctx context.Context, id, state, reason string, next time.Time) error {
	settled := nowText()
	if state == "pending" {
		settled = ""
	}
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET state=?,last_error=?,next_attempt_at=?,claim_owner='',settled_at=? WHERE id=?`, state, publicRemoteIndexError(reason), timeText(next), settled, id)
	return err
}

// ListMessageActionsForMessage returns every action recorded for one message,
// oldest first. The rechecker uses it to detect whether a newer unsettled action
// already supersedes a stale failure.
func (s *Store) ListMessageActionsForMessage(ctx context.Context, accountID, inboxID, messageID string) ([]MessageAction, error) {
	rows, err := s.read.QueryContext(ctx, actionSelect+` WHERE account_id=? AND inbox_id=? AND message_id=? ORDER BY rowid`, accountID, inboxID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListFailedActionsForRecheck returns failed, non-dismissed actions whose
// recheck is due. Because next_attempt_at on a failed row is the next recheck
// time, the caller passes the current time: an action is due when its recheck
// time has arrived. The interval is applied once, when next_attempt_at is
// written, never again here. Ordering is oldest first so the rechecker works
// through a backlog steadily. It deliberately excludes 'ambiguous' rows: those
// require human verification before they may run again.
func (s *Store) ListFailedActionsForRecheck(ctx context.Context, due time.Time, limit int) ([]MessageAction, error) {
	rows, err := s.read.QueryContext(ctx, actionSelect+` WHERE dismissed=0 AND state='failed' AND next_attempt_at<=? ORDER BY next_attempt_at,rowid LIMIT ?`, timeText(due), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RequeueMessageAction returns a failed action to the pending queue. It is
// conditional on the row still being failed and undismissed, so a human retry or
// dismissal that landed first is never clobbered. Attempts are left intact: the
// rechecker's job is to try again after an interval, not to reset the record of
// how often the action has already run.
func (s *Store) RequeueMessageAction(ctx context.Context, id string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET state='pending',next_attempt_at=?,claim_owner='' WHERE id=? AND state='failed' AND dismissed=0`, nowText(), id)
	return err
}

// DeferMessageActionRecheck pushes a failed action's next recheck back without
// changing its state, used when the rechecker cannot yet determine the provider
// outcome and must try again later.
func (s *Store) DeferMessageActionRecheck(ctx context.Context, id string, next time.Time) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET next_attempt_at=? WHERE id=? AND state='failed' AND dismissed=0`, timeText(next), id)
	return err
}

func (s *Store) RecoverMessageActions(ctx context.Context) error {
	// A crash after a remote move/purge may have committed externally. Never
	// replay those blindly; safe absolute state patches can be retried.
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET state=CASE WHEN json_extract(input_json,'$.kind')='empty_trash' OR (json_extract(input_json,'$.kind')='patch' AND json_extract(input_json,'$.spam') IS NULL) THEN 'pending' ELSE 'ambiguous' END,claim_owner='',last_error='Interrupted action; verify the message before retrying' WHERE state='running'`)
	return err
}

func (s *Store) SweepMessageActions(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM inbox_message_actions WHERE state='done' AND settled_at<? AND NOT EXISTS (SELECT 1 FROM empty_trash_targets t WHERE t.child_id=inbox_message_actions.id)`, timeText(time.Now().Add(-7*24*time.Hour)))
	return err
}

func (s *Store) UpdateMessageAction(ctx context.Context, p model.Principal, id string, retry bool) error {
	a, err := s.GetMessageAction(ctx, p, id)
	if err != nil {
		return err
	}
	if !p.CanAssist(a.InboxID) {
		return ErrForbidden
	}
	if a.State != "failed" && a.State != "ambiguous" && a.State != "done" {
		return ErrConflict
	}
	if retry {
		if a.State != "failed" {
			return ErrConflict
		}
		_, err = s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET state='pending',attempts=0,last_error='',next_attempt_at=?,settled_at='',dismissed=0 WHERE id=? AND state='failed'`, nowText(), id)
	} else {
		_, err = s.write.ExecContext(ctx, `UPDATE inbox_message_actions SET dismissed=1,state=CASE WHEN state='ambiguous' THEN 'failed' ELSE state END WHERE id=? AND state IN ('failed','ambiguous','done')`, id)
	}
	return err
}
