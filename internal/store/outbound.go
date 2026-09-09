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
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,envelope_to_json,subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,sent_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,0,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, threadID, "outbound", r.Provider, r.ProviderMessageID, r.RFCMessageID, r.InReplyTo, jsonString(r.References), r.From.Name, r.From.Address, jsonString(r.To), jsonString(r.CC), `[]`, r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, timeText(r.SentAt), now)
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
	ev, err := insertEventTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, "message.sent", id, map[string]any{"message_id": id, "inbox_id": r.Inbox.ID, "thread_id": threadID})
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Message{}, model.Event{}, err
	}
	m, err := s.GetMessageByID(ctx, r.Inbox.AccountID, id)
	return m, ev, err
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
