package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// This file is the durable remote event/detection surface for standalone (IMAP)
// inboxes. It owns:
//
//   - the per-folder detection cursor, which turns a live UID set into "these
//     UIDs are genuinely new" without flooding on the first observation of an
//     existing mailbox;
//   - the durable arrival record, one per detected message, keyed so a repeated
//     detection is idempotent and no arrival is ever lost or double-counted;
//   - the notification baseline, so a restart (or a re-detect) never re-notifies
//     a message and an ordinary metadata read never advances the notification
//     baseline;
//   - the durable ACK/auto-action state for a remote arrival (mark read, delayed
//     move to Trash), independent of the metadata index because a remote body is
//     never archived.
//
// Detection is deliberately distinct from the metadata index (inbox_remote_messages):
// an ordinary reconcile/read updates the index but must NOT create an arrival or
// advance the notification baseline. Only the watcher's detection pass, driven by
// the durable cursor, creates arrivals.

// RemoteCursor is the durable per-folder detection anchor.
type RemoteCursor struct {
	InboxID      string
	FolderPath   string
	UIDValidity  uint32
	LastUID      uint32
	BaselineDone bool
	UpdatedAt    time.Time
}

// RemoteArrival is a durably-recorded detected arrival. It is the unit of the
// proactive fan-out: one per genuinely new message. delivery_state is the
// webhook/Hermes fan-out state, never a live-read state.
type RemoteArrival struct {
	ID             string
	AccountID      string
	InboxID        string
	FolderPath     string
	UIDValidity    uint32
	UID            uint32
	RFCMessageID   string
	FromName       string
	FromAddress    string
	Subject        string
	SizeBytes      int64
	InternalDate   *time.Time
	DeliveryState  string
	Attempts       int
	NextAttemptAt  time.Time
	ClaimOwner     string
	ClaimExpiresAt time.Time
	LastError      string
	Control        bool
	CreatedAt      time.Time
	DetectedAt     time.Time
	SettledAt      *time.Time
}

// Remote arrival delivery states.
const (
	// RemoteArrivalPending: detected, proactive fan-out not yet settled.
	RemoteArrivalPending = "pending"
	// RemoteArrivalDelivered: the proactive fan-out settled (all demand-based
	// clients were dispatched, or there were none).
	RemoteArrivalDelivered = "delivered"
	// RemoteArrivalSkipped: the arrival is one the proactive path never forwards
	// (an approval control message, or a handoff's own notification/echo).
	RemoteArrivalSkipped = "skipped"
	// RemoteArrivalFailed: a terminal proactive fan-out failure.
	RemoteArrivalFailed = "failed"
)

// GetRemoteCursor returns a folder's detection cursor, or ok=false when none has
// been established.
func (s *Store) GetRemoteCursor(ctx context.Context, accountID, inboxID, folderPath string) (RemoteCursor, bool, error) {
	var c RemoteCursor
	var done int
	var updated string
	err := s.read.QueryRowContext(ctx, `SELECT inbox_id,folder_path,remote_uid_validity,last_uid,baseline_done,updated_at FROM inbox_remote_cursors WHERE account_id=? AND inbox_id=? AND folder_path=?`, accountID, inboxID, folderPath).
		Scan(&c.InboxID, &c.FolderPath, &c.UIDValidity, &c.LastUID, &done, &updated)
	if err == sql.ErrNoRows {
		return RemoteCursor{}, false, nil
	}
	if err != nil {
		return RemoteCursor{}, false, err
	}
	c.BaselineDone = done != 0
	c.UpdatedAt = parseTime(updated)
	return c, true, nil
}

// EstablishRemoteBaseline records a folder's detection baseline without emitting
// any arrival: last_uid is set to the current highest UID and baseline_done=1, so
// only strictly-greater UIDs are new on later passes. It is idempotent while the
// UIDVALIDITY is unchanged. A UIDVALIDITY change re-establishes the baseline (a
// new generation) so no stale UID is treated as new mail.
func (s *Store) EstablishRemoteBaseline(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, maxUID uint32) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO inbox_remote_cursors(inbox_id,account_id,folder_path,remote_uid_validity,last_uid,baseline_done,updated_at) VALUES(?,?,?,?,?,1,?)
		ON CONFLICT(inbox_id,folder_path) DO UPDATE SET
			remote_uid_validity=excluded.remote_uid_validity,
			last_uid=CASE WHEN inbox_remote_cursors.remote_uid_validity IS excluded.remote_uid_validity
				THEN MAX(inbox_remote_cursors.last_uid,excluded.last_uid) ELSE excluded.last_uid END,
			baseline_done=1,
			updated_at=excluded.updated_at
		WHERE inbox_remote_cursors.remote_uid_validity IS NOT excluded.remote_uid_validity OR inbox_remote_cursors.baseline_done=0`,
		inboxID, accountID, folderPath, uidValidity, maxUID, nowText())
	return err
}

// AdvanceRemoteCursor moves a folder's cursor past the given UID after the
// arrivals up to it have been persisted. It never moves the cursor backwards and
// never crosses a UIDVALIDITY change.
func (s *Store) AdvanceRemoteCursor(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, lastUID uint32) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_cursors SET last_uid=MAX(last_uid,?),updated_at=? WHERE account_id=? AND inbox_id=? AND folder_path=? AND remote_uid_validity=?`,
		lastUID, nowText(), accountID, inboxID, folderPath, uidValidity)
	return err
}

// RemoteArrivalInput is the detected content of a new arrival to persist.
type RemoteArrivalInput struct {
	FolderPath   string
	UIDValidity  uint32
	UID          uint32
	RFCMessageID string
	FromName     string
	FromAddress  string
	Subject      string
	SizeBytes    int64
	InternalDate *time.Time
	Control      bool
}

// RecordRemoteArrival durably records a detected arrival and returns it plus
// whether it was newly inserted. It is idempotent on (inbox, folder,
// uid_validity, uid): a re-detection returns the existing row with inserted=false
// and emits no second event. It does NOT create a metadata-index row: detection
// is independent of the read index.
func (s *Store) RecordRemoteArrival(ctx context.Context, accountID, inboxID string, in RemoteArrivalInput) (RemoteArrival, bool, error) {
	if kind, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return RemoteArrival{}, false, err
	} else if kind != model.InboxKindStandalone {
		return RemoteArrival{}, false, ErrStandaloneRequired
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return RemoteArrival{}, false, err
	}
	defer tx.Rollback()
	now := nowText()
	id := idgen.New("rar")
	res, err := tx.ExecContext(ctx, `INSERT INTO inbox_remote_arrivals(id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,from_name,from_address,subject,size_bytes,internal_date,delivery_state,delivery_attempts,next_attempt_at,claim_owner,claim_expires_at,last_error,control,created_at,detected_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0,'','','','',?,?,?)
		ON CONFLICT(inbox_id,folder_path,remote_uid_validity,remote_uid) DO NOTHING`,
		id, accountID, inboxID, in.FolderPath, in.UIDValidity, in.UID, in.RFCMessageID, in.FromName, in.FromAddress, in.Subject, in.SizeBytes, nullTimePtr(in.InternalDate), RemoteArrivalPending, boolInt(in.Control), now, now)
	if err != nil {
		return RemoteArrival{}, false, err
	}
	n, _ := res.RowsAffected()
	row, err := scanRemoteArrival(tx.QueryRowContext(ctx, remoteArrivalSelect+` WHERE account_id=? AND inbox_id=? AND folder_path=? AND remote_uid_validity=? AND remote_uid=?`, accountID, inboxID, in.FolderPath, in.UIDValidity, in.UID))
	if err != nil {
		return RemoteArrival{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return RemoteArrival{}, false, err
	}
	return row, n > 0, nil
}

const remoteArrivalSelect = `SELECT id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,from_name,from_address,subject,size_bytes,internal_date,delivery_state,delivery_attempts,next_attempt_at,claim_owner,claim_expires_at,last_error,control,created_at,detected_at,settled_at FROM inbox_remote_arrivals`

func scanRemoteArrival(row interface{ Scan(...any) error }) (RemoteArrival, error) {
	var a RemoteArrival
	var internal, next, claimExp, created, detected, settled sql.NullString
	var control int
	err := row.Scan(&a.ID, &a.AccountID, &a.InboxID, &a.FolderPath, &a.UIDValidity, &a.UID, &a.RFCMessageID, &a.FromName, &a.FromAddress, &a.Subject, &a.SizeBytes, &internal, &a.DeliveryState, &a.Attempts, &next, &a.ClaimOwner, &claimExp, &a.LastError, &control, &created, &detected, &settled)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Control = control != 0
	a.InternalDate = nullableTime(internal)
	a.NextAttemptAt = parseTime(next.String)
	a.ClaimExpiresAt = parseTime(claimExp.String)
	a.CreatedAt = parseTime(created.String)
	a.DetectedAt = parseTime(detected.String)
	a.SettledAt = nullableTime(settled)
	return a, nil
}

// GetRemoteArrival loads one arrival by id.
func (s *Store) GetRemoteArrival(ctx context.Context, accountID, id string) (RemoteArrival, error) {
	return scanRemoteArrival(s.read.QueryRowContext(ctx, remoteArrivalSelect+` WHERE account_id=? AND id=?`, accountID, id))
}

// RemoteArrivalMessageID resolves the RFC Message-ID of an arrival.
func (s *Store) RemoteArrivalMessageID(ctx context.Context, accountID, id string) (string, error) {
	a, err := s.GetRemoteArrival(ctx, accountID, id)
	if err != nil {
		return "", err
	}
	return a.RFCMessageID, nil
}

// RemoteMessageForArrival resolves the cached metadata row that a detected
// arrival refers to, so the canonical read surface (which addresses a remote
// message by its inbox_remote_messages.id) can serve a message a client learned
// about from an arrival event. The event's entity id is the arrival id, which is
// deliberately distinct from the metadata id: detection is independent of the
// metadata index and may run before the reconcile that creates the row. When the
// row is missing it is created from the arrival's own durable header fields, so
// a client following an event can always read the message. A UIDVALIDITY the
// arrival recorded is honoured; an arrival for a folder whose UIDVALIDITY has
// since changed cannot be resolved to a current row and reports ErrNotFound.
func (s *Store) RemoteMessageForArrival(ctx context.Context, accountID, arrivalID string) (RemoteMessage, error) {
	a, err := s.GetRemoteArrival(ctx, accountID, arrivalID)
	if err != nil {
		return RemoteMessage{}, err
	}
	if s.IsGoogle(ctx, accountID, a.InboxID) {
		id, e := s.GoogleLocalID(ctx, accountID, a.InboxID, strings.TrimPrefix(a.FolderPath, "gmail:"))
		if e != nil {
			return RemoteMessage{}, e
		}
		return s.GetRemoteMessage(ctx, accountID, a.InboxID, id)
	}
	if rec, gerr := s.GetRemoteMessageByUID(ctx, accountID, a.InboxID, a.FolderPath, a.UIDValidity, a.UID); gerr == nil {
		return rec, nil
	} else if !errors.Is(gerr, ErrNotFound) {
		return RemoteMessage{}, gerr
	}
	// Not yet indexed: materialize a metadata row from the arrival's durable
	// header fields so the canonical read resolves. The next reconcile upserts
	// the same (inbox, folder, uidvalidity, uid) row in place.
	received := ""
	if a.InternalDate != nil {
		received = a.InternalDate.UTC().Format(time.RFC3339Nano)
	}
	in := RemoteMessageInput{
		FolderPath:   a.FolderPath,
		UIDValidity:  a.UIDValidity,
		UID:          a.UID,
		RFCMessageID: a.RFCMessageID,
		FromName:     a.FromName,
		FromAddress:  a.FromAddress,
		Subject:      a.Subject,
		SizeBytes:    a.SizeBytes,
	}
	if received != "" {
		in.ReceivedAt = &received
	}
	return s.UpsertRemoteMessage(ctx, accountID, a.InboxID, in)
}

// ClaimNextRemoteArrival atomically claims the next due pending arrival for the
// proactive fan-out. It returns ok=false when none is due.
func (s *Store) ClaimNextRemoteArrival(ctx context.Context, now time.Time, owner string, lease time.Duration) (RemoteArrival, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return RemoteArrival{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM inbox_remote_arrivals WHERE delivery_state=? AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY detected_at ASC LIMIT 1`, RemoteArrivalPending, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return RemoteArrival{}, false, nil
	}
	if err != nil {
		return RemoteArrival{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_remote_arrivals SET claim_owner=?,claim_expires_at=?,delivery_attempts=delivery_attempts+1 WHERE id=?`, owner, timeText(now.Add(lease)), id); err != nil {
		return RemoteArrival{}, false, err
	}
	a, err := scanRemoteArrival(tx.QueryRowContext(ctx, remoteArrivalSelect+` WHERE id=?`, id))
	if err != nil {
		return RemoteArrival{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return RemoteArrival{}, false, err
	}
	return a, true, nil
}

// RecoverAbandonedRemoteArrivalClaims clears claims left by a previous process so
// the worker re-drives them.
func (s *Store) RecoverAbandonedRemoteArrivalClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_arrivals SET claim_owner='',claim_expires_at='' WHERE delivery_state=? AND claim_owner!=''`, RemoteArrivalPending)
	return err
}

// SettleRemoteArrival terminally settles a pending arrival's proactive fan-out.
func (s *Store) SettleRemoteArrival(ctx context.Context, accountID, id, state, reason string) error {
	if state != RemoteArrivalDelivered && state != RemoteArrivalSkipped && state != RemoteArrivalFailed {
		return fmt.Errorf("unknown remote arrival state %q", state)
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_arrivals SET delivery_state=?,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at='',settled_at=? WHERE id=? AND account_id=? AND delivery_state=?`,
		state, publicRemoteIndexError(reason), nowText(), id, accountID, RemoteArrivalPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// RetryRemoteArrival returns a pending arrival to the queue with backoff after a
// transient proactive failure. It never touches the arrival's identity.
func (s *Store) RetryRemoteArrival(ctx context.Context, accountID, id, reason string, next time.Time) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_arrivals SET last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND delivery_state=?`,
		publicRemoteIndexError(reason), timeText(next), id, accountID, RemoteArrivalPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// RemoteNotifyBaselineSet reports whether an inbox's notification baseline has been
// established (so the first watcher pass records the baseline without notifying).
func (s *Store) RemoteNotifyBaselineSet(ctx context.Context, accountID, inboxID string) (bool, error) {
	var v int
	err := s.read.QueryRowContext(ctx, `SELECT remote_notify_baseline_set FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&v)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// MarkRemoteNotifyBaselineSet records that the notification baseline is
// established for an inbox.
func (s *Store) MarkRemoteNotifyBaselineSet(ctx context.Context, accountID, inboxID string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET remote_notify_baseline_set=1 WHERE id=? AND account_id=?`, inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRemoteNotified records that a notification decision was made for a remote
// message, keyed by its locator. It is idempotent and never advances a read index.
func (s *Store) MarkRemoteNotified(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, uid uint32) error {
	_, err := s.write.ExecContext(ctx, `INSERT INTO inbox_remote_notifications(inbox_id,account_id,folder_path,remote_uid_validity,remote_uid,notified_at) VALUES(?,?,?,?,?,?) ON CONFLICT(inbox_id,folder_path,remote_uid_validity,remote_uid) DO NOTHING`,
		inboxID, accountID, folderPath, uidValidity, uid, nowText())
	return err
}

// RemoteNotified reports whether a remote message already has a notification
// baseline entry.
func (s *Store) RemoteNotified(ctx context.Context, accountID, inboxID, folderPath string, uidValidity, uid uint32) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_remote_notifications WHERE account_id=? AND inbox_id=? AND folder_path=? AND remote_uid_validity=? AND remote_uid=?`, accountID, inboxID, folderPath, uidValidity, uid).Scan(&n)
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// RemoteAction is the durable ACK/auto-action state for a remote arrival.
type RemoteAction struct {
	ArrivalID    string
	AccountID    string
	InboxID      string
	FolderPath   string
	UIDValidity  uint32
	UID          uint32
	MarkReadDone bool
	TrashDueAt   *time.Time
	TrashDone    bool
	Attempts     int
	LastError    string
	UpdatedAt    time.Time
}

const remoteActionSelect = `SELECT arrival_id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,mark_read_done,trash_due_at,trash_done,attempts,last_error,updated_at FROM inbox_remote_actions`

func scanRemoteAction(row interface{ Scan(...any) error }) (RemoteAction, error) {
	var a RemoteAction
	var markRead, trashDone int
	var trashDue, updated sql.NullString
	err := row.Scan(&a.ArrivalID, &a.AccountID, &a.InboxID, &a.FolderPath, &a.UIDValidity, &a.UID, &markRead, &trashDue, &trashDone, &a.Attempts, &a.LastError, &updated)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.MarkReadDone = markRead != 0
	a.TrashDone = trashDone != 0
	a.TrashDueAt = nullableTime(trashDue)
	a.UpdatedAt = parseTime(updated.String)
	return a, nil
}

// EnsureRemoteAction creates (or returns) the auto-action row for a remote
// arrival, stamping the delayed Trash due instant from the inbox's auto-trash
// window when one is configured. MarkReadDone starts false so the ACK path can
// still set \Seen on the live server exactly once.
func (s *Store) EnsureRemoteAction(ctx context.Context, accountID string, arrival RemoteArrival, trashAfter time.Duration) (RemoteAction, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return RemoteAction{}, err
	}
	defer tx.Rollback()
	now := nowText()
	var due any
	if trashAfter > 0 {
		due = timeText(time.Now().UTC().Add(trashAfter))
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_actions(arrival_id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,mark_read_done,trash_due_at,trash_done,attempts,last_error,updated_at) VALUES(?,?,?,?,?,?,0,?,0,0,'',?)
		ON CONFLICT(arrival_id) DO NOTHING`,
		arrival.ID, accountID, arrival.InboxID, arrival.FolderPath, arrival.UIDValidity, arrival.UID, due, now); err != nil {
		return RemoteAction{}, err
	}
	a, err := scanRemoteAction(tx.QueryRowContext(ctx, remoteActionSelect+` WHERE arrival_id=?`, arrival.ID))
	if err != nil {
		return RemoteAction{}, err
	}
	if err = tx.Commit(); err != nil {
		return RemoteAction{}, err
	}
	return a, nil
}

// SetRemoteActionMarkReadDone records that the ACK path marked the remote message
// read (so it is not repeated).
func (s *Store) SetRemoteActionMarkReadDone(ctx context.Context, accountID, arrivalID string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_actions SET mark_read_done=1,updated_at=? WHERE arrival_id=? AND account_id=?`, nowText(), arrivalID, accountID)
	return err
}

// RecordRemoteActionError records a failed ACK/auto-action attempt and bumps the
// count so a persistent failure backs off instead of spinning.
func (s *Store) RecordRemoteActionError(ctx context.Context, accountID, arrivalID, reason string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_actions SET attempts=attempts+1,last_error=?,updated_at=? WHERE arrival_id=? AND account_id=?`, publicRemoteIndexError(reason), nowText(), arrivalID, accountID)
	return err
}

// ListRemoteActionsDueForTrash returns the auto-action rows whose delayed Trash
// instant has passed and not yet been applied.
func (s *Store) ListRemoteActionsDueForTrash(ctx context.Context, now time.Time, limit int) ([]RemoteAction, error) {
	if limit <= 0 {
		limit = 128
	}
	rows, err := s.read.QueryContext(ctx, remoteActionSelect+` WHERE trash_done=0 AND trash_due_at IS NOT NULL AND trash_due_at<>'' AND trash_due_at<=? ORDER BY trash_due_at ASC LIMIT ?`, timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteAction{}
	for rows.Next() {
		a, scanErr := scanRemoteAction(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkRemoteActionTrashDone records that the delayed Trash move was applied.
func (s *Store) MarkRemoteActionTrashDone(ctx context.Context, accountID, arrivalID string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_actions SET trash_done=1,updated_at=? WHERE arrival_id=? AND account_id=?`, nowText(), arrivalID, accountID)
	return err
}

// SweepRemoteArrivalRetention removes terminal arrivals older than the cutoff and
// their action rows. Raw bodies are never stored for an arrival, so no file is
// unlinked. It returns the number removed.
func (s *Store) SweepRemoteArrivalRetention(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var ids []string
	rows, err := tx.QueryContext(ctx, `SELECT id FROM inbox_remote_arrivals WHERE settled_at IS NOT NULL AND settled_at<>'' AND settled_at<=?`, timeText(cutoff))
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_actions WHERE arrival_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_arrivals WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// EventRemoteMessageReceived is the durable event type emitted for a detected
// remote arrival. It is deliberately the same "message.received" vocabulary the
// relay and webhook selectors already match, so a remote arrival wakes the same
// realtime surfaces; the payload carries remote=true and the remote locator so a
// consumer never mistakes it for a locally-persisted message.
const EventRemoteMessageReceived = "message.received"

// ListRemoteArrivalsForInbox returns an inbox's detections, newest first. It is the
// read surface for a client or the UI to inspect remote detections.
func (s *Store) ListRemoteArrivalsForInbox(ctx context.Context, accountID, inboxID string, limit int) ([]RemoteArrival, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.read.QueryContext(ctx, remoteArrivalSelect+` WHERE account_id=? AND inbox_id=? ORDER BY detected_at DESC LIMIT ?`, accountID, inboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteArrival{}
	for rows.Next() {
		a, scanErr := scanRemoteArrival(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListDetectableRemoteInboxes returns the standalone inboxes the watcher should
// observe: enabled, remote-configured, with a configured credential. It is a
// plain read and never opens a connection.
func (s *Store) ListDetectableRemoteInboxes(ctx context.Context) ([]model.Inbox, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+fullInboxSelectCols+` FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.kind='standalone' AND i.enabled=1 AND i.remote_configured=1 ORDER BY i.created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Inbox{}
	for rows.Next() {
		ib, _, scanErr := scanFullInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, ib)
	}
	return out, rows.Err()
}

// RecordRemoteArrivalEvent writes the durable arrival event and returns it, but
// ONLY if the arrival has not already had its event recorded. It returns a
// zero Event and emitted=false when the event already exists, so a concurrent
// detection pass and the crash-recovery drain can never double-emit. It is emitted
// atomically with the event_recorded flag, so the durable truth and its marker
// commit together.
func (s *Store) RecordRemoteArrivalEvent(ctx context.Context, accountID, inboxID string, a RemoteArrival) (model.Event, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Event{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE inbox_remote_arrivals SET event_recorded=1 WHERE id=? AND account_id=? AND event_recorded=0`, a.ID, accountID)
	if err != nil {
		return model.Event{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The event was already recorded (or the arrival does not exist). Do not
		// emit a second event.
		return model.Event{}, false, nil
	}
	payload := map[string]any{
		"message_id":     a.ID,
		"inbox_id":       a.InboxID,
		"remote":         true,
		"remote_uid":     a.UID,
		"uid_validity":   a.UIDValidity,
		"folder_path":    a.FolderPath,
		"rfc_message_id": a.RFCMessageID,
		"from_address":   a.FromAddress,
		"from_name":      a.FromName,
		"subject":        a.Subject,
		"size_bytes":     a.SizeBytes,
		"is_spam":        false,
	}
	ev, err := insertEventTx(ctx, tx, accountID, inboxID, EventRemoteMessageReceived, a.ID, payload)
	if err != nil {
		return model.Event{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.Event{}, false, err
	}
	return ev, true, nil
}

// IsRemoteArrivalEvent reports whether a durable event describes a remote arrival
// rather than a locally-persisted message. It is the discriminator a relay/webhook
// consumer uses to select the remote delivery path.
func IsRemoteArrivalEvent(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	v, _ := payload["remote"].(bool)
	return v
}

// SetRemoteActionTrashDueForTest forces a remote auto-action's trash due instant.
// It exists only so a test can exercise the delayed-trash sweep without waiting for
// the window; it is not part of the production flow.
func (s *Store) SetRemoteActionTrashDueForTest(ctx context.Context, accountID, arrivalID string, due time.Time) error {
	_, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_actions SET trash_due_at=?,updated_at=? WHERE arrival_id=? AND account_id=?`, timeText(due), nowText(), arrivalID, accountID)
	return err
}

// SetPendingApprovalTokenForTest replaces a draft's pending approval token hash with
// the hash of a known plaintext token, so a test can drive the approval control path
// with a deterministic token. It is a test-only seam and is not used in production.
func (s *Store) SetPendingApprovalTokenForTest(ctx context.Context, accountID, draftID, tokenHash string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE draft_send_requests SET token_hash=? WHERE account_id=? AND draft_id=? AND status=?`, tokenHash, accountID, draftID, model.SendRequestPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
