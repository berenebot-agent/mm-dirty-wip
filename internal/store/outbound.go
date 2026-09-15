package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/limits"
	"gatehouse-mail/internal/model"
)

// HoldPending records why a pending message is not being delivered and defers
// its next attempt without counting a retry. It is used when a domain has no
// outbound provider yet: the message stays queued and delivers once a provider
// is assigned.
func (s *Store) HoldPending(ctx context.Context, accountID, id, reason string, next time.Time) error {
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending'`, reason, timeText(next), id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CommitOutbound(ctx context.Context, r OutboundRecord) (model.Message, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	defer tx.Rollback()
	if r.SendingExternalAliasID != "" {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM external_aliases WHERE id=? AND account_id=? AND inbox_id=? AND address=?`, r.SendingExternalAliasID, r.Inbox.AccountID, r.Inbox.ID, r.From.Address).Scan(&n); err != nil {
			return model.Message{}, model.Event{}, err
		}
		if n != 1 || r.SendingDomainID != "" {
			return model.Message{}, model.Event{}, ErrExternalAliasDeleted
		}
	}
	// Consume the draft first so its freed bytes count against the quota check
	// and the draft can never be double-counted alongside the sent message.
	if r.DraftID != "" {
		d, err := getDraftTx(ctx, tx, r.Inbox.AccountID, r.DraftID)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		_, attTotal, err := draftAttachmentPathsTx(ctx, tx, r.DraftID)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND account_id=?`, r.DraftID, r.Inbox.AccountID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		if err = adjustStorageTx(ctx, tx, r.Inbox.AccountID, -(draftBodyBytes(d) + attTotal)); err != nil {
			return model.Message{}, model.Event{}, err
		}
	}
	var quota, used int64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, r.Inbox.AccountID).Scan(&quota, &used); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if quota > 0 && used+r.SizeBytes > quota {
		return model.Message{}, model.Event{}, ErrQuota
	}
	draftEvent := model.Event{}
	newReqID := ""
	if r.NewSendRequest != nil {
		created, cerr := createSendRequestTx(ctx, tx, r.Inbox.AccountID, *r.NewSendRequest)
		if cerr != nil {
			return model.Message{}, model.Event{}, cerr
		}
		newReqID = created.ID
	}
	threadID := r.ThreadID
	now := nowText()
	if threadID == "" {
		threadID, err = findThreadTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, r.InReplyTo, r.References)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		if threadID == "" {
			threadID = idgen.New("thr")
			if _, err = tx.ExecContext(ctx, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES(?,?,?,?,?,?)`, threadID, r.Inbox.AccountID, r.Inbox.ID, r.Subject, now, now); err != nil {
				return model.Message{}, model.Event{}, err
			}
		}
	}
	id := idgen.New("msg")
	// The message is enqueued as pending; the worker marks it sent after the
	// provider accepts it. sent_at is left NULL until delivery succeeds.
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,bcc_json,envelope_to_json,client_label,client_id,subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,status,idem_key,last_error,internal,sending_domain_id,sending_external_alias_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,0,'pending',?,?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, threadID, "outbound", r.Provider, r.ProviderMessageID, r.RFCMessageID, r.InReplyTo, jsonString(r.References), r.From.Name, r.From.Address, jsonString(r.To), jsonString(r.CC), jsonString(r.BCC), `[]`, r.ClientLabel, r.ClientID, r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, r.IdemKey, r.LastError, boolInt(r.Internal), nullString(r.SendingDomainID), r.SendingExternalAliasID, now)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	var attachmentNames []string
	for _, a := range r.Attachments {
		aid := idgen.New("att")
		if _, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,part_index,content_id) VALUES(?,?,?,?,?,?,?)`, aid, id, a.Filename, a.ContentType, a.Size, a.PartIndex, a.ContentID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		attachmentNames = append(attachmentNames, a.Filename)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_fts(message_id,account_id,inbox_id,subject,from_address,recipients,body,attachment_names) VALUES(?,?,?,?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, r.Subject, r.From.Address, strings.Join(append(append([]string{}, r.To...), r.CC...), " "), r.Text+" "+stripHTMLText(r.HTML), strings.Join(attachmentNames, " ")); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=storage_used_bytes+? WHERE id=?`, r.SizeBytes, r.Inbox.AccountID); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE threads SET updated_at=? WHERE id=?`, now, threadID); err != nil {
		return model.Message{}, model.Event{}, err
	}
	// Record the key-to-message mapping as part of enqueueing, so a retry with
	// the same key replays the queued message regardless of delivery outcome.
	if r.IdemKey != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_idempotency(account_id,idem_key,message_id,inbox_id,result_json,status,created_at) VALUES(?,?,?,?,?,'done',?) ON CONFLICT(account_id,idem_key) DO UPDATE SET message_id=excluded.message_id,inbox_id=excluded.inbox_id,result_json=excluded.result_json,status='done'`, r.Inbox.AccountID, r.IdemKey, id, r.Inbox.ID, "{}", now); err != nil {
			return model.Message{}, model.Event{}, err
		}
	}
	// An approved send claims its send request in the same transaction that
	// enqueues the message, so a request can never authorize two sends.
	if r.SendRequestID != "" {
		ev, aerr := approveSendRequestTx(ctx, tx, r.Inbox.AccountID, r.DraftID, r.SendRequestID, r.DecisionActor, r.DecisionActorID, r.DecisionMethod, r.DecisionFeedback, id)
		if aerr != nil {
			return model.Message{}, model.Event{}, aerr
		}
		draftEvent = ev
	}
	// A newly created external request links to its just-queued approval email
	// and emits the requested event in the same transaction.
	if newReqID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET approval_message_id=?,updated_at=? WHERE id=? AND account_id=?`, id, now, newReqID, r.Inbox.AccountID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		req, rerr := getSendRequestByIDTx(ctx, tx, r.Inbox.AccountID, newReqID)
		if rerr != nil {
			return model.Message{}, model.Event{}, rerr
		}
		ev, eerr := insertEventTx(ctx, tx, r.Inbox.AccountID, req.InboxID, model.EventDraftSendRequested, req.ID, sendRequestPayload(req))
		if eerr != nil {
			return model.Message{}, model.Event{}, eerr
		}
		draftEvent = ev
	}
	m, err := s.getMessageTx(ctx, tx, r.Inbox.AccountID, id)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), r.Inbox.AccountID, id); gerr == nil {
			return existing, model.Event{}, nil
		}
		return model.Message{}, model.Event{}, err
	}
	return m, draftEvent, nil
}

// MarkSent transitions a pending outbound message to sent after the provider
// accepts it, recording the provider message id and emitting the message.sent
// event. It is the durable truth that delivery succeeded. The delivery attempt
// is appended to the per-domain log in the same transaction. The domain is
// derived from the message's inbox in the same transaction, so deleting the
// domain config during an in-flight send cannot break outcome persistence.
func (s *Store) MarkSent(ctx context.Context, accountID, id, providerMessageID, provider string) (model.Message, []model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	var inboxID, threadID, domainID string
	var internal int
	if err = tx.QueryRowContext(ctx, `SELECT m.inbox_id,m.thread_id,CASE WHEN m.sending_external_alias_id<>'' THEN '' ELSE COALESCE(m.sending_domain_id,i.domain_id) END,m.internal FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, id, accountID).Scan(&inboxID, &threadID, &domainID, &internal); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, nil, ErrNotFound
		}
		return model.Message{}, nil, err
	}
	now := nowText()
	update := `UPDATE messages SET status='sent',provider=?,provider_message_id=?,sent_at=?,attempts=attempts+1,last_error='',next_attempt_at='',claim_owner='',claim_expires_at=''`
	args := []any{provider, providerMessageID, now}
	// A provider that assigns its own Message-ID (Brevo, Mailgun) returns the
	// real wire id, which is what recipients reply to. Promote it so that
	// In-Reply-To/References matching finds this message. Provider ids that are
	// not RFC Message-IDs (Resend's UUID, SMTP's sentinel) are left alone.
	if looksLikeRFC5322MessageID(providerMessageID) {
		update += `,rfc_message_id=?`
		args = append(args, strings.TrimSpace(providerMessageID))
	}
	update += ` WHERE id=? AND account_id=?`
	args = append(args, id, accountID)
	if _, err = tx.ExecContext(ctx, update, args...); err != nil {
		return model.Message{}, nil, err
	}
	// A message cancelled (deleted) between the initial read and this write is
	// gone: do not append an unattributable delivery-log row, and report it as
	// not-found so the caller can distinguish cancellation from a send error.
	if ok, exErr := messageExistsTx(ctx, tx, accountID, id); exErr != nil {
		return model.Message{}, nil, exErr
	} else if !ok {
		return model.Message{}, nil, ErrNotFound
	}
	if err = s.insertDeliveryAttemptTx(ctx, tx, accountID, domainID, provider, id, "sent", providerMessageID, ""); err != nil {
		return model.Message{}, nil, err
	}
	events := []model.Event{}
	if internal == 0 {
		ev, eerr := insertEventTx(ctx, tx, accountID, inboxID, "message.sent", id, map[string]any{"message_id": id, "inbox_id": inboxID, "thread_id": threadID})
		if eerr != nil {
			return model.Message{}, nil, eerr
		}
		events = append(events, ev)
	}
	if dev, derr := sendRequestDeliveryTx(ctx, tx, accountID, id, model.SendDeliverySent, model.EventDraftSent); derr != nil {
		return model.Message{}, nil, derr
	} else if dev != nil {
		events = append(events, *dev)
	}
	m, err := s.getMessageTx(ctx, tx, accountID, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), accountID, id); gerr == nil {
			return existing, events, nil
		}
		return model.Message{}, nil, err
	}
	return m, events, nil
}

// MarkFailed records a failed delivery attempt. If attempts remain, the message
// is returned to pending with a next_attempt_at; otherwise it is marked failed.
// The failed attempt is appended to the per-domain log in the same transaction.
// The domain is derived from the message's inbox in the same transaction, so a
// config deletion during an in-flight send cannot break outcome persistence.
func (s *Store) MarkFailed(ctx context.Context, accountID, id, errText string, nextAttemptAt time.Time, maxAttempts int, provider string) (model.Message, []model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	var attempts int
	var domainID string
	if err = tx.QueryRowContext(ctx, `SELECT m.attempts,CASE WHEN m.sending_external_alias_id<>'' THEN '' ELSE COALESCE(m.sending_domain_id,i.domain_id) END FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, id, accountID).Scan(&attempts, &domainID); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, nil, ErrNotFound
		}
		return model.Message{}, nil, err
	}
	attempts++
	status := "pending"
	next := ""
	if attempts >= maxAttempts {
		status = "failed"
	} else {
		next = timeText(nextAttemptAt)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status=?,attempts=?,last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=?`, status, attempts, errText, next, id, accountID); err != nil {
		return model.Message{}, nil, err
	}
	// A message cancelled (deleted) between the initial read and this write is
	// gone; do not append an unattributable delivery-log row for it.
	if ok, exErr := messageExistsTx(ctx, tx, accountID, id); exErr != nil {
		return model.Message{}, nil, exErr
	} else if !ok {
		return model.Message{}, nil, ErrNotFound
	}
	if err = s.insertDeliveryAttemptTx(ctx, tx, accountID, domainID, provider, id, "failed", "", errText); err != nil {
		return model.Message{}, nil, err
	}
	var events []model.Event
	if status == "failed" {
		if dev, derr := sendRequestDeliveryTx(ctx, tx, accountID, id, model.SendDeliveryFailed, model.EventDraftSendFailed); derr != nil {
			return model.Message{}, nil, derr
		} else if dev != nil {
			events = append(events, *dev)
		}
	}
	m, err := s.getMessageTx(ctx, tx, accountID, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), accountID, id); gerr == nil {
			return existing, events, nil
		}
		return model.Message{}, nil, err
	}
	return m, events, nil
}

// ClaimNextPending atomically claims the next due pending message for delivery.
// It returns the message id, or "" if none is due. The claim is separate from
// retry scheduling: next_attempt_at is left untouched and only the claim lease
// is recorded, so a crash can be recovered without waiting 24 hours.
func (s *Store) ClaimNextPending(ctx context.Context, now time.Time, owner string, lease time.Duration) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE status='pending' AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY created_at ASC LIMIT 1`, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET claim_owner=?,claim_expires_at=? WHERE id=?`, owner, timeText(now.Add(lease)), id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// RecoverAbandonedClaims clears claims left by a previous process. Under the
// single-process model every outstanding claim at startup is abandoned.
func (s *Store) RecoverAbandonedClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE messages SET claim_owner='',claim_expires_at='' WHERE status='pending' AND claim_owner!=''`)
	return err
}

// ReleaseMessageClaim clears a claim held by owner so the message can be
// retried immediately instead of waiting for the lease to expire. The status
// and ownership guard make it a no-op for a re-claimed or resolved message.
func (s *Store) ReleaseMessageClaim(ctx context.Context, id, owner string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE messages SET claim_owner='',claim_expires_at='' WHERE id=? AND claim_owner=? AND status='pending'`, id, owner)
	return err
}

// MessageClaimOwner returns the current claim owner for a message.
func (s *Store) MessageClaimOwner(ctx context.Context, accountID, id string) (string, error) {
	var owner string
	err := s.read.QueryRowContext(ctx, `SELECT claim_owner FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return owner, err
}

// RequeuePendingForDomain resets every pending outbound message for a domain so
// it retries immediately after the domain's sending configuration is saved.
// This covers both messages whose inbox is on the domain and send-as-alias
// messages that chose the domain's config. Attempts, retry backoff, the
// no-provider hold and any claim lease are all cleared, restoring the full
// retry budget. Sent and failed messages are untouched; failed messages still
// require an explicit retry.
func (s *Store) RequeuePendingForDomain(ctx context.Context, accountID, domainID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE account_id=? AND direction='outbound' AND status='pending' AND sending_external_alias_id='' AND (sending_domain_id=? OR (sending_domain_id IS NULL AND inbox_id IN (SELECT id FROM inboxes WHERE domain_id=? AND account_id=?)))`, accountID, domainID, domainID, accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RequeueFailed resets a failed message to pending for a manual retry.
func (s *Store) RequeueFailed(ctx context.Context, p model.Principal, id string) error {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return err
	}
	if m.Direction != "outbound" || m.Status != "failed" {
		return ErrConflict
	}
	if !p.CanOwn(m.InboxID) {
		return ErrForbidden
	}
	_, err = s.write.ExecContext(ctx, `UPDATE messages SET status='pending',attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE id=? AND account_id=?`, id, p.AccountID)
	return err
}

// ListOutbox lists pending and failed outbound messages for an account,
// optionally scoped to an inbox.
func (s *Store) ListOutbox(ctx context.Context, p model.Principal, inboxID string, limit int) ([]model.Message, error) {
	q := messageSelect + ` FROM messages m WHERE m.account_id=? AND m.internal=0 AND m.direction='outbound' AND m.status IN ('pending','failed')`
	args := []any{p.AccountID}
	if inboxID != "" {
		if !p.CanRead(inboxID) {
			return nil, ErrForbidden
		}
		q += ` AND m.inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Message{}, nil
		}
		q += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q += ` ORDER BY m.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountOutbox returns the number of pending or failed outbound messages for
// an inbox (or across all accessible inboxes when inboxID is empty).
func (s *Store) CountOutbox(ctx context.Context, p model.Principal, inboxID string) (int, error) {
	q := `SELECT count(*) FROM messages m WHERE m.account_id=? AND m.internal=0 AND m.direction='outbound' AND m.status IN ('pending','failed')`
	args := []any{p.AccountID}
	if inboxID != "" {
		if !p.CanRead(inboxID) {
			return 0, ErrForbidden
		}
		q += ` AND m.inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return 0, nil
		}
		q += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	var n int
	if err := s.read.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOutboxMessage removes a pending or failed outbound message (cancelling
// a queued send or discarding a failed one).
func (s *Store) DeleteOutboxMessage(ctx context.Context, p model.Principal, id string) (string, int64, model.Event, error) {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return "", 0, model.Event{}, err
	}
	if m.Direction != "outbound" || (m.Status != "pending" && m.Status != "failed") {
		return "", 0, model.Event{}, ErrConflict
	}
	if !p.CanOwn(m.InboxID) {
		return "", 0, model.Event{}, ErrForbidden
	}
	return s.DeleteMessage(ctx, p, id)
}

// idempotencyLease bounds how long a pending reservation may block the same
// key before it is treated as abandoned by a crashed process. It is generous
// relative to a provider send so a slow in-flight send is never reclaimed.
const idempotencyLease = 15 * time.Minute

// IdempotencyReserve atomically claims an idempotency key for an in-flight
// send. It returns (true, "", nil) if this caller won the reservation and may
// proceed to enqueue. It returns (false, messageID, nil) if the key was already
// completed, so the caller can return the existing message. It returns
// (false, "", ErrConflict) if another request currently holds the reservation
// (in-flight), which the caller should treat as a retryable conflict.
//
// A pending reservation older than idempotencyLease with no message linked to
// it is treated as abandoned (the process crashed between reserving and
// enqueueing) and is reclaimed, so a legitimate retry is not blocked forever.
// A reservation whose message exists is never reclaimed, because the send was
// durably enqueued and replaying it would be wrong.
func (s *Store) IdempotencyReserve(ctx context.Context, accountID, key, inboxID string) (bool, string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var mid, status, created string
	err = tx.QueryRowContext(ctx, `SELECT message_id,status,created_at FROM outbound_idempotency WHERE account_id=? AND idem_key=?`, accountID, key).Scan(&mid, &status, &created)
	if err == nil {
		if status == "done" {
			return false, mid, nil
		}
		// Pending. Reclaim it only when it is stale and has no enqueued message.
		if mid == "" && !parseTime(created).IsZero() && time.Since(parseTime(created)) > idempotencyLease {
			if _, err = tx.ExecContext(ctx, `UPDATE outbound_idempotency SET status='pending',inbox_id=?,created_at=? WHERE account_id=? AND idem_key=? AND status='pending' AND message_id=''`, inboxID, nowText(), accountID, key); err != nil {
				return false, "", err
			}
			if err = tx.Commit(); err != nil {
				return false, "", err
			}
			return true, "", nil
		}
		// Another request is in flight.
		return false, "", ErrConflict
	}
	if err != sql.ErrNoRows {
		return false, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_idempotency(account_id,idem_key,message_id,inbox_id,result_json,status,created_at) VALUES(?,?,?,?,?,?,?)`, accountID, key, "", inboxID, "", "pending", nowText()); err != nil {
		return false, "", err
	}
	if err = tx.Commit(); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// IdempotencyRelease clears a pending reservation so a failed send can be
// retried with the same key.
func (s *Store) IdempotencyRelease(ctx context.Context, accountID, key string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND idem_key=? AND status='pending'`, accountID, key)
	return err
}

// RecoverStaleIdempotency deletes pending reservations left by a crashed
// process: a pending key with no linked message older than the lease. Keys with
// an enqueued message are preserved (the send is durable and replayable).
func (s *Store) RecoverStaleIdempotency(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE status='pending' AND message_id='' AND created_at<=?`, timeText(now.Add(-idempotencyLease)))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) LatestMessageInThread(ctx context.Context, accountID, threadID string) (model.Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.account_id=? AND m.thread_id=? AND m.internal=0 ORDER BY m.created_at DESC LIMIT 1`, accountID, threadID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) LatestInboundMessageInThread(ctx context.Context, accountID, inboxID, threadID string) (model.Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.account_id=? AND m.inbox_id=? AND m.thread_id=? AND m.direction='inbound' ORDER BY m.created_at DESC LIMIT 1`, accountID, inboxID, threadID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}
