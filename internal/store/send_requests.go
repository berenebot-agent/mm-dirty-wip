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

const sendRequestSelect = `SELECT id,draft_id,inbox_id,status,delivery_status,content_hash,requested_at,requested_by,requested_by_api_key_id,requested_by_user_id,approver_email,token_hash,token_expires_at,approval_message_id,approval_workflow_id,notification_status,decided_at,decision_actor,decision_actor_id,decision_method,feedback,message_id,created_at,updated_at FROM draft_send_requests`

func scanSendRequest(row interface{ Scan(...any) error }) (model.DraftSendRequest, error) {
	var r model.DraftSendRequest
	var requested, created, updated string
	var expires, decided sql.NullString
	err := row.Scan(&r.ID, &r.DraftID, &r.InboxID, &r.Status, &r.DeliveryStatus, &r.ContentHash, &requested, &r.RequestedBy, &r.RequestedByAPIKeyID, &r.RequestedByUserID, &r.ApproverEmail, &r.TokenHash, &expires, &r.ApprovalMessageID, &r.ApprovalWorkflowID, &r.NotificationStatus, &decided, &r.DecisionActor, &r.DecisionActorID, &r.DecisionMethod, &r.Feedback, &r.MessageID, &created, &updated)
	if err != nil {
		return r, err
	}
	r.RequestedAt = parseTime(requested)
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	r.DecidedAt = nullableTime(decided)
	r.TokenExpiresAt = nullableTime(expires)
	return r, nil
}

// ActorIdentity is the denormalized audit label recorded for the principal that
// requested or decided a send request. Labels are stored on the request so
// history survives a key rename or deletion.
type ActorIdentity struct {
	Label    string
	APIKeyID string
	UserID   string
}

// rowQuerier is satisfied by both *sql.Tx and *sql.DB, letting actor resolution
// run inside a write transaction or directly against the read pool.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// actorIdentity resolves the display label for a principal. It never fails on a
// missing row: the stable id is used as a fallback label so an audit record is
// always produced.
func actorIdentity(ctx context.Context, q rowQuerier, p model.Principal) (ActorIdentity, error) {
	if p.APIKeyID != "" {
		var name string
		err := q.QueryRowContext(ctx, `SELECT name FROM clients WHERE id=? AND account_id=? AND type='api_key'`, p.APIKeyID, p.AccountID).Scan(&name)
		if err != nil && err != sql.ErrNoRows {
			return ActorIdentity{}, err
		}
		if name == "" {
			name = p.APIKeyID
		}
		return ActorIdentity{Label: name, APIKeyID: p.APIKeyID}, nil
	}
	if p.UserID != "" {
		var email string
		err := q.QueryRowContext(ctx, `SELECT email FROM users WHERE id=? AND account_id=?`, p.UserID, p.AccountID).Scan(&email)
		if err != nil && err != sql.ErrNoRows {
			return ActorIdentity{}, err
		}
		if email == "" {
			email = p.UserID
		}
		return ActorIdentity{Label: email, UserID: p.UserID}, nil
	}
	return ActorIdentity{Label: "system"}, nil
}

// ActorIdentity resolves the audit label for a principal.
func (s *Store) ActorIdentity(ctx context.Context, p model.Principal) (ActorIdentity, error) {
	return actorIdentity(ctx, s.read, p)
}

func actorIdentityTx(ctx context.Context, tx *sql.Tx, p model.Principal) (ActorIdentity, error) {
	return actorIdentity(ctx, tx, p)
}

// ID returns the stable principal identifier for a decision actor, preferring
// the API key id when present.
func (a ActorIdentity) ID() string {
	if a.APIKeyID != "" {
		return a.APIKeyID
	}
	return a.UserID
}

func sendRequestPayload(r model.DraftSendRequest) map[string]any {
	p := map[string]any{
		"request_id":      r.ID,
		"draft_id":        r.DraftID,
		"inbox_id":        r.InboxID,
		"status":          r.Status,
		"delivery_status": r.DeliveryStatus,
	}
	if r.DecisionActor != "" {
		p["decision_actor"] = r.DecisionActor
	}
	if r.DecisionMethod != "" {
		p["decision_method"] = r.DecisionMethod
	}
	if r.Feedback != "" {
		p["feedback"] = r.Feedback
	}
	if r.MessageID != "" {
		p["message_id"] = r.MessageID
	}
	if r.ApproverEmail != "" {
		p["approver_email"] = r.ApproverEmail
	}
	if r.NotificationStatus != "" && r.NotificationStatus != model.NotificationNone {
		p["notification_status"] = r.NotificationStatus
	}
	if r.TokenExpiresAt != nil {
		p["token_expires_at"] = r.TokenExpiresAt.UTC().Format(time.RFC3339)
	}
	return p
}

// getSendRequestByDraftTx returns the most recent send request for a draft
// inside the caller's transaction, or ErrNotFound.
func getSendRequestByDraftTx(ctx context.Context, tx *sql.Tx, accountID, draftID string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(tx.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND draft_id=? ORDER BY requested_at DESC LIMIT 1`, accountID, draftID))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// latestSendRequestForDraft returns the most recent send request for a draft,
// or nil when none exists.
func (s *Store) latestSendRequestForDraft(ctx context.Context, accountID, draftID string) (*model.DraftSendRequest, error) {
	r, err := scanSendRequest(s.read.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND draft_id=? ORDER BY requested_at DESC LIMIT 1`, accountID, draftID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// attachLatestSendRequests populates SendRequest on each draft from its most
// recent request. Callers must already have filtered the drafts by permission.
func (s *Store) attachLatestSendRequests(ctx context.Context, accountID string, drafts []model.Draft) {
	for i := range drafts {
		r, err := s.latestSendRequestForDraft(ctx, accountID, drafts[i].ID)
		if err != nil || r == nil {
			continue
		}
		drafts[i].SendRequest = r
	}
}

// SendRequestInsert is the durable content of a new draft send request. It is
// shared by the synchronous (no email) path and the external-approval path
// that queues the approval email in the same transaction.
type SendRequestInsert struct {
	ID                  string
	DraftID             string
	InboxID             string
	ContentHash         string
	RequestedAt         time.Time
	RequestedBy         string
	RequestedByAPIKeyID string
	RequestedByUserID   string
	ApproverEmail       string
	TokenHash           string
	TokenExpiresAt      *time.Time
}

// createSendRequestTx inserts a send request and freezes its draft inside the
// caller's transaction. It does not verify that no pending request exists; the
// caller must have expired any stale request first.
func createSendRequestTx(ctx context.Context, tx *sql.Tx, accountID string, ins SendRequestInsert) (model.DraftSendRequest, error) {
	now := nowText()
	r := model.DraftSendRequest{
		ID:                  ins.ID,
		DraftID:             ins.DraftID,
		InboxID:             ins.InboxID,
		Status:              model.SendRequestPending,
		DeliveryStatus:      model.SendDeliveryNone,
		ContentHash:         ins.ContentHash,
		RequestedAt:         ins.RequestedAt,
		RequestedBy:         ins.RequestedBy,
		RequestedByAPIKeyID: ins.RequestedByAPIKeyID,
		RequestedByUserID:   ins.RequestedByUserID,
		ApproverEmail:       ins.ApproverEmail,
		TokenHash:           ins.TokenHash,
		TokenExpiresAt:      ins.TokenExpiresAt,
		CreatedAt:           ins.RequestedAt,
		UpdatedAt:           ins.RequestedAt,
	}
	var expires any
	if ins.TokenExpiresAt != nil {
		expires = timeText(*ins.TokenExpiresAt)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_send_requests(id,account_id,inbox_id,draft_id,status,delivery_status,content_hash,requested_at,requested_by,requested_by_api_key_id,requested_by_user_id,approver_email,token_hash,token_expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, accountID, r.InboxID, r.DraftID, r.Status, r.DeliveryStatus, r.ContentHash, now, r.RequestedBy, r.RequestedByAPIKeyID, r.RequestedByUserID, r.ApproverEmail, r.TokenHash, expires, now, now); err != nil {
		return model.DraftSendRequest{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusPendingApproval, now, r.DraftID, accountID); err != nil {
		return model.DraftSendRequest{}, err
	}
	return r, nil
}

// expireStaleForDraftTx expires any pending request for a draft whose token has
// passed, unfreezing the draft. It returns the events for the expiries so the
// caller can publish them.
func expireStaleForDraftTx(ctx context.Context, tx *sql.Tx, accountID, draftID string, now time.Time) ([]model.Event, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,inbox_id,draft_id FROM draft_send_requests WHERE account_id=? AND draft_id=? AND status=? AND token_expires_at IS NOT NULL AND token_expires_at<=?`, accountID, draftID, model.SendRequestPending, timeText(now))
	if err != nil {
		return nil, err
	}
	type stale struct{ id, inboxID, draftID string }
	var list []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.inboxID, &s.draftID); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var events []model.Event
	for _, s := range list {
		nowT := nowText()
		res, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.SendRequestExpired, nowT, s.id, accountID, model.SendRequestPending)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.DraftStatusDraft, nowT, s.draftID, accountID, model.DraftStatusPendingApproval); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='approval request expired',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, nowT, accountID, s.id); err != nil {
			return nil, err
		}
		r, err := getSendRequestByIDTx(ctx, tx, accountID, s.id)
		if err != nil {
			return nil, err
		}
		ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, model.EventDraftApprovalExpired, r.ID, sendRequestPayload(r))
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// pendingRequestExistsTx reports whether a draft still has an unexpired pending
// request inside the caller's transaction.
func pendingRequestExistsTx(ctx context.Context, tx *sql.Tx, accountID, draftID string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM draft_send_requests WHERE account_id=? AND draft_id=? AND status=?`, accountID, draftID, model.SendRequestPending).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ExpireApprovalRequests expires every external approval request whose token has
// passed, unfreezing the draft, and returns the events to publish. It is called
// by the background sweep.
func (s *Store) ExpireApprovalRequests(ctx context.Context, now time.Time) ([]model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,inbox_id,draft_id FROM draft_send_requests WHERE status=? AND token_expires_at IS NOT NULL AND token_expires_at<=?`, model.SendRequestPending, timeText(now))
	if err != nil {
		return nil, err
	}
	type stale struct{ id, accountID, inboxID, draftID string }
	var list []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.accountID, &st.inboxID, &st.draftID); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var events []model.Event
	for _, st := range list {
		nowT := nowText()
		res, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.SendRequestExpired, nowT, st.id, st.accountID, model.SendRequestPending)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.DraftStatusDraft, nowT, st.draftID, st.accountID, model.DraftStatusPendingApproval); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='approval request expired',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, nowT, st.accountID, st.id); err != nil {
			return nil, err
		}
		r, err := getSendRequestByIDTx(ctx, tx, st.accountID, st.id)
		if err != nil {
			return nil, err
		}
		ev, err := insertEventTx(ctx, tx, st.accountID, r.InboxID, model.EventDraftApprovalExpired, r.ID, sendRequestPayload(r))
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// PendingRequestExists reports whether a draft has an unexpired pending send
// request.
func (s *Store) PendingRequestExists(ctx context.Context, accountID, draftID string) (bool, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM draft_send_requests WHERE account_id=? AND draft_id=? AND status=?`, accountID, draftID, model.SendRequestPending).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ExpireStaleRequestForDraft expires a pending request for a draft whose token
// has passed and returns the events to publish.
func (s *Store) ExpireStaleRequestForDraft(ctx context.Context, accountID, draftID string) ([]model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	events, err := expireStaleForDraftTx(ctx, tx, accountID, draftID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// CreateSendRequest records an assistant's request that a draft be authorized
// and sent, freezing the draft while the request is outstanding. The caller
// must have validated that the draft is sendable (recipient and body present).
// It returns the requested event plus any expiry event produced by clearing a
// stale pending request for the same draft.
func (s *Store) CreateSendRequest(ctx context.Context, p model.Principal, draftID, contentHash string) (model.DraftSendRequest, []model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.DraftSendRequest{}, nil, ErrForbidden
	}
	expired, err := expireStaleForDraftTx(ctx, tx, p.AccountID, draftID, time.Now().UTC())
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	pending, err := pendingRequestExistsTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	if pending {
		return model.DraftSendRequest{}, nil, ErrConflict
	}
	actor, err := actorIdentityTx(ctx, tx, p)
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	now := time.Now().UTC()
	r, err := createSendRequestTx(ctx, tx, p.AccountID, SendRequestInsert{
		ID:                  idgen.New("dsr"),
		DraftID:             draftID,
		InboxID:             d.InboxID,
		ContentHash:         contentHash,
		RequestedAt:         now,
		RequestedBy:         actor.Label,
		RequestedByAPIKeyID: actor.APIKeyID,
		RequestedByUserID:   actor.UserID,
	})
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, r.InboxID, model.EventDraftSendRequested, r.ID, sendRequestPayload(r))
	if err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, nil, err
	}
	return r, append(expired, ev), nil
}

// CancelSendRequest withdraws an outstanding request and returns the draft to
// its ordinary editable state.
func (s *Store) CancelSendRequest(ctx context.Context, p model.Principal, draftID string) (model.DraftSendRequest, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.DraftSendRequest{}, model.Event{}, ErrForbidden
	}
	r, err := getSendRequestByDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if r.Status != model.SendRequestPending {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	now := nowText()
	res, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.SendRequestCancelled, now, r.ID, p.AccountID, model.SendRequestPending)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusDraft, now, draftID, p.AccountID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='request cancelled',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, now, p.AccountID, r.ID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	r.Status = model.SendRequestCancelled
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, p.AccountID, r.InboxID, model.EventDraftSendRequestCancelled, r.ID, sendRequestPayload(r))
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	return r, ev, nil
}

// RejectSendRequest records an owner's rejection of an outstanding request. The
// draft is kept and marked rejected so it can be revised and resubmitted.
func (s *Store) RejectSendRequest(ctx context.Context, p model.Principal, draftID, feedback, method string) (model.DraftSendRequest, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if !p.CanOwn(d.InboxID) {
		return model.DraftSendRequest{}, model.Event{}, ErrForbidden
	}
	r, err := getSendRequestByDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if r.Status != model.SendRequestPending {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	actor, err := actorIdentityTx(ctx, tx, p)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	now := nowText()
	res, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,decided_at=?,decision_actor=?,decision_actor_id=?,decision_method=?,feedback=?,updated_at=? WHERE id=? AND account_id=? AND status=?`, model.SendRequestRejected, now, actor.Label, actor.ID(), method, feedback, now, r.ID, p.AccountID, model.SendRequestPending)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusRejected, now, draftID, p.AccountID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='request rejected',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, now, p.AccountID, r.ID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	r.Status = model.SendRequestRejected
	r.DecidedAt = timePtr(parseTime(now))
	r.DecisionActor = actor.Label
	r.DecisionActorID = actor.ID()
	r.DecisionMethod = method
	r.Feedback = feedback
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, p.AccountID, r.InboxID, model.EventDraftRejected, r.ID, sendRequestPayload(r))
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	return r, ev, nil
}

// GetSendRequestByDraft returns the most recent send request for a draft. It
// works even after the draft has been consumed by an approved send.
func (s *Store) GetSendRequestByDraft(ctx context.Context, p model.Principal, draftID string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(s.read.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND draft_id=? ORDER BY requested_at DESC LIMIT 1`, p.AccountID, draftID))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if !p.CanAssist(r.InboxID) {
		return model.DraftSendRequest{}, ErrForbidden
	}
	return r, nil
}

// GetSendRequest returns a single send request by id.
func (s *Store) GetSendRequest(ctx context.Context, p model.Principal, id string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(s.read.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND id=?`, p.AccountID, id))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if !p.CanAssist(r.InboxID) {
		return model.DraftSendRequest{}, ErrForbidden
	}
	return r, nil
}

// ListSendRequests lists send requests for an inbox, or across every inbox the
// principal can assist when inboxID is empty.
func (s *Store) ListSendRequests(ctx context.Context, p model.Principal, inboxID string, activeOnly bool, limit int) ([]model.DraftSendRequest, error) {
	if inboxID != "" && !p.CanAssist(inboxID) {
		return nil, ErrForbidden
	}
	q := sendRequestSelect + ` WHERE account_id=?`
	args := []any{p.AccountID}
	if inboxID != "" {
		q += ` AND inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := assistInboxIDs(p)
		if len(ids) == 0 {
			return []model.DraftSendRequest{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if activeOnly {
		q += ` AND status=?`
		args = append(args, model.SendRequestPending)
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q += ` ORDER BY requested_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DraftSendRequest{}
	for rows.Next() {
		r, err := scanSendRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DraftCountsByInbox returns the number of unsent drafts per inbox. Every draft
// is unsent by definition (sending consumes it), so this covers draft,
// pending_approval and rejected states.
func (s *Store) DraftCountsByInbox(ctx context.Context, p model.Principal) (map[string]int, error) {
	q := `SELECT inbox_id,COUNT(*) FROM drafts WHERE account_id=?`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := assistInboxIDs(p)
		if len(ids) == 0 {
			return map[string]int{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` GROUP BY inbox_id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var inboxID string
		var n int
		if err := rows.Scan(&inboxID, &n); err != nil {
			return nil, err
		}
		out[inboxID] = n
	}
	return out, rows.Err()
}

// PendingDraftCountsByInbox returns the number of drafts awaiting approval to
// send per inbox. A draft is pending exactly while its status is
// pending_approval, which mirrors an outstanding send request.
func (s *Store) PendingDraftCountsByInbox(ctx context.Context, p model.Principal) (map[string]int, error) {
	q := `SELECT inbox_id,COUNT(*) FROM drafts WHERE account_id=? AND status=?`
	args := []any{p.AccountID, model.DraftStatusPendingApproval}
	if !p.Admin {
		ids := assistInboxIDs(p)
		if len(ids) == 0 {
			return map[string]int{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` GROUP BY inbox_id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var inboxID string
		var n int
		if err := rows.Scan(&inboxID, &n); err != nil {
			return nil, err
		}
		out[inboxID] = n
	}
	return out, rows.Err()
}

func assistInboxIDs(p model.Principal) []string {
	ids := make([]string, 0, len(p.MailboxRoles))
	for id, role := range p.MailboxRoles {
		if role == "assistant" || role == "owner" {
			ids = append(ids, id)
		}
	}
	return ids
}

// getSendRequestByIDTx reads a single request inside a transaction.
func getSendRequestByIDTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(tx.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND id=?`, accountID, id))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// approveSendRequestTx atomically claims an outstanding request for an approved
// send and links it to the queued message. It returns ErrConflict when the
// request has already been decided, which is what makes a decision single-use
// and prevents a duplicate send.
func approveSendRequestTx(ctx context.Context, tx *sql.Tx, accountID, draftID, requestID, actorLabel, actorID, method, feedback, messageID string) (model.Event, error) {
	now := nowText()
	q := `UPDATE draft_send_requests SET status=?,delivery_status=?,decided_at=?,decision_actor=?,decision_actor_id=?,decision_method=?,feedback=?,message_id=?,updated_at=? WHERE id=? AND account_id=? AND draft_id=? AND status=?`
	args := []any{model.SendRequestApproved, model.SendDeliveryPending, now, actorLabel, actorID, method, feedback, messageID, now, requestID, accountID, draftID, model.SendRequestPending}
	// An email approval must not claim a request whose token has expired, even
	// if the background sweep has not run yet.
	if method == model.DecisionMethodEmail {
		q += ` AND (token_expires_at IS NULL OR token_expires_at>?)`
		args = append(args, now)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Event{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='request decided',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, now, accountID, requestID); err != nil {
		return model.Event{}, err
	}
	r, err := getSendRequestByIDTx(ctx, tx, accountID, requestID)
	if err != nil {
		return model.Event{}, err
	}
	return insertEventTx(ctx, tx, accountID, r.InboxID, model.EventDraftApproved, r.ID, sendRequestPayload(r))
}

// sendRequestDeliveryTx records the delivery outcome on a request linked to the
// given message, if any, and returns the corresponding draft event. It returns
// (nil, nil) when the message was not produced by a watched send request.
func sendRequestDeliveryTx(ctx context.Context, tx *sql.Tx, accountID, messageID, delivery, eventType string) (*model.Event, error) {
	r, err := scanSendRequest(tx.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND message_id=? ORDER BY requested_at DESC LIMIT 1`, accountID, messageID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET delivery_status=?,updated_at=? WHERE id=? AND account_id=?`, delivery, now, r.ID, accountID); err != nil {
		return nil, err
	}
	r.DeliveryStatus = delivery
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, eventType, r.ID, sendRequestPayload(r))
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// GetSendRequestInternal loads a send request by id without a principal. It is
// used by the external email approval path.
func (s *Store) GetSendRequestInternal(ctx context.Context, accountID, id string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(s.read.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND id=?`, accountID, id))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// FindPendingSendRequestByToken resolves a pending request from a hashed token
// scoped to its inbox. It returns ErrNotFound when no live request matches.
func (s *Store) FindPendingSendRequestByToken(ctx context.Context, accountID, inboxID, tokenHash string) (model.DraftSendRequest, error) {
	r, err := scanSendRequest(s.read.QueryRowContext(ctx, sendRequestSelect+` WHERE account_id=? AND inbox_id=? AND token_hash=? AND status=? COLLATE BINARY`, accountID, inboxID, tokenHash, model.SendRequestPending))
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// RejectSendRequestInternal records a rejection without a principal. It is used
// by the external email approval path after the decision has been validated.
func (s *Store) RejectSendRequestInternal(ctx context.Context, accountID, requestID, actorLabel, actorID, method, feedback string) (model.DraftSendRequest, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	defer tx.Rollback()
	r, err := getSendRequestByIDTx(ctx, tx, accountID, requestID)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	now := nowText()
	q := `UPDATE draft_send_requests SET status=?,decided_at=?,decision_actor=?,decision_actor_id=?,decision_method=?,feedback=?,updated_at=? WHERE id=? AND account_id=? AND status=?`
	args := []any{model.SendRequestRejected, now, actorLabel, actorID, method, feedback, now, r.ID, accountID, model.SendRequestPending}
	if method == model.DecisionMethodEmail {
		q += ` AND (token_expires_at IS NULL OR token_expires_at>?)`
		args = append(args, now)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusRejected, now, r.DraftID, accountID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',last_error='request rejected',terminal_at=?,claim_owner='',claim_expires_at='',next_attempt_at='' WHERE account_id=? AND request_id=? AND status='pending'`, now, accountID, r.ID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	r.Status = model.SendRequestRejected
	r.DecidedAt = timePtr(parseTime(now))
	r.DecisionActor = actorLabel
	r.DecisionActorID = actorID
	r.DecisionMethod = method
	r.Feedback = feedback
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, model.EventDraftRejected, r.ID, sendRequestPayload(r))
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	return r, ev, nil
}

// ControlMessageRecord is one consumed inbound approval control message. It is
// the durable evidence for the per-domain receiving log and the webhook dedup
// key.
type ControlMessageRecord struct {
	ID                 string
	AccountID          string
	InboxID            string
	Provider           string
	ProviderDeliveryID string
	EnvelopeRecipient  string
	FromName           string
	FromAddress        string
	RequestID          string
	Action             string
	Outcome            string
	Reason             string
	// Subject is the reviewed draft subject snapshotted at decision time. It is
	// never the raw inbound subject, which carries the approval token.
	Subject string
}

// ControlMessage is a recorded control message for display.
type ControlMessage struct {
	ID                string
	InboxID           string
	Provider          string
	FromName          string
	FromAddress       string
	EnvelopeRecipient string
	RequestID         string
	Action            string
	Outcome           string
	Reason            string
	Subject           string
	CreatedAt         time.Time
}

// ApprovalSubjectLabel is the log label for a consumed approval control message:
// the reviewed draft subject, prefixed "Rejected" when the decision was a
// rejection and "Approval" otherwise (approved, invalid or error), or the bare
// prefix when the request could not be resolved. The raw inbound subject (which
// carries the token) is never used.
func ApprovalSubjectLabel(outcome, subject string) string {
	prefix := "Approval"
	if outcome == "rejected" {
		prefix = "Rejected"
	}
	if s := strings.TrimSpace(subject); s != "" {
		return prefix + ": " + s
	}
	return prefix
}

// controlMessageSelect lists the displayable columns shared by the per-inbox
// and account-wide control-message reads.
const controlMessageSelect = `SELECT id,inbox_id,provider,from_name,from_address,envelope_recipient,request_id,action,outcome,reason,subject,created_at FROM inbound_control_messages`

func scanControlMessages(rows *sql.Rows) ([]ControlMessage, error) {
	out := []ControlMessage{}
	for rows.Next() {
		var m ControlMessage
		var created string
		if err := rows.Scan(&m.ID, &m.InboxID, &m.Provider, &m.FromName, &m.FromAddress, &m.EnvelopeRecipient, &m.RequestID, &m.Action, &m.Outcome, &m.Reason, &m.Subject, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordControlMessage persists a consumed approval control message. It returns
// false when an identical provider delivery has already been recorded, so a
// webhook retry is a harmless no-op.
func (s *Store) RecordControlMessage(ctx context.Context, r ControlMessageRecord) (bool, error) {
	now := nowText()
	res, err := s.write.ExecContext(ctx, `INSERT OR IGNORE INTO inbound_control_messages(id,account_id,inbox_id,provider,provider_delivery_id,envelope_recipient,from_name,from_address,request_id,action,outcome,reason,subject,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, idgen.New("icm"), r.AccountID, r.InboxID, r.Provider, nullString(r.ProviderDeliveryID), r.EnvelopeRecipient, r.FromName, r.FromAddress, r.RequestID, r.Action, r.Outcome, r.Reason, r.Subject, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListControlMessages returns an inbox's consumed control messages, newest
// first, for the per-domain activity log.
func (s *Store) ListControlMessages(ctx context.Context, accountID, inboxID string, limit int) ([]ControlMessage, error) {
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	rows, err := s.read.QueryContext(ctx, controlMessageSelect+` WHERE account_id=? AND inbox_id=? ORDER BY created_at DESC LIMIT ?`, accountID, inboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanControlMessages(rows)
}

// ListAccountControlMessages returns an account's consumed control messages,
// newest first, for the admin dashboard's Recent messages list.
func (s *Store) ListAccountControlMessages(ctx context.Context, accountID string, limit int) ([]ControlMessage, error) {
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	rows, err := s.read.QueryContext(ctx, controlMessageSelect+` WHERE account_id=? ORDER BY created_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanControlMessages(rows)
}
