package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/limits"
	"github.com/dellarb/mailmoose/internal/model"
)

// Workflow is a durable outbound job for system/workflow mail — currently the
// draft approval-request email carrying a one-time token. Workflow mail is not
// mailbox content: it creates no thread, is invisible to the mailbox read
// surface, and never counts against account storage quota. Raw MIME is stored
// under the data directory and retained for a fixed window after the job
// reaches a terminal state, then swept.
type Workflow struct {
	ID                string
	AccountID         string
	InboxID           string
	RequestID         string
	Kind              string
	Provider          string
	From              model.Address
	To                []string
	CC                []string
	BCC               []string
	Subject           string
	Text              string
	HTML              string
	RawPath           string
	SizeBytes         int64
	Status            string
	Attempts          int
	LastError         string
	NextAttemptAt     time.Time
	ClaimOwner        string
	ClaimExpiresAt    time.Time
	ProviderMessageID string
	Redacted          bool
	CreatedAt         time.Time
	SentAt            *time.Time
	TerminalAt        *time.Time
	// SendAttemptState is the durable pre-send marker: empty means no provider
	// send is in flight (or the last one reached a definitive outcome), and a
	// non-empty value is the claim token of the attempt that started the send.
	SendAttemptState string
}

// WorkflowRecord is the durable content of a new workflow job. NewSendRequest,
// when set, creates the linked draft send request in the same transaction.
type WorkflowRecord struct {
	Inbox          model.Inbox
	RequestID      string
	Kind           string
	Provider       string
	From           model.Address
	To, CC, BCC    []string
	Subject        string
	Text, HTML     string
	RawPath        string
	SizeBytes      int64
	LastError      string
	NewSendRequest *SendRequestInsert
}

const workflowSelect = `SELECT id,account_id,inbox_id,request_id,kind,provider,from_name,from_address,to_json,cc_json,bcc_json,subject,text_body,html_body,raw_path,size_bytes,status,attempts,last_error,next_attempt_at,claim_owner,claim_expires_at,provider_message_id,redacted,created_at,sent_at,terminal_at,send_attempt_state FROM outbound_workflow`

func scanWorkflow(row interface{ Scan(...any) error }) (Workflow, error) {
	var w Workflow
	var to, cc, bcc, next, claimExp, created string
	var sent, terminal sql.NullString
	var redacted int
	err := row.Scan(&w.ID, &w.AccountID, &w.InboxID, &w.RequestID, &w.Kind, &w.Provider, &w.From.Name, &w.From.Address, &to, &cc, &bcc, &w.Subject, &w.Text, &w.HTML, &w.RawPath, &w.SizeBytes, &w.Status, &w.Attempts, &w.LastError, &next, &w.ClaimOwner, &claimExp, &w.ProviderMessageID, &redacted, &created, &sent, &terminal, &w.SendAttemptState)
	if err != nil {
		return w, err
	}
	w.Redacted = redacted != 0
	w.To = decodeStrings(to)
	w.CC = decodeStrings(cc)
	w.BCC = decodeStrings(bcc)
	w.NextAttemptAt = parseTime(next)
	w.ClaimExpiresAt = parseTime(claimExp)
	w.CreatedAt = parseTime(created)
	w.SentAt = nullableTime(sent)
	w.TerminalAt = nullableTime(terminal)
	return w, nil
}

// CommitWorkflow enqueues a workflow job and, when the record carries a new
// send request, creates that request and links the two atomically. The request
// starts in notification_status 'queued' and is only presented as awaiting
// approval once the job is actually handed to the outbound path.
func (s *Store) CommitWorkflow(ctx context.Context, r WorkflowRecord) (model.DraftSendRequest, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	defer tx.Rollback()
	now := nowText()
	id := idgen.New("wfl")
	var req model.DraftSendRequest
	if r.NewSendRequest != nil {
		created, cerr := createSendRequestTx(ctx, tx, r.Inbox.AccountID, *r.NewSendRequest)
		if cerr != nil {
			return model.DraftSendRequest{}, model.Event{}, cerr
		}
		req = created
		r.RequestID = created.ID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_workflow(id,account_id,inbox_id,request_id,kind,provider,from_name,from_address,to_json,cc_json,bcc_json,subject,text_body,html_body,raw_path,size_bytes,status,attempts,last_error,next_attempt_at,claim_owner,claim_expires_at,provider_message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',0,?,'','','','',?)`,
		id, r.Inbox.AccountID, r.Inbox.ID, r.RequestID, r.Kind, r.Provider, r.From.Name, r.From.Address, jsonString(r.To), jsonString(r.CC), jsonString(r.BCC), r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, r.LastError, now); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	ev := model.Event{}
	if r.NewSendRequest != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET approval_workflow_id=?,notification_status=?,updated_at=? WHERE id=? AND account_id=?`, id, model.NotificationQueued, now, req.ID, r.Inbox.AccountID); err != nil {
			return model.DraftSendRequest{}, model.Event{}, err
		}
		req.ApprovalWorkflowID = id
		req.NotificationStatus = model.NotificationQueued
		req.UpdatedAt = parseTime(now)
		ev, err = insertEventTx(ctx, tx, r.Inbox.AccountID, req.InboxID, model.EventDraftSendRequested, req.ID, sendRequestPayload(req))
		if err != nil {
			return model.DraftSendRequest{}, model.Event{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	return req, ev, nil
}

// WorkflowAccountID resolves the account id for a workflow job id. Used by the
// outbox worker, which claims jobs without a principal.
func (s *Store) WorkflowAccountID(ctx context.Context, id string) (string, error) {
	var accountID string
	err := s.read.QueryRowContext(ctx, `SELECT account_id FROM outbound_workflow WHERE id=?`, id).Scan(&accountID)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return accountID, err
}

// GetWorkflowInternal loads a workflow job by id without a principal.
func (s *Store) GetWorkflowInternal(ctx context.Context, accountID, id string) (Workflow, error) {
	w, err := scanWorkflow(s.read.QueryRowContext(ctx, workflowSelect+` WHERE account_id=? AND id=?`, accountID, id))
	if err == sql.ErrNoRows {
		return w, ErrNotFound
	}
	return w, err
}

// DomainSendingConfigForWorkflow resolves the sending configuration for a
// workflow job's inbox. A domain inbox uses its managed domain (with
// inheritance); a standalone inbox uses its own remote SMTP binding, so a
// standalone notification is queued and held for a missing provider rather than
// attributed to a non-existent domain. A missing job or foreign account is
// ErrNotFound; an inbox whose binding has no provider is ErrNoProvider.
func (s *Store) DomainSendingConfigForWorkflow(ctx context.Context, accountID, workflowID string) (DomainSendingConfig, error) {
	var domainID sql.NullString
	var inboxID, kind string
	err := s.read.QueryRowContext(ctx, `SELECT i.domain_id,i.id,COALESCE(i.kind,'domain') FROM outbound_workflow w JOIN inboxes i ON i.id=w.inbox_id WHERE w.id=? AND w.account_id=?`, workflowID, accountID).Scan(&domainID, &inboxID, &kind)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	if kind == model.InboxKindStandalone || !domainID.Valid || domainID.String == "" {
		return s.SendingConfigForTarget(ctx, accountID, inboxID, SendingTarget{InboxID: inboxID})
	}
	return s.ResolveDomainSendingConfig(ctx, accountID, domainID.String)
}

// ClaimNextWorkflow atomically claims the next due pending workflow job. It
// returns the job id and the claim token minted for this claim (the same
// per-claim uniqueness as ClaimNextPending), or "" when none is due.
func (s *Store) ClaimNextWorkflow(ctx context.Context, now time.Time, owner string, lease time.Duration) (string, string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM outbound_workflow WHERE status='pending' AND send_attempt_state='' AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY created_at ASC LIMIT 1`, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	claim := owner + ":" + idgen.New("clm")
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner=?,claim_expires_at=? WHERE id=?`, claim, timeText(now.Add(lease)), id); err != nil {
		return "", "", err
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return id, claim, nil
}

// ReleaseWorkflowClaim clears a claim held by the given claim token so the job
// can be retried immediately instead of waiting for the lease to expire. owner
// is the claim token minted by ClaimNextWorkflow (an empty owner releases
// nothing). A job carrying a durable pre-send marker is a no-op: its claim is
// the record of the unresolved in-flight send and must not be dropped.
func (s *Store) ReleaseWorkflowClaim(ctx context.Context, id, owner string) error {
	if owner == "" {
		return nil
	}
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner='',claim_expires_at='' WHERE id=? AND claim_owner=? AND status='pending' AND send_attempt_state=''`, id, owner)
	return err
}

// WorkflowClaimOwner returns the current claim owner for a workflow job.
func (s *Store) WorkflowClaimOwner(ctx context.Context, accountID, id string) (string, error) {
	var owner string
	err := s.read.QueryRowContext(ctx, `SELECT claim_owner FROM outbound_workflow WHERE id=? AND account_id=?`, id, accountID).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return owner, err
}

// WorkflowSendInFlight reports whether a workflow job currently carries an
// unresolved pre-send marker.
func (s *Store) WorkflowSendInFlight(ctx context.Context, accountID, id string) (bool, error) {
	var state string
	err := s.read.QueryRowContext(ctx, `SELECT send_attempt_state FROM outbound_workflow WHERE id=? AND account_id=?`, id, accountID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	return state != "", err
}

// LatestWorkflowForInbox returns the most recently created workflow job for an
// inbox without a principal. It exists for tests and internal callers that need
// to locate the job a commit just enqueued. Ordering is by rowid: created_at is
// stored to second precision, so several jobs committed in the same second tie
// and the insertion order is the only reliable recency signal.
func (s *Store) LatestWorkflowForInbox(ctx context.Context, accountID, inboxID string) (Workflow, error) {
	w, err := scanWorkflow(s.read.QueryRowContext(ctx, workflowSelect+` WHERE account_id=? AND inbox_id=? ORDER BY rowid DESC LIMIT 1`, accountID, inboxID))
	if err == sql.ErrNoRows {
		return w, ErrNotFound
	}
	return w, err
}

// QuarantinedWorkflowSend is a pending workflow job whose provider handoff
// started but never reached a definitive outcome. See QuarantinedSend.
type QuarantinedWorkflowSend struct {
	WorkflowID  string
	InboxID     string
	RequestID   string
	Kind        string
	Subject     string
	Provider    string
	Attempts    int
	LastError   string
	ClaimOwner  string
	ClaimExpire time.Time
	CreatedAt   time.Time
}

// ListQuarantinedWorkflowSends returns the account's pending workflow jobs with
// an unresolved pre-send marker, for operator review.
func (s *Store) ListQuarantinedWorkflowSends(ctx context.Context, accountID string, limit int) ([]QuarantinedWorkflowSend, error) {
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	rows, err := s.read.QueryContext(ctx, `SELECT id,inbox_id,request_id,kind,subject,provider,attempts,last_error,claim_owner,claim_expires_at,created_at FROM outbound_workflow WHERE account_id=? AND status='pending' AND send_attempt_state<>'' ORDER BY created_at ASC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuarantinedWorkflowSend{}
	for rows.Next() {
		var q QuarantinedWorkflowSend
		var claimExp, created string
		if err = rows.Scan(&q.WorkflowID, &q.InboxID, &q.RequestID, &q.Kind, &q.Subject, &q.Provider, &q.Attempts, &q.LastError, &q.ClaimOwner, &claimExp, &created); err != nil {
			return nil, err
		}
		q.ClaimExpire = parseTime(claimExp)
		q.CreatedAt = parseTime(created)
		out = append(out, q)
	}
	return out, rows.Err()
}

// ResolveQuarantinedWorkflowSend is the operator's explicit resolution of a
// quarantined workflow handoff: allowResend true clears the marker and returns
// the job to the queue (the provider may already have accepted it); false marks
// it terminally failed with reason, resolving the linked request notification.
func (s *Store) ResolveQuarantinedWorkflowSend(ctx context.Context, accountID, id, reason string, allowResend bool) error {
	if allowResend {
		res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET send_attempt_state='',attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state<>''`, id, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	w, err := getWorkflowTx(ctx, tx, accountID, id)
	if err != nil {
		return err
	}
	if w.Status != model.WorkflowPending || w.SendAttemptState == "" {
		return ErrConflict
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error=?,next_attempt_at='',claim_owner='',claim_expires_at='',send_attempt_state='',terminal_at=? WHERE id=? AND account_id=?`, reason, now, id, accountID); err != nil {
		return err
	}
	if w.RequestID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET notification_status=?,updated_at=? WHERE id=? AND account_id=?`, model.NotificationFailed, now, w.RequestID, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkWorkflowUncertain records an unresolved approval notification without
// claiming the approver did not receive it.
func (s *Store) MarkWorkflowUncertain(ctx context.Context, accountID, id, reason, claim string) ([]model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := getWorkflowTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	if job.Status != model.WorkflowPending || job.SendAttemptState != claim || claimTaken(job.ClaimOwner, claim) {
		return nil, ErrConflict
	}
	if strings.TrimSpace(reason) == "" {
		reason = "approval notification outcome could not be verified"
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',attempts=attempts+1,last_error=?,terminal_at=?,next_attempt_at='',claim_owner='',claim_expires_at=''
		WHERE id=? AND account_id=? AND status='pending'`, quarantinedReasonPrefix+reason, now, id, accountID); err != nil {
		return nil, err
	}
	var events []model.Event
	if job.RequestID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET notification_status=?,updated_at=? WHERE id=? AND account_id=? AND status='pending'`,
			model.NotificationUncertain, now, job.RequestID, accountID); err != nil {
			return nil, err
		}
		ev, eerr := insertEventTx(ctx, tx, accountID, job.InboxID, model.EventDraftNotificationFailed, job.RequestID,
			map[string]any{"request_id": job.RequestID, "workflow_id": id, "notification_status": model.NotificationUncertain})
		if eerr != nil {
			return nil, eerr
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// BeginWorkflowSend durably records that this attempt is about to hand the
// workflow job to the provider, mirroring BeginMessageSend.
func (s *Store) BeginWorkflowSend(ctx context.Context, accountID, id, claim string) error {
	if claim == "" {
		return nil
	}
	if err := consumeOutboundWriteFault("BeginWorkflowSend", id); err != nil {
		return err
	}
	res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET send_attempt_state=? WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state='' AND `+sendClaimClause, claim, id, accountID, claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	var owner, state, status string
	if err := s.read.QueryRowContext(ctx, `SELECT claim_owner,send_attempt_state,status FROM outbound_workflow WHERE id=? AND account_id=?`, id, accountID).Scan(&owner, &state, &status); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if claimTaken(owner, claim) || (state != "" && state != claim) {
		return ErrClaimTaken
	}
	return ErrNotFound
}

// RecoverAbandonedWorkflowClaims clears claims left by a previous process. A job
// carrying a durable pre-send marker is deliberately excluded, so an unresolved
// provider send is never made claimable (and resent) by startup recovery.
func (s *Store) RecoverAbandonedWorkflowClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner='',claim_expires_at='' WHERE status='pending' AND claim_owner!='' AND send_attempt_state=''`)
	return err
}

// HoldWorkflow defers a pending workflow job without counting a retry. It is
// used when a domain has no outbound provider yet. claim guards against a
// stale attempt holding an expired claim; an empty token can update only an
// unclaimed workflow, never bypass a live claim. A job carrying a durable
// pre-send marker is never held: its claim is left attached so ownership is not
// dropped on a job whose provider outcome is unknown.
func (s *Store) HoldWorkflow(ctx context.Context, accountID, id, reason string, next time.Time, claim string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state='' AND `+sendClaimClause, reason, timeText(next), id, accountID, claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkWorkflowSent records a successful handoff. When tokenExpiry > 0 it starts
// the approval expiry clock, so the token is only time-bounded once the
// approver has actually been notified. It returns the draft events to publish.
func (s *Store) MarkWorkflowSent(ctx context.Context, accountID, id, providerMessageID, provider string, tokenExpiry time.Duration, claim string) ([]model.Event, error) {
	if err := consumeOutboundWriteFault("MarkWorkflowSent", id); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	w, err := getWorkflowTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	if w.Status != model.WorkflowPending {
		return nil, ErrConflict
	}
	// A send that outlived its lease must not record its outcome over a live
	// re-claim; the re-claiming attempt owns the outcome now. An empty owner
	// (the claim was released without a replacement) is still ours to resolve.
	if claimTaken(w.ClaimOwner, claim) {
		return nil, ErrClaimTaken
	}
	// The pre-send marker, when present, must belong to this attempt; a marker
	// left by a different, later attempt means this outcome is stale.
	if w.SendAttemptState != "" && w.SendAttemptState != claim {
		return nil, ErrClaimTaken
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='sent',provider=?,provider_message_id=?,sent_at=?,terminal_at=?,attempts=attempts+1,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='',send_attempt_state='' WHERE id=? AND account_id=? AND status='pending'`, provider, providerMessageID, now, now, id, accountID); err != nil {
		return nil, err
	}
	if err = s.insertWorkflowAttemptTx(ctx, tx, accountID, w.InboxID, provider, id, "sent", providerMessageID, ""); err != nil {
		return nil, err
	}
	var events []model.Event
	if w.RequestID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET notification_status=?,token_expires_at=?,updated_at=? WHERE id=? AND account_id=?`, model.NotificationSent, tokenExpiryText(now, tokenExpiry), now, w.RequestID, accountID); err != nil {
			return nil, err
		}
		ev, eerr := insertEventTx(ctx, tx, accountID, w.InboxID, model.EventDraftNotificationSent, w.RequestID, map[string]any{"request_id": w.RequestID, "workflow_id": id, "notification_status": model.NotificationSent})
		if eerr != nil {
			return nil, eerr
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// MarkWorkflowFailed records a failed handoff. If attempts remain the job is
// returned to pending with a next_attempt_at; otherwise it is terminal and the
// request notification status becomes 'failed' so the UI can surface it.
func (s *Store) MarkWorkflowFailed(ctx context.Context, accountID, id, errText string, nextAttemptAt time.Time, maxAttempts int, provider, claim string) ([]model.Event, error) {
	if err := consumeOutboundWriteFault("MarkWorkflowFailed", id); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	w, err := getWorkflowTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	// A failure may be recorded for a pending job (a retry or a terminal
	// failure) or re-recorded for an already-failed one (an inert duplicate
	// outcome). A sent job must not be dragged back into the failure path.
	if w.Status != model.WorkflowPending && w.Status != model.WorkflowFailed {
		return nil, ErrConflict
	}
	if w.Status == model.WorkflowFailed {
		return nil, nil
	}
	// A stale attempt's failure must not clear another attempt's live claim;
	// that attempt owns the outcome now, so this write is skipped entirely.
	if claimTaken(w.ClaimOwner, claim) {
		return nil, ErrClaimTaken
	}
	// The pre-send marker, when present, must belong to this attempt.
	if w.SendAttemptState != "" && w.SendAttemptState != claim {
		return nil, ErrClaimTaken
	}
	attempts := w.Attempts + 1
	status := model.WorkflowPending
	next := ""
	terminal := false
	if attempts >= maxAttempts {
		status = model.WorkflowFailed
		terminal = true
	} else {
		next = timeText(nextAttemptAt)
	}
	now := nowText()
	if terminal {
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status=?,attempts=?,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at='',send_attempt_state='',terminal_at=? WHERE id=? AND account_id=?`, status, attempts, errText, now, id, accountID); err != nil {
			return nil, err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status=?,attempts=?,last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='',send_attempt_state='' WHERE id=? AND account_id=?`, status, attempts, errText, next, id, accountID); err != nil {
			return nil, err
		}
	}
	if err = s.insertWorkflowAttemptTx(ctx, tx, accountID, w.InboxID, provider, id, "failed", "", errText); err != nil {
		return nil, err
	}
	var events []model.Event
	if terminal && w.RequestID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET notification_status=?,updated_at=? WHERE id=? AND account_id=?`, model.NotificationFailed, now, w.RequestID, accountID); err != nil {
			return nil, err
		}
		ev, eerr := insertEventTx(ctx, tx, accountID, w.InboxID, model.EventDraftNotificationFailed, w.RequestID, map[string]any{"request_id": w.RequestID, "workflow_id": id, "notification_status": model.NotificationFailed, "error": errText})
		if eerr != nil {
			return nil, eerr
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// UpdateWorkflowBodies rewrites the stored text/html bodies of a workflow job
// and marks it redacted, used to remove token markers from the retained copy
// after it reaches a terminal state.
func (s *Store) UpdateWorkflowBodies(ctx context.Context, accountID, id, text, html string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET text_body=?,html_body=?,redacted=1 WHERE id=? AND account_id=?`, text, html, id, accountID)
	return err
}

// TerminalUnredactedWorkflows returns the id, account, and raw path of every
// terminal workflow job whose stored bodies still carry token markers.
func (s *Store) TerminalUnredactedWorkflows(ctx context.Context) ([]Workflow, error) {
	rows, err := s.read.QueryContext(ctx, workflowSelect+` WHERE terminal_at IS NOT NULL AND terminal_at<>'' AND redacted=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workflow{}
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SweepWorkflows deletes workflow jobs whose terminal state is older than the
// retention cutoff and returns the raw file paths that the caller must remove.
func (s *Store) SweepWorkflows(ctx context.Context, cutoff time.Time) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,raw_path FROM outbound_workflow WHERE terminal_at IS NOT NULL AND terminal_at<>'' AND terminal_at<=?`, timeText(cutoff))
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
		if _, err = tx.ExecContext(ctx, `DELETE FROM outbound_workflow WHERE id=?`, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}

// RequeuePendingWorkflowForDomain resets every pending workflow job for a domain
// so it retries immediately after the domain's sending configuration is saved.
// A job with a send in flight (a live claim or a durable pre-send marker) is
// never touched: the active send finishes under the configuration it started
// with, and the new configuration applies to a later attempt.
func (s *Store) RequeuePendingWorkflowForDomain(ctx context.Context, accountID, domainID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE account_id=? AND status='pending' AND send_attempt_state='' AND claim_owner='' AND inbox_id IN (SELECT id FROM inboxes WHERE domain_id=? AND account_id=?)`, accountID, domainID, accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func getWorkflowTx(ctx context.Context, tx *sql.Tx, accountID, id string) (Workflow, error) {
	w, err := scanWorkflow(tx.QueryRowContext(ctx, workflowSelect+` WHERE account_id=? AND id=?`, accountID, id))
	if err == sql.ErrNoRows {
		return w, ErrNotFound
	}
	return w, err
}

// insertWorkflowAttemptTx appends a delivery-log row attributed to a workflow
// job rather than a message, so per-domain activity is complete without a
// messages row.
//
// domain_id is read as a nullable value: a standalone inbox has no managed
// domain, so its domain_id is NULL. Scanning that into a plain string fails
// with "converting NULL to string is unsupported", which would otherwise fail
// every workflow delivery for a standalone inbox (an approval request or a
// RemoteDraft handoff notification) before the provider call is even made.
// A missing inbox row is still treated as an unattributed attempt.
func (s *Store) insertWorkflowAttemptTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, provider, workflowID, status, providerMessageID, errorText string) error {
	var domainID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT domain_id FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&domainID); err != nil {
		if err == sql.ErrNoRows {
			domainID = sql.NullString{}
		} else {
			return err
		}
	}
	attempt := 1
	if err := tx.QueryRowContext(ctx, `SELECT attempts FROM outbound_workflow WHERE id=? AND account_id=?`, workflowID, accountID).Scan(&attempt); err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,domain_id,provider,message_id,workflow_id,attempt,status,provider_message_id,error_text,created_at) VALUES(?,?,?,NULL,?,?,?,?,?,?)`,
		accountID, nullString(domainID.String), provider, workflowID, attempt, status, providerMessageID, errorText, nowText()); err != nil {
		return err
	}
	return s.pruneDeliveryLogTx(ctx, tx, accountID)
}

// tokenExpiryText returns the absolute expiry text for a token that starts its
// clock now. A non-positive duration means the token never expires (NULL).
func tokenExpiryText(now string, d time.Duration) any {
	if d <= 0 {
		return nil
	}
	t := parseTime(now).Add(d)
	return timeText(t)
}
