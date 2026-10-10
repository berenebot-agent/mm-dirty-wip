package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// This file is the durable remote-actions surface: the separate state machine
// that copies a successfully sent message into a standalone inbox's remote Sent
// folder. It is intentionally independent of the SMTP submission: the copy is
// enqueued only after a send has committed, and a failing copy never re-runs the
// SMTP submission. Every copy attempt is an IMAP APPEND of the frozen raw MIME,
// verified by Message-ID when the server reports no APPENDUID, so a copy can never
// duplicate a delivery the provider already accepted.
//
// Sent-copy states. They mirror the handoff publication vocabulary but are scoped
// to the copy job alone.
const (
	// RemoteCopyPending: queued, not yet attempted.
	RemoteCopyPending = "pending"
	// RemoteCopyCopied: the APPEND was confirmed (APPENDUID, or a Message-ID
	// lookup found exactly one match).
	RemoteCopyCopied = "copied"
	// RemoteCopyAmbiguous: the APPEND outcome could not be determined; the
	// message may or may not exist in Sent. Terminal for this job: it is never
	// re-appended automatically, because that could duplicate it.
	RemoteCopyAmbiguous = "ambiguous"
	// RemoteCopyFailed: the APPEND failed terminally (permanent provider error).
	RemoteCopyFailed = "failed"
)

// ErrNoSentFolder is returned when a standalone inbox has no remote Sent folder
// to copy into, so the copy cannot be created. It is a permanent condition.
var ErrNoSentFolder = errors.New("standalone inbox has no remote Sent folder")

// RemoteSentCopy is a durable pending copy of a sent message into an inbox's
// remote Sent folder.
type RemoteSentCopy struct {
	ID             string
	AccountID      string
	InboxID        string
	MessageID      string
	FolderPath     string
	RFCMessageID   string
	MessageIDHdr   string
	RawPath        string
	SizeBytes      int64
	State          string
	RemoteUIDValid uint32
	RemoteUID      uint32
	Attempts       int
	LastError      string
	NextAttemptAt  time.Time
	ClaimOwner     string
	ClaimExpiresAt time.Time
	CreatedAt      time.Time
	CopiedAt       *time.Time
	TerminalAt     *time.Time
}

// RemoteSentCopyInput is the durable content of a new sent-copy job.
type RemoteSentCopyInput struct {
	MessageID    string
	FolderPath   string
	RFCMessageID string
	MessageIDHdr string
	RawPath      string
	SizeBytes    int64
}

const remoteSentCopySelect = `SELECT id,account_id,inbox_id,message_id,folder_path,rfc_message_id,message_id_header,raw_path,size_bytes,state,remote_uid_validity,remote_uid,attempts,last_error,next_attempt_at,claim_owner,claim_expires_at,created_at,copied_at,terminal_at FROM remote_sent_copies`

func scanRemoteSentCopy(row interface{ Scan(...any) error }) (RemoteSentCopy, error) {
	var c RemoteSentCopy
	var next, claimExp, created string
	var messageID, copied, terminal sql.NullString
	err := row.Scan(&c.ID, &c.AccountID, &c.InboxID, &messageID, &c.FolderPath, &c.RFCMessageID, &c.MessageIDHdr, &c.RawPath, &c.SizeBytes, &c.State, &c.RemoteUIDValid, &c.RemoteUID, &c.Attempts, &c.LastError, &next, &c.ClaimOwner, &claimExp, &created, &copied, &terminal)
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.MessageID = messageID.String
	c.NextAttemptAt = parseTime(next)
	c.ClaimExpiresAt = parseTime(claimExp)
	c.CreatedAt = parseTime(created)
	c.CopiedAt = nullableTime(copied)
	c.TerminalAt = nullableTime(terminal)
	return c, nil
}

// EnqueueRemoteSentCopy queues a durable copy of a sent message into the inbox's
// remote Sent folder. It is called after a send commits; it never touches the
// message's own lifecycle. A blank folder path means "resolve the Sent folder at
// copy time" (the worker reads the inbox's Sent-role folder), so a folder rename
// after enqueue still resolves correctly.
func (s *Store) EnqueueRemoteSentCopy(ctx context.Context, accountID, inboxID string, in RemoteSentCopyInput) (RemoteSentCopy, error) {
	if kind, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return RemoteSentCopy{}, err
	} else if kind != model.InboxKindStandalone {
		return RemoteSentCopy{}, ErrStandaloneRequired
	}
	now := nowText()
	id := idgen.New("rsc")
	if _, err := s.write.ExecContext(ctx, `INSERT INTO remote_sent_copies(id,account_id,inbox_id,message_id,folder_path,rfc_message_id,message_id_header,raw_path,size_bytes,state,attempts,last_error,next_attempt_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,0,'',?,?)`,
		id, accountID, inboxID, nullString(in.MessageID), in.FolderPath, in.RFCMessageID, in.MessageIDHdr, in.RawPath, in.SizeBytes, RemoteCopyPending, now, now); err != nil {
		return RemoteSentCopy{}, err
	}
	return scanRemoteSentCopy(s.write.QueryRowContext(ctx, remoteSentCopySelect+` WHERE id=? AND account_id=?`, id, accountID))
}

// EnqueueUniqueRemoteSentCopy enqueues a sent copy unless one identical job
// already exists (same inbox and RFC Message-ID), so a retried send path never
// queues a second copy of the same message.
func (s *Store) EnqueueUniqueRemoteSentCopy(ctx context.Context, accountID, inboxID string, in RemoteSentCopyInput) (RemoteSentCopy, bool, error) {
	if in.RFCMessageID != "" {
		existing, err := s.GetRemoteSentCopyByMessageID(ctx, accountID, inboxID, in.RFCMessageID)
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return RemoteSentCopy{}, false, err
		}
	}
	c, err := s.EnqueueRemoteSentCopy(ctx, accountID, inboxID, in)
	if err != nil {
		return RemoteSentCopy{}, false, err
	}
	return c, true, nil
}

// GetRemoteSentCopyByMessageID returns the copy job for a message's RFC
// Message-ID within an inbox.
func (s *Store) GetRemoteSentCopyByMessageID(ctx context.Context, accountID, inboxID, rfcMessageID string) (RemoteSentCopy, error) {
	return scanRemoteSentCopy(s.read.QueryRowContext(ctx, remoteSentCopySelect+` WHERE account_id=? AND inbox_id=? AND rfc_message_id=? ORDER BY created_at DESC LIMIT 1`, accountID, inboxID, rfcMessageID))
}

// GetRemoteSentCopy loads one copy job by id.
func (s *Store) GetRemoteSentCopy(ctx context.Context, accountID, id string) (RemoteSentCopy, error) {
	return scanRemoteSentCopy(s.read.QueryRowContext(ctx, remoteSentCopySelect+` WHERE account_id=? AND id=?`, accountID, id))
}

// ClaimNextRemoteSentCopy atomically claims the next due pending copy job for the
// worker.
func (s *Store) ClaimNextRemoteSentCopy(ctx context.Context, now time.Time, owner string, lease time.Duration) (RemoteSentCopy, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return RemoteSentCopy{}, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM remote_sent_copies WHERE state=? AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY created_at ASC LIMIT 1`, RemoteCopyPending, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return RemoteSentCopy{}, false, nil
	}
	if err != nil {
		return RemoteSentCopy{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE remote_sent_copies SET claim_owner=?,claim_expires_at=? WHERE id=?`, owner, timeText(now.Add(lease)), id); err != nil {
		return RemoteSentCopy{}, false, err
	}
	c, err := scanRemoteSentCopy(tx.QueryRowContext(ctx, remoteSentCopySelect+` WHERE id=?`, id))
	if err != nil {
		return RemoteSentCopy{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return RemoteSentCopy{}, false, err
	}
	return c, true, nil
}

// RecoverAbandonedRemoteSentCopyClaims clears copy claims left by a previous
// process so the worker re-drives them.
func (s *Store) RecoverAbandonedRemoteSentCopyClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE remote_sent_copies SET claim_owner='',claim_expires_at='' WHERE state=? AND claim_owner!=''`, RemoteCopyPending)
	return err
}

// MarkRemoteSentCopyDone records a confirmed copy. remoteUID/uidValidity may be
// zero when the caller confirmed by Message-ID.
func (s *Store) MarkRemoteSentCopyDone(ctx context.Context, accountID, id string, uidValidity, remoteUID uint32) error {
	res, err := s.write.ExecContext(ctx, `UPDATE remote_sent_copies SET state=?,remote_uid_validity=?,remote_uid=?,copied_at=?,terminal_at=?,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND state=?`,
		RemoteCopyCopied, uidValidity, remoteUID, nowText(), nowText(), id, accountID, RemoteCopyPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// MarkRemoteSentCopyAmbiguous records an unverifiable copy outcome. It is
// terminal: the copy is never re-appended automatically, because doing so could
// duplicate the sent message.
func (s *Store) MarkRemoteSentCopyAmbiguous(ctx context.Context, accountID, id, reason string) error {
	return s.settleRemoteSentCopyTerminal(ctx, accountID, id, RemoteCopyAmbiguous, reason)
}

// MarkRemoteSentCopyFailed records a terminal copy failure.
func (s *Store) MarkRemoteSentCopyFailed(ctx context.Context, accountID, id, reason string) error {
	return s.settleRemoteSentCopyTerminal(ctx, accountID, id, RemoteCopyFailed, reason)
}

func (s *Store) settleRemoteSentCopyTerminal(ctx context.Context, accountID, id, state, reason string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE remote_sent_copies SET state=?,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at='',terminal_at=? WHERE id=? AND account_id=? AND state=?`,
		state, publicRemoteIndexError(reason), nowText(), id, accountID, RemoteCopyPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// RetryRemoteSentCopy returns a pending copy job to the queue after a transient
// failure, counting the attempt and scheduling the next try. It never touches the
// message or the SMTP send.
func (s *Store) RetryRemoteSentCopy(ctx context.Context, accountID, id, reason string, next time.Time) error {
	res, err := s.write.ExecContext(ctx, `UPDATE remote_sent_copies SET attempts=attempts+1,last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND state=?`,
		publicRemoteIndexError(reason), timeText(next), id, accountID, RemoteCopyPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// SweepRemoteSentCopies deletes copy jobs whose terminal state is older than the
// cutoff and returns the raw file paths the caller must remove. A copied or
// ambiguous job's frozen raw MIME may be removed once terminal; the remote copy
// (if any) is unaffected.
func (s *Store) SweepRemoteSentCopies(ctx context.Context, cutoff time.Time) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,raw_path FROM remote_sent_copies WHERE terminal_at IS NOT NULL AND terminal_at<>'' AND terminal_at<=?`, timeText(cutoff))
	if err != nil {
		return nil, err
	}
	var ids, paths []string
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		if path != "" {
			paths = append(paths, path)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM remote_sent_copies WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}
