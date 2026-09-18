package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
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

const workflowSelect = `SELECT id,account_id,inbox_id,request_id,kind,provider,from_name,from_address,to_json,cc_json,bcc_json,subject,text_body,html_body,raw_path,size_bytes,status,attempts,last_error,next_attempt_at,claim_owner,claim_expires_at,provider_message_id,redacted,created_at,sent_at,terminal_at FROM outbound_workflow`

func scanWorkflow(row interface{ Scan(...any) error }) (Workflow, error) {
	var w Workflow
	var to, cc, bcc, next, claimExp, created string
	var sent, terminal sql.NullString
	var redacted int
	err := row.Scan(&w.ID, &w.AccountID, &w.InboxID, &w.RequestID, &w.Kind, &w.Provider, &w.From.Name, &w.From.Address, &to, &cc, &bcc, &w.Subject, &w.Text, &w.HTML, &w.RawPath, &w.SizeBytes, &w.Status, &w.Attempts, &w.LastError, &next, &w.ClaimOwner, &claimExp, &w.ProviderMessageID, &redacted, &created, &sent, &terminal)
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

// DomainSendingConfigForWorkflow resolves the sending configuration for the
// domain of a workflow job's inbox. A missing job or foreign account is
// ErrNotFound; a job whose domain has no config is ErrNoProvider.
func (s *Store) DomainSendingConfigForWorkflow(ctx context.Context, accountID, workflowID string) (DomainSendingConfig, error) {
	var domainID string
	err := s.read.QueryRowContext(ctx, `SELECT i.domain_id FROM outbound_workflow w JOIN inboxes i ON i.id=w.inbox_id WHERE w.id=? AND w.account_id=?`, workflowID, accountID).Scan(&domainID)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	return s.GetDomainSendingConfig(ctx, accountID, domainID)
}

// ClaimNextWorkflow atomically claims the next due pending workflow job. It
// returns the job id, or "" if none is due.
func (s *Store) ClaimNextWorkflow(ctx context.Context, now time.Time, owner string, lease time.Duration) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM outbound_workflow WHERE status='pending' AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY created_at ASC LIMIT 1`, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner=?,claim_expires_at=? WHERE id=?`, owner, timeText(now.Add(lease)), id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ReleaseWorkflowClaim clears a claim held by owner so the job can be retried
// immediately instead of waiting for the lease to expire.
func (s *Store) ReleaseWorkflowClaim(ctx context.Context, id, owner string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner='',claim_expires_at='' WHERE id=? AND claim_owner=? AND status='pending'`, id, owner)
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

// RecoverAbandonedWorkflowClaims clears claims left by a previous process.
func (s *Store) RecoverAbandonedWorkflowClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET claim_owner='',claim_expires_at='' WHERE status='pending' AND claim_owner!=''`)
	return err
}

// HoldWorkflow defers a pending workflow job without counting a retry. It is
// used when a domain has no outbound provider yet.
func (s *Store) HoldWorkflow(ctx context.Context, accountID, id, reason string, next time.Time) error {
	res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending'`, reason, timeText(next), id, accountID)
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
func (s *Store) MarkWorkflowSent(ctx context.Context, accountID, id, providerMessageID, provider string, tokenExpiry time.Duration) ([]model.Event, error) {
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
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='sent',provider=?,provider_message_id=?,sent_at=?,terminal_at=?,attempts=attempts+1,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE id=? AND account_id=?`, provider, providerMessageID, now, now, id, accountID); err != nil {
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
func (s *Store) MarkWorkflowFailed(ctx context.Context, accountID, id, errText string, nextAttemptAt time.Time, maxAttempts int, provider string) ([]model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	w, err := getWorkflowTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
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
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status=?,attempts=?,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at='',terminal_at=? WHERE id=? AND account_id=?`, status, attempts, errText, now, id, accountID); err != nil {
			return nil, err
		}
	} else {
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status=?,attempts=?,last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=?`, status, attempts, errText, next, id, accountID); err != nil {
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
func (s *Store) RequeuePendingWorkflowForDomain(ctx context.Context, accountID, domainID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE outbound_workflow SET attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE account_id=? AND status='pending' AND inbox_id IN (SELECT id FROM inboxes WHERE domain_id=? AND account_id=?)`, accountID, domainID, accountID)
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
func (s *Store) insertWorkflowAttemptTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, provider, workflowID, status, providerMessageID, errorText string) error {
	var domainID string
	if err := tx.QueryRowContext(ctx, `SELECT domain_id FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&domainID); err != nil {
		if err == sql.ErrNoRows {
			domainID = ""
		} else {
			return err
		}
	}
	attempt := 1
	if err := tx.QueryRowContext(ctx, `SELECT attempts FROM outbound_workflow WHERE id=? AND account_id=?`, workflowID, accountID).Scan(&attempt); err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_delivery_log(account_id,domain_id,provider,message_id,workflow_id,attempt,status,provider_message_id,error_text,created_at) VALUES(?,?,?,NULL,?,?,?,?,?,?)`,
		accountID, nullString(domainID), provider, workflowID, attempt, status, providerMessageID, errorText, nowText()); err != nil {
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
