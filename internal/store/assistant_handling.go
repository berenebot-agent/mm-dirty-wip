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

// ErrHandoffUnsupported is returned when a RemoteDraft handoff is requested for
// an inbox that cannot receive one. It is a permanent refusal, not a retryable
// fault.
var ErrHandoffUnsupported = errors.New("remote draft handoff is not available for this inbox")

// inboxAuthoring reads the raw stored authoring mode and notify default of an
// inbox, plus its kind.
func (s *Store) inboxAuthoring(ctx context.Context, accountID, inboxID string) (kind, storedMode, notify string, err error) {
	err = s.read.QueryRowContext(ctx, `SELECT kind,authoring_mode,notify_default_address FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).
		Scan(&kind, &storedMode, &notify)
	if err == sql.ErrNoRows {
		return "", "", "", ErrNotFound
	}
	return kind, storedMode, notify, err
}

// InboxAuthoringSettings is the per-inbox assistant authoring configuration.
type InboxAuthoringSettings struct {
	// Mode is the effective authoring mode (see model.Authoring* constants).
	Mode string
	// NotifyAddress is the effective address notifications are sent to. When the
	// stored override is blank this is the inbox's own connected address, so a
	// notification always has a destination.
	NotifyAddress string
	// NotifyOverridden reports whether an explicit per-inbox notify address is
	// set (as opposed to the blank default that follows the connected address).
	NotifyOverridden bool
}

// GetInboxAuthoringSettings returns the effective authoring settings for an inbox
// a principal can read.
func (s *Store) GetInboxAuthoringSettings(ctx context.Context, p model.Principal, inboxID string) (InboxAuthoringSettings, error) {
	if !p.CanRead(inboxID) {
		return InboxAuthoringSettings{}, ErrForbidden
	}
	return s.inboxAuthoringSettingsInternal(ctx, p.AccountID, inboxID)
}

// GetInboxAuthoringSettingsInternal resolves the effective authoring settings
// without a principal, for the handoff worker and inbound path.
func (s *Store) GetInboxAuthoringSettingsInternal(ctx context.Context, accountID, inboxID string) (InboxAuthoringSettings, error) {
	return s.inboxAuthoringSettingsInternal(ctx, accountID, inboxID)
}

func (s *Store) inboxAuthoringSettingsInternal(ctx context.Context, accountID, inboxID string) (InboxAuthoringSettings, error) {
	kind, stored, notify, err := s.inboxAuthoring(ctx, accountID, inboxID)
	if err != nil {
		return InboxAuthoringSettings{}, err
	}
	out := InboxAuthoringSettings{Mode: model.NormalizeAuthoringMode(kind, stored)}
	if override := strings.TrimSpace(notify); override != "" {
		out.NotifyAddress = override
		out.NotifyOverridden = true
	} else {
		out.NotifyAddress = s.connectedAddress(ctx, accountID, inboxID)
	}
	return out, nil
}

// connectedAddress returns the address MailMoose sends notifications from and to
// for an inbox when no explicit notify override is set: the inbox's own primary
// address (the standalone connected address, or the domain inbox's managed
// address).
func (s *Store) connectedAddress(ctx context.Context, accountID, inboxID string) string {
	var address string
	if err := s.read.QueryRowContext(ctx, `SELECT address FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&address); err != nil {
		return ""
	}
	return strings.TrimSpace(address)
}

// SetInboxAuthoringMode sets the per-inbox authoring mode. An empty mode clears
// the override so the effective mode follows the inbox kind default. It requires
// Owner on the inbox (or account admin).
func (s *Store) SetInboxAuthoringMode(ctx context.Context, p model.Principal, inboxID, mode string) error {
	if !p.CanOwn(inboxID) {
		return ErrForbidden
	}
	mode = strings.TrimSpace(mode)
	if mode != "" && !model.ValidAuthoringMode(mode) {
		return fmt.Errorf("unknown authoring mode %q", mode)
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET authoring_mode=? WHERE id=? AND account_id=?`, mode, inboxID, p.AccountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxNotifyAddress sets the per-inbox default notification address. A blank
// value clears the override so notifications follow the connected address.
func (s *Store) SetInboxNotifyAddress(ctx context.Context, p model.Principal, inboxID, address string) error {
	if !p.CanOwn(inboxID) {
		return ErrForbidden
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET notify_default_address=? WHERE id=? AND account_id=?`, strings.TrimSpace(address), inboxID, p.AccountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRemoteSentCopy configures a standalone inbox's sent-copy behaviour: whether a
// sent message is copied into the remote Sent folder, and an optional explicit
// destination folder path (empty means resolve the Sent-role folder at copy time).
// It requires Owner on the inbox and is a no-op error for a domain inbox.
func (s *Store) SetRemoteSentCopy(ctx context.Context, p model.Principal, inboxID string, enabled bool, folder string) error {
	if !p.CanOwn(inboxID) {
		return ErrForbidden
	}
	var kind string
	if err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, p.AccountID).Scan(&kind); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if kind != model.InboxKindStandalone {
		return ErrStandaloneRequired
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET remote_sent_copy_enabled=?,remote_sent_copy_folder=? WHERE id=? AND account_id=?`, boolInt(enabled), strings.TrimSpace(folder), inboxID, p.AccountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AssistantHandlingInsert is the durable content of a new RemoteDraft handoff.
type AssistantHandlingInsert struct {
	ID          string
	DraftID     string
	InboxID     string
	Mode        string
	ContentHash string
	HandoffID   string
	MessageID   string
	// RemoteFolder is the resolved remote Drafts folder the worker will append
	// to, recorded up front so publication is deterministic.
	RemoteFolder string
	// RawPath is the data-dir-relative path of the frozen raw MIME the handoff
	// will append. It is written by the app layer before the record is created
	// and is immutable thereafter.
	RawPath string
	// SizeBytes is the frozen raw MIME size.
	SizeBytes int64
}

// handlingColumns projects every assistant_handling_requests column, in the order
// scanAssistantHandling reads them.
const handlingColumns = `id,account_id,inbox_id,draft_id,mode,content_hash,handoff_id,message_id,remote_folder,raw_path,size_bytes,publication,remote_uid,notification_status,attempts,last_error,requested_at,published_at,created_at,updated_at,notification_message_id`

func scanAssistantHandling(row interface{ Scan(...any) error }) (model.AssistantHandlingRequest, error) {
	var r model.AssistantHandlingRequest
	var accountID, rawPath, requested, created, updated string
	var published sql.NullString
	err := row.Scan(&r.ID, &accountID, &r.InboxID, &r.DraftID, &r.Mode, &r.ContentHash, &r.HandoffID, &r.MessageID, &r.RemoteFolder, &rawPath, &r.SizeBytes, &r.Publication, &r.RemoteUID, &r.NotificationStatus, &r.Attempts, &r.LastError, &requested, &published, &created, &updated, &r.NotificationMessageID)
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.RawPath = rawPath
	r.RequestedAt = parseTime(requested)
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	r.PublishedAt = nullableTime(published)
	return r, nil
}

// GetAssistantHandling returns a handoff record by id, enforcing read access.
func (s *Store) GetAssistantHandling(ctx context.Context, p model.Principal, id string) (model.AssistantHandlingRequest, error) {
	r, err := scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND id=?`, p.AccountID, id))
	if err != nil {
		return r, err
	}
	if !p.CanRead(r.InboxID) {
		return model.AssistantHandlingRequest{}, ErrForbidden
	}
	return r, nil
}

// GetAssistantHandlingInternal loads a handoff record without a principal.
func (s *Store) GetAssistantHandlingInternal(ctx context.Context, accountID, id string) (model.AssistantHandlingRequest, error) {
	return scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND id=?`, accountID, id))
}

// LatestAssistantHandlingForDraft returns the most recent handoff record for a
// draft, or nil when none exists.
func (s *Store) LatestAssistantHandlingForDraft(ctx context.Context, accountID, draftID string) (*model.AssistantHandlingRequest, error) {
	r, err := scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND draft_id=? ORDER BY requested_at DESC LIMIT 1`, accountID, draftID))
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// InboxAccountID resolves the account id that owns an inbox.
func (s *Store) InboxAccountID(ctx context.Context, inboxID string) (string, error) {
	var accountID string
	err := s.read.QueryRowContext(ctx, `SELECT account_id FROM inboxes WHERE id=?`, inboxID).Scan(&accountID)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return accountID, err
}

// CreateAssistantHandling records a RemoteDraft handoff and freezes its draft in
// one transaction. The caller must have validated that the draft is sendable and
// that the inbox accepts a handoff. It returns the requested event plus any
// expiry event produced when clearing a stale MailMooseApproval request for the
// same draft so the two request kinds never coexist.
func (s *Store) CreateAssistantHandling(ctx context.Context, p model.Principal, ins AssistantHandlingInsert) (model.AssistantHandlingRequest, []model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, ins.DraftID)
	if err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.AssistantHandlingRequest{}, nil, ErrForbidden
	}
	// A handoff freezes the same draft the approval path freezes; clearing any
	// stale approval request first keeps the two request kinds mutually exclusive.
	expired, err := expireStaleForDraftTx(ctx, tx, p.AccountID, ins.DraftID, time.Now().UTC())
	if err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	if pending, err := pendingRequestExistsTx(ctx, tx, p.AccountID, ins.DraftID); err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	} else if pending {
		return model.AssistantHandlingRequest{}, nil, ErrConflict
	}
	// A draft that already has an outstanding handoff cannot be handed off again
	// until that one settles (published, ambiguous, failed or cancelled), which is
	// what keeps a single append in flight per draft.
	var outstanding int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM assistant_handling_requests WHERE account_id=? AND draft_id=? AND publication=?`, p.AccountID, ins.DraftID, model.HandoffPending).Scan(&outstanding); err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	if outstanding != 0 {
		return model.AssistantHandlingRequest{}, nil, ErrConflict
	}
	now := nowText()
	if ins.ID == "" {
		ins.ID = idgen.New("dsh")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO assistant_handling_requests(id,account_id,inbox_id,draft_id,mode,content_hash,handoff_id,message_id,remote_folder,raw_path,size_bytes,publication,remote_uid,notification_status,attempts,last_error,requested_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ins.ID, p.AccountID, ins.InboxID, ins.DraftID, ins.Mode, ins.ContentHash, ins.HandoffID, ins.MessageID, ins.RemoteFolder, ins.RawPath, ins.SizeBytes, model.HandoffPending, 0, model.NotificationNone, 0, "", now, now, now); err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusPendingApproval, now, ins.DraftID, p.AccountID); err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	created, err := getAssistantHandlingTx(ctx, tx, p.AccountID, ins.ID)
	if err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, ins.InboxID, model.EventDraftHandoffRequested, created.ID, handoffPayload(created))
	if err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		return model.AssistantHandlingRequest{}, nil, err
	}
	return created, append(expired, ev), nil
}

// handoffPayload is the event payload for a handoff record. It never carries the
// frozen content hash or any token.
func handoffPayload(r model.AssistantHandlingRequest) map[string]any {
	p := map[string]any{
		"request_id":    r.ID,
		"draft_id":      r.DraftID,
		"inbox_id":      r.InboxID,
		"mode":          r.Mode,
		"publication":   r.Publication,
		"handoff_id":    r.HandoffID,
		"remote_uid":    r.RemoteUID,
		"remote_folder": r.RemoteFolder,
	}
	if r.MessageID != "" {
		p["message_id"] = r.MessageID
	}
	if r.NotificationStatus != "" && r.NotificationStatus != model.NotificationNone {
		p["notification_status"] = r.NotificationStatus
	}
	if r.LastError != "" {
		p["error"] = r.LastError
	}
	return p
}

func getAssistantHandlingTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.AssistantHandlingRequest, error) {
	return scanAssistantHandling(tx.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND id=?`, accountID, id))
}

// ListPendingHandoffs returns handoff records still awaiting publication, oldest
// first, for the worker to claim.
func (s *Store) ListPendingHandoffs(ctx context.Context, limit int) ([]model.AssistantHandlingRequest, error) {
	if limit <= 0 {
		limit = 128
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE publication=? ORDER BY requested_at ASC LIMIT ?`, model.HandoffPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AssistantHandlingRequest{}
	for rows.Next() {
		r, scanErr := scanAssistantHandling(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimNextHandoff atomically claims the oldest pending handoff for publication.
// It returns the record, or a zero record when none is due. Publication is not a
// lease-based queue: the claim moves the record to HandoffPending with a bumped
// attempt count and the caller must call MarkHandoffPublished / MarkHandoffFailed
// / MarkHandoffAmbiguous to settle it. A claim that is never settled is retried
// because the record stays Pending and the worker's publish path is idempotent by
// handoff id.
func (s *Store) ClaimNextHandoff(ctx context.Context, owner string) (model.AssistantHandlingRequest, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.AssistantHandlingRequest{}, false, err
	}
	defer tx.Rollback()
	var id, accountID string
	err = tx.QueryRowContext(ctx, `SELECT id,account_id FROM assistant_handling_requests WHERE publication=? ORDER BY requested_at ASC LIMIT 1`, model.HandoffPending).Scan(&id, &accountID)
	if err == sql.ErrNoRows {
		return model.AssistantHandlingRequest{}, false, nil
	}
	if err != nil {
		return model.AssistantHandlingRequest{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET attempts=attempts+1,updated_at=? WHERE id=? AND account_id=?`, nowText(), id, accountID); err != nil {
		return model.AssistantHandlingRequest{}, false, err
	}
	r, err := getAssistantHandlingTx(ctx, tx, accountID, id)
	if err != nil {
		return model.AssistantHandlingRequest{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.AssistantHandlingRequest{}, false, err
	}
	return r, true, nil
}

// MarkHandoffPublished records a confirmed append and returns the event. It is
// idempotent: a record already published returns (nil event, nil) and never
// re-emits. remoteUID is zero when the server reported no APPENDUID; the record
// is then still Published because the caller confirmed existence by lookup.
func (s *Store) MarkHandoffPublished(ctx context.Context, accountID, id string, remoteUID uint32) (*model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := getAssistantHandlingTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	if r.Publication == model.HandoffPublished {
		return nil, nil
	}
	if r.Publication != model.HandoffPending && r.Publication != model.HandoffAmbiguous {
		return nil, ErrConflict
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET publication=?,remote_uid=?,published_at=?,last_error='',updated_at=? WHERE id=? AND account_id=?`, model.HandoffPublished, remoteUID, now, now, id, accountID); err != nil {
		return nil, err
	}
	r.Publication = model.HandoffPublished
	r.RemoteUID = remoteUID
	r.PublishedAt = timePtr(parseTime(now))
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, model.EventDraftHandoffPublished, r.ID, handoffPayload(r))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

// MarkHandoffAmbiguous records that the append outcome could not be verified. It
// is an explicit terminal-for-retry state: the draft may or may not exist
// remotely, so no automatic re-append follows.
func (s *Store) MarkHandoffAmbiguous(ctx context.Context, accountID, id, reason string) (*model.Event, error) {
	return s.settleHandoff(ctx, accountID, id, model.HandoffAmbiguous, reason, model.EventDraftHandoffAmbiguous, false)
}

// MarkHandoffFailed records a terminal handoff failure.
func (s *Store) MarkHandoffFailed(ctx context.Context, accountID, id, reason string) (*model.Event, error) {
	return s.settleHandoff(ctx, accountID, id, model.HandoffFailed, reason, model.EventDraftHandoffFailed, true)
}

func (s *Store) settleHandoff(ctx context.Context, accountID, id, state, reason, eventType string, unfreeze bool) (*model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := getAssistantHandlingTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	if r.Publication != model.HandoffPending {
		// Already settled (published or otherwise): never regress a published
		// handoff, and never re-emit for a settled record.
		return nil, nil
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET publication=?,last_error=?,updated_at=? WHERE id=? AND account_id=?`, state, reason, now, id, accountID); err != nil {
		return nil, err
	}
	if unfreeze {
		if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusDraft, now, r.DraftID, accountID); err != nil {
			return nil, err
		}
	}
	r.Publication = state
	r.LastError = reason
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, eventType, r.ID, handoffPayload(r))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

// CancelHandoff withdraws an outstanding handoff that has not been published and
// unfreezes its draft. It accepts both a Pending handoff (before publication) and
// an Ambiguous one (the append outcome could not be verified): resolving an
// ambiguous handoff gives the operator an exit so its frozen draft is not trapped.
// A published handoff cannot be cancelled.
func (s *Store) CancelHandoff(ctx context.Context, p model.Principal, draftID string) (model.AssistantHandlingRequest, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.AssistantHandlingRequest{}, model.Event{}, ErrForbidden
	}
	r, err := scanAssistantHandling(tx.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND draft_id=? ORDER BY requested_at DESC LIMIT 1`, p.AccountID, draftID))
	if err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	if r.Publication != model.HandoffPending && r.Publication != model.HandoffAmbiguous {
		return model.AssistantHandlingRequest{}, model.Event{}, ErrConflict
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET publication=?,last_error='cancelled',updated_at=? WHERE id=? AND account_id=?`, model.HandoffFailed, now, r.ID, p.AccountID); err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE drafts SET status=?,updated_at=? WHERE id=? AND account_id=?`, model.DraftStatusDraft, now, draftID, p.AccountID); err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	r.Publication = model.HandoffFailed
	r.LastError = "cancelled"
	r.UpdatedAt = parseTime(now)
	ev, err := insertEventTx(ctx, tx, p.AccountID, r.InboxID, model.EventDraftHandoffCancelled, r.ID, handoffPayload(r))
	if err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.AssistantHandlingRequest{}, model.Event{}, err
	}
	return r, ev, nil
}

// MarkHandoffNotificationSent records that the handoff notification was handed to
// the outbound path. It advances independently of publication and never touches
// the remote append.
func (s *Store) MarkHandoffNotificationSent(ctx context.Context, accountID, id string) (*model.Event, error) {
	return s.settleHandoffNotification(ctx, accountID, id, model.NotificationSent, "", model.EventDraftHandoffNotificationSent)
}

// MarkHandoffNotificationFailed records a terminal notification failure.
// Publication is unaffected by this state.
func (s *Store) MarkHandoffNotificationFailed(ctx context.Context, accountID, id, reason string) (*model.Event, error) {
	return s.settleHandoffNotification(ctx, accountID, id, model.NotificationFailed, reason, model.EventDraftHandoffNotificationFailed)
}

func (s *Store) settleHandoffNotification(ctx context.Context, accountID, id, state, reason, eventType string) (*model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := getAssistantHandlingTx(ctx, tx, accountID, id)
	if err != nil {
		return nil, err
	}
	// Notification is at-most-once from the local outbox: never emit a second
	// event for a state it is already in, and never regress a delivered
	// notification back to failed when a duplicate worker pass re-settles it.
	if r.NotificationStatus == state {
		return nil, nil
	}
	if r.NotificationStatus == model.NotificationSent && state == model.NotificationFailed {
		return nil, nil
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET notification_status=?,updated_at=? WHERE id=? AND account_id=?`, state, now, id, accountID); err != nil {
		return nil, err
	}
	r.NotificationStatus = state
	r.UpdatedAt = parseTime(now)
	payload := handoffPayload(r)
	if reason != "" {
		payload["error"] = reason
	}
	ev, err := insertEventTx(ctx, tx, accountID, r.InboxID, eventType, r.ID, payload)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

// SetHandoffNotificationWorkflow links a handoff record to its queued
// notification workflow job and marks the notification queued. The workflow job
// carries only the notification kind, never a token.
func (s *Store) SetHandoffNotificationWorkflow(ctx context.Context, accountID, id, workflowID string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE assistant_handling_requests SET notification_status=?,updated_at=? WHERE id=? AND account_id=?`, model.NotificationQueued, nowText(), id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_ = workflowID
	return nil
}

// CommitHandoffNotification enqueues the handoff notification workflow job and
// links the handoff record to it in one transaction. The job kind is
// model.WorkflowKindHandoff and it carries no token; RawPath/SizeBytes describe
// the generated notification MIME.
func (s *Store) CommitHandoffNotification(ctx context.Context, accountID string, r model.AssistantHandlingRequest, wf WorkflowRecord, provider, lastError, notificationMessageID string) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id := idgen.New("wfl")
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_workflow(id,account_id,inbox_id,request_id,kind,provider,from_name,from_address,to_json,cc_json,bcc_json,subject,text_body,html_body,raw_path,size_bytes,status,attempts,last_error,next_attempt_at,claim_owner,claim_expires_at,provider_message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',0,?,'','','','',?)`,
		id, accountID, wf.Inbox.ID, r.ID, model.WorkflowKindHandoff, provider, wf.From.Name, wf.From.Address, jsonString(wf.To), jsonString(wf.CC), jsonString(wf.BCC), wf.Subject, wf.Text, wf.HTML, wf.RawPath, wf.SizeBytes, lastError, nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_handling_requests SET notification_status=?,notification_message_id=?,updated_at=? WHERE id=? AND account_id=?`, model.NotificationQueued, strings.TrimSpace(notificationMessageID), nowText(), r.ID, accountID); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// HandoffNotificationWorkflowID resolves the queued notification workflow job id
// for a handoff record, or "" when none is linked.
func (s *Store) HandoffNotificationWorkflowID(ctx context.Context, accountID, requestID string) (string, error) {
	var id string
	err := s.read.QueryRowContext(ctx, `SELECT id FROM outbound_workflow WHERE account_id=? AND request_id=? AND kind=? ORDER BY created_at DESC LIMIT 1`, accountID, requestID, model.WorkflowKindHandoff).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// HandoffByMessageID resolves a handoff record from the RFC5322 Message-ID of the
// frozen draft. It is the inbound-side correlation used to keep a handoff's own
// notification mail out of future remote detection.
func (s *Store) HandoffByMessageID(ctx context.Context, accountID, messageID string) (model.AssistantHandlingRequest, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return model.AssistantHandlingRequest{}, ErrNotFound
	}
	return scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND message_id=? AND message_id<>'' ORDER BY requested_at DESC LIMIT 1`, accountID, messageID))
}

// HandoffByNotificationMessageID resolves a handoff record from the RFC5322
// Message-ID of its generated notification email. A notification delivered back
// into the connected inbox is excluded from remote detection by this durable
// lookup, so the exclusion does not depend on a live body fetch.
func (s *Store) HandoffByNotificationMessageID(ctx context.Context, accountID, messageID string) (model.AssistantHandlingRequest, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return model.AssistantHandlingRequest{}, ErrNotFound
	}
	return scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND notification_message_id=? AND notification_message_id<>'' ORDER BY requested_at DESC LIMIT 1`, accountID, messageID))
}

// HandoffByHandoffID resolves a handoff record from its stable handoff id.
func (s *Store) HandoffByHandoffID(ctx context.Context, accountID, handoffID string) (model.AssistantHandlingRequest, error) {
	handoffID = strings.TrimSpace(handoffID)
	if handoffID == "" {
		return model.AssistantHandlingRequest{}, ErrNotFound
	}
	return scanAssistantHandling(s.read.QueryRowContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND handoff_id=? AND handoff_id<>'' ORDER BY requested_at DESC LIMIT 1`, accountID, handoffID))
}

// CleanupPublishedHandoffLocalDraft queues the frozen local draft's body and
// attachment files for post-commit removal after a confirmed publication, and
// consumes the local draft row. The handoff record and its workflow state are
// retained. It is idempotent: a handoff whose draft is already gone is a no-op.
//
// Files are recorded in pending_file_cleanup inside the transaction and unlinked
// by the startup sweep, so a rollback never leaves a live row pointing at a
// missing file.
func (s *Store) CleanupPublishedHandoffLocalDraft(ctx context.Context, accountID, requestID string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := getAssistantHandlingTx(ctx, tx, accountID, requestID)
	if err != nil {
		return err
	}
	if r.Publication != model.HandoffPublished {
		return ErrConflict
	}
	// Attachment raw paths.
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(raw_path,'') FROM draft_attachments WHERE draft_id=?`, r.DraftID)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		if strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	// Refund the draft's storage (body + attachments) since the draft is consumed.
	var text, html string
	var inboxID string
	err = tx.QueryRowContext(ctx, `SELECT inbox_id,COALESCE(text_body,''),COALESCE(html_body,'') FROM drafts WHERE id=? AND account_id=?`, r.DraftID, accountID).Scan(&inboxID, &text, &html)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		var attBytes int64
		if e := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM draft_attachments WHERE draft_id=?`, r.DraftID).Scan(&attBytes); e != nil {
			return e
		}
		delta := -int64(len(text)+len(html)) - attBytes
		if _, e := tx.ExecContext(ctx, `DELETE FROM draft_attachments WHERE draft_id=?`, r.DraftID); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND account_id=?`, r.DraftID, accountID); e != nil {
			return e
		}
		if delta != 0 {
			if e := adjustStorageTx(ctx, tx, accountID, inboxID, delta); e != nil {
				return e
			}
		}
	}
	for _, p := range paths {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pending_file_cleanup(rel_path,created_at) VALUES(?,?)`, p, nowText()); err != nil {
			return err
		}
	}
	// The handoff's own frozen raw MIME is no longer needed once it is published:
	// the bytes now live on the remote server. Retire it through the durable
	// cleanup queue so a rollback never orphans the row or the file.
	if rp := strings.TrimSpace(r.RawPath); rp != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO pending_file_cleanup(rel_path,created_at) VALUES(?,?)`, rp, nowText()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HandoffCountsByInbox returns the number of handoffs per inbox by publication
// state, for the per-inbox sidebar badge.
func (s *Store) HandoffCountsByInbox(ctx context.Context, p model.Principal, publication string) (map[string]int, error) {
	q := `SELECT inbox_id,COUNT(*) FROM assistant_handling_requests WHERE account_id=?`
	args := []any{p.AccountID}
	if publication != "" {
		q += ` AND publication=?`
		args = append(args, publication)
	}
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
		if err = rows.Scan(&inboxID, &n); err != nil {
			return nil, err
		}
		out[inboxID] = n
	}
	return out, rows.Err()
}

// RecordHandoffAppendAttempt is a convenience that bumps the attempt count and
// records a last error without changing the publication state, used when an
// append attempt fails transiently and will be retried.
func (s *Store) RecordHandoffAppendAttempt(ctx context.Context, accountID, id, reason string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE assistant_handling_requests SET attempts=attempts+1,last_error=?,updated_at=? WHERE id=? AND account_id=?`, reason, nowText(), id, accountID)
	return err
}

// ListAssistantHandlingForInbox returns an inbox's assistant-handling (handoff)
// records, newest first, retaining terminal history (published, ambiguous,
// failed/cancelled) as well as in-flight ones. It enforces the caller's Read role
// on the inbox. It is the durable source for the mailbox's handoff history panel,
// independent of whether the underlying local draft still exists (a published
// handoff's local draft is cleaned up).
func (s *Store) ListAssistantHandlingForInbox(ctx context.Context, p model.Principal, inboxID string, limit int) ([]model.AssistantHandlingRequest, error) {
	if !p.CanRead(inboxID) {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+handlingColumns+` FROM assistant_handling_requests WHERE account_id=? AND inbox_id=? ORDER BY requested_at DESC LIMIT ?`, p.AccountID, inboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AssistantHandlingRequest{}
	for rows.Next() {
		r, scanErr := scanAssistantHandling(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
