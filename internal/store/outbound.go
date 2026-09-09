package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

type OutboundCredential struct {
	ID, AccountID, Name, Provider, EncryptedConfig string
	CreatedAt, UpdatedAt                           time.Time
}

func (s *Store) SaveOutboundCredential(ctx context.Context, accountID, id, name, provider, encrypted string) (OutboundCredential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := nowText()
	if id == "" {
		id = idgen.New("out")
		_, err := s.write.ExecContext(ctx, `INSERT INTO outbound_credentials(id,account_id,name,provider,encrypted_config,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, name, provider, encrypted, now, now)
		if err != nil {
			return OutboundCredential{}, err
		}
	} else {
		res, err := s.write.ExecContext(ctx, `UPDATE outbound_credentials SET name=?,provider=?,encrypted_config=?,updated_at=? WHERE id=? AND account_id=?`, name, provider, encrypted, now, id, accountID)
		if err != nil {
			return OutboundCredential{}, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return OutboundCredential{}, ErrNotFound
		}
	}
	return s.GetOutboundCredential(ctx, accountID, id)
}
func (s *Store) GetOutboundCredential(ctx context.Context, accountID, id string) (OutboundCredential, error) {
	var c OutboundCredential
	var created, updated string
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,name,provider,encrypted_config,created_at,updated_at FROM outbound_credentials WHERE id=? AND account_id=?`, id, accountID).Scan(&c.ID, &c.AccountID, &c.Name, &c.Provider, &c.EncryptedConfig, &created, &updated)
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}
func (s *Store) ListOutboundCredentials(ctx context.Context, accountID string) ([]OutboundCredential, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,account_id,name,provider,encrypted_config,created_at,updated_at FROM outbound_credentials WHERE account_id=? ORDER BY name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboundCredential
	for rows.Next() {
		var c OutboundCredential
		var cr, up string
		if err = rows.Scan(&c.ID, &c.AccountID, &c.Name, &c.Provider, &c.EncryptedConfig, &cr, &up); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(cr)
		c.UpdatedAt = parseTime(up)
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) DeleteOutboundCredential(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM outbound_credentials WHERE id=? AND account_id=?`, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) ActiveOutboundCredential(ctx context.Context, accountID string) (OutboundCredential, error) {
	var id string
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(active_outbound_credential_id,'') FROM accounts WHERE id=?`, accountID).Scan(&id)
	if err == sql.ErrNoRows {
		return OutboundCredential{}, ErrNotFound
	}
	if err != nil {
		return OutboundCredential{}, err
	}
	if id == "" {
		return OutboundCredential{}, ErrNotFound
	}
	return s.GetOutboundCredential(ctx, accountID, id)
}

func (s *Store) SetActiveOutboundCredential(ctx context.Context, accountID, id string) error {
	if id != "" {
		var n int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM outbound_credentials WHERE id=? AND account_id=?`, id, accountID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrNotFound
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE accounts SET active_outbound_credential_id=? WHERE id=?`, nullString(id), accountID)
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
	var quota, used int64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, r.Inbox.AccountID).Scan(&quota, &used); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if quota > 0 && used+r.SizeBytes > quota {
		return model.Message{}, model.Event{}, ErrQuota
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
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,bcc_json,envelope_to_json,subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,status,idem_key,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,0,'pending',?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, threadID, "outbound", r.Provider, r.ProviderMessageID, r.RFCMessageID, r.InReplyTo, jsonString(r.References), r.From.Name, r.From.Address, jsonString(r.To), jsonString(r.CC), jsonString(r.BCC), `[]`, r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, r.IdemKey, now)
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
	if err = tx.Commit(); err != nil {
		return model.Message{}, model.Event{}, err
	}
	m, err := s.GetMessageByID(ctx, r.Inbox.AccountID, id)
	return m, model.Event{}, err
}

// MarkSent transitions a pending outbound message to sent after the provider
// accepts it, recording the provider message id and emitting the message.sent
// event. It is the durable truth that delivery succeeded.
func (s *Store) MarkSent(ctx context.Context, accountID, id, providerMessageID string) (model.Message, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	defer tx.Rollback()
	var inboxID, threadID string
	if err = tx.QueryRowContext(ctx, `SELECT inbox_id,thread_id FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&inboxID, &threadID); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, model.Event{}, ErrNotFound
		}
		return model.Message{}, model.Event{}, err
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status='sent',provider_message_id=?,sent_at=?,attempts=attempts+1,last_error='',next_attempt_at='' WHERE id=? AND account_id=?`, providerMessageID, now, id, accountID); err != nil {
		return model.Message{}, model.Event{}, err
	}
	ev, err := insertEventTx(ctx, tx, accountID, inboxID, "message.sent", id, map[string]any{"message_id": id, "inbox_id": inboxID, "thread_id": threadID})
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Message{}, model.Event{}, err
	}
	m, err := s.GetMessageByID(ctx, accountID, id)
	return m, ev, err
}

// MarkFailed records a failed delivery attempt. If attempts remain, the message
// is returned to pending with a next_attempt_at; otherwise it is marked failed.
func (s *Store) MarkFailed(ctx context.Context, accountID, id, errText string, nextAttemptAt time.Time, maxAttempts int) (model.Message, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, err
	}
	defer tx.Rollback()
	var attempts int
	if err = tx.QueryRowContext(ctx, `SELECT attempts FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&attempts); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, ErrNotFound
		}
		return model.Message{}, err
	}
	attempts++
	status := "pending"
	next := ""
	if attempts >= maxAttempts {
		status = "failed"
	} else {
		next = timeText(nextAttemptAt)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status=?,attempts=?,last_error=?,next_attempt_at=? WHERE id=? AND account_id=?`, status, attempts, errText, next, id, accountID); err != nil {
		return model.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Message{}, err
	}
	return s.GetMessageByID(ctx, accountID, id)
}

// ClaimNextPending atomically claims the next due pending message for delivery.
// It returns the message id, or "" if none is due.
func (s *Store) ClaimNextPending(ctx context.Context, now time.Time) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE status='pending' AND (next_attempt_at='' OR next_attempt_at<=?) ORDER BY created_at ASC LIMIT 1`, timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Mark as in-flight so a concurrent claim (or a restart) does not pick it
	// up again while the worker is delivering. next_attempt_at is set far in the
	// future; MarkSent/MarkFailed overwrite it.
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET next_attempt_at=? WHERE id=?`, timeText(now.Add(24*time.Hour)), id); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
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
	_, err = s.write.ExecContext(ctx, `UPDATE messages SET status='pending',attempts=0,last_error='',next_attempt_at='' WHERE id=? AND account_id=?`, id, p.AccountID)
	return err
}

// ListOutbox lists pending and failed outbound messages for an account,
// optionally scoped to an inbox.
func (s *Store) ListOutbox(ctx context.Context, p model.Principal, inboxID string, limit int) ([]model.Message, error) {
	q := messageSelect + ` FROM messages m WHERE m.account_id=? AND m.direction='outbound' AND m.status IN ('pending','failed')`
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
	if limit <= 0 || limit > 200 {
		limit = 100
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

func (s *Store) IdempotencyGet(ctx context.Context, accountID, key string) (string, string, bool, error) {
	var mid, res string
	err := s.read.QueryRowContext(ctx, `SELECT message_id,result_json FROM outbound_idempotency WHERE account_id=? AND idem_key=? AND status='done'`, accountID, key).Scan(&mid, &res)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return mid, res, err == nil, err
}

// IdempotencyReserve atomically claims an idempotency key for an in-flight
// send. It returns (true, "", nil) if this caller won the reservation and may
// proceed to send. It returns (false, messageID, resultJSON, nil) if the key was
// already completed, so the caller can return the stored result. It returns
// (false, "", "", ErrConflict) if another request currently holds the
// reservation (in-flight), which the caller should treat as a retryable
// conflict rather than sending again.
func (s *Store) IdempotencyReserve(ctx context.Context, accountID, key string) (bool, string, string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, "", "", err
	}
	defer tx.Rollback()
	var mid, res, status string
	err = tx.QueryRowContext(ctx, `SELECT message_id,result_json,status FROM outbound_idempotency WHERE account_id=? AND idem_key=?`, accountID, key).Scan(&mid, &res, &status)
	if err == nil {
		if status == "done" {
			return false, mid, res, nil
		}
		// pending: another request is in flight.
		return false, "", "", ErrConflict
	}
	if err != sql.ErrNoRows {
		return false, "", "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_idempotency(account_id,idem_key,message_id,result_json,status,created_at) VALUES(?,?,?,?,?,?)`, accountID, key, "", "", "pending", nowText()); err != nil {
		return false, "", "", err
	}
	if err = tx.Commit(); err != nil {
		return false, "", "", err
	}
	return true, "", "", nil
}

// IdempotencyComplete marks a reserved key as done with its result.
func (s *Store) IdempotencyComplete(ctx context.Context, accountID, key, messageID string, result any) error {
	b, _ := json.Marshal(result)
	_, err := s.write.ExecContext(ctx, `UPDATE outbound_idempotency SET message_id=?,result_json=?,status='done' WHERE account_id=? AND idem_key=?`, messageID, string(b), accountID, key)
	return err
}

// IdempotencyRelease clears a pending reservation so a failed send can be
// retried with the same key.
func (s *Store) IdempotencyRelease(ctx context.Context, accountID, key string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND idem_key=? AND status='pending'`, accountID, key)
	return err
}

func (s *Store) IdempotencyPut(ctx context.Context, accountID, key, messageID string, result any) error {
	b, _ := json.Marshal(result)
	_, err := s.write.ExecContext(ctx, `INSERT OR IGNORE INTO outbound_idempotency(account_id,idem_key,message_id,result_json,status,created_at) VALUES(?,?,?,?,?,?)`, accountID, key, messageID, string(b), "done", nowText())
	return err
}

func (s *Store) LatestMessageInThread(ctx context.Context, accountID, threadID string) (model.Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.account_id=? AND m.thread_id=? ORDER BY m.created_at DESC LIMIT 1`, accountID, threadID))
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
