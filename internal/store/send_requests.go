package store

import (
	"context"
	"database/sql"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

const sendRequestSelect = `SELECT id,draft_id,inbox_id,status,delivery_status,content_hash,requested_at,requested_by,requested_by_api_key_id,requested_by_user_id,decided_at,decision_actor,decision_actor_id,decision_method,feedback,message_id,created_at,updated_at FROM draft_send_requests`

func scanSendRequest(row interface{ Scan(...any) error }) (model.DraftSendRequest, error) {
	var r model.DraftSendRequest
	var requested, created, updated string
	var decided sql.NullString
	err := row.Scan(&r.ID, &r.DraftID, &r.InboxID, &r.Status, &r.DeliveryStatus, &r.ContentHash, &requested, &r.RequestedBy, &r.RequestedByAPIKeyID, &r.RequestedByUserID, &decided, &r.DecisionActor, &r.DecisionActorID, &r.DecisionMethod, &r.Feedback, &r.MessageID, &created, &updated)
	if err != nil {
		return r, err
	}
	r.RequestedAt = parseTime(requested)
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	r.DecidedAt = nullableTime(decided)
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
		err := q.QueryRowContext(ctx, `SELECT name FROM api_keys WHERE id=? AND account_id=?`, p.APIKeyID, p.AccountID).Scan(&name)
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

// CreateSendRequest records an assistant's request that a draft be authorized
// and sent, freezing the draft while the request is outstanding. The caller
// must have validated that the draft is sendable (recipient and body present).
func (s *Store) CreateSendRequest(ctx context.Context, p model.Principal, draftID, contentHash string) (model.DraftSendRequest, model.Event, error) {
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
	if d.Status == model.DraftStatusPendingApproval {
		return model.DraftSendRequest{}, model.Event{}, ErrConflict
	}
	actor, err := actorIdentityTx(ctx, tx, p)
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	now := nowText()
	id := idgen.New("dsr")
	r := model.DraftSendRequest{
		ID:                  id,
		DraftID:             draftID,
		InboxID:             d.InboxID,
		Status:              model.SendRequestPending,
		DeliveryStatus:      model.SendDeliveryNone,
		ContentHash:         contentHash,
		RequestedAt:         parseTime(now),
		RequestedBy:         actor.Label,
		RequestedByAPIKeyID: actor.APIKeyID,
		RequestedByUserID:   actor.UserID,
		CreatedAt:           parseTime(now),
		UpdatedAt:           parseTime(now),
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO draft_send_requests(id,account_id,inbox_id,draft_id,status,delivery_status,content_hash,requested_at,requested_by,requested_by_api_key_id,requested_by_user_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, p.AccountID, r.InboxID, r.DraftID, r.Status, r.DeliveryStatus, r.ContentHash, now, r.RequestedBy, r.RequestedByAPIKeyID, r.RequestedByUserID, now, now); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusPendingApproval, now, draftID, p.AccountID); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, r.InboxID, model.EventDraftSendRequested, r.ID, sendRequestPayload(r))
	if err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.DraftSendRequest{}, model.Event{}, err
	}
	return r, ev, nil
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
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q += ` ORDER BY requested_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DraftSendRequest
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
	res, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,delivery_status=?,decided_at=?,decision_actor=?,decision_actor_id=?,decision_method=?,feedback=?,message_id=?,updated_at=? WHERE id=? AND account_id=? AND draft_id=? AND status=?`, model.SendRequestApproved, model.SendDeliveryPending, now, actorLabel, actorID, method, feedback, messageID, now, requestID, accountID, draftID, model.SendRequestPending)
	if err != nil {
		return model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Event{}, ErrConflict
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
