package store

import (
	"context"
	"database/sql"
	"strings"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

func (s *Store) CreateDraft(ctx context.Context, p model.Principal, d model.Draft) (model.Draft, error) {
	if !p.CanAssist(d.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Draft{}, err
	}
	defer tx.Rollback()
	if err := adjustStorageTx(ctx, tx, p.AccountID, draftBodyBytes(d)); err != nil {
		return model.Draft{}, err
	}
	from, _, err := resolveSenderQuery(ctx, tx, p.AccountID, d.InboxID, d.FromAddress)
	if err != nil {
		return model.Draft{}, err
	}
	id := idgen.New("drf")
	now := nowText()
	if _, err := tx.ExecContext(ctx, `INSERT INTO drafts(id,account_id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.AccountID, d.InboxID, d.ReplyToMessageID, from.Address, from.Name, jsonString(d.To), jsonString(d.CC), jsonString(d.BCC), d.Subject, d.Text, d.HTML, model.DraftStatusDraft, now, now); err != nil {
		return model.Draft{}, err
	}
	d.FromAddress = from.Address
	d.FromName = from.Name
	if err := tx.Commit(); err != nil {
		return model.Draft{}, err
	}
	d.ID = id
	d.Status = model.DraftStatusDraft
	d.CreatedAt = parseTime(now)
	d.UpdatedAt = d.CreatedAt
	return d, nil
}
func scanDraft(row interface{ Scan(...any) error }) (model.Draft, error) {
	var d model.Draft
	var to, cc, bcc, created, updated string
	err := row.Scan(&d.ID, &d.InboxID, &d.ReplyToMessageID, &d.FromAddress, &d.FromName, &to, &cc, &bcc, &d.Subject, &d.Text, &d.HTML, &d.Status, &created, &updated)
	if err != nil {
		return d, err
	}
	d.To = decodeStrings(to)
	d.CC = decodeStrings(cc)
	d.BCC = decodeStrings(bcc)
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	return d, nil
}
func (s *Store) GetDraft(ctx context.Context, p model.Principal, id string) (model.Draft, error) {
	d, err := scanDraft(s.read.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, p.AccountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	d.SendRequest, _ = s.latestSendRequestForDraft(ctx, p.AccountID, id)
	return d, nil
}

// GetDraftInternal loads a draft without a principal. It is used by the
// external email approval path, which has already proved the decision.
func (s *Store) GetDraftInternal(ctx context.Context, accountID, id string) (model.Draft, error) {
	d, err := scanDraft(s.read.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	return d, err
}
func (s *Store) ListDrafts(ctx context.Context, p model.Principal, inboxID string) ([]model.Draft, error) {
	if inboxID != "" && !p.CanAssist(inboxID) {
		return nil, ErrForbidden
	}
	q := `SELECT id,inbox_id,reply_to_message_id,from_address,from_name,to_json,cc_json,bcc_json,subject,text_body,html_body,status,created_at,updated_at FROM drafts WHERE account_id=?`
	args := []any{p.AccountID}
	if inboxID != "" {
		q += ` AND inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := []string{}
		for id, role := range p.MailboxRoles {
			if role == "assistant" || role == "owner" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return []model.Draft{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Draft
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.attachLatestSendRequests(ctx, p.AccountID, out)
	return out, nil
}

// CountDrafts returns the number of drafts in an inbox (or across all
// accessible inboxes when inboxID is empty).
func (s *Store) CountDrafts(ctx context.Context, p model.Principal, inboxID string) (int, error) {
	if inboxID != "" && !p.CanAssist(inboxID) {
		return 0, ErrForbidden
	}
	q := `SELECT count(*) FROM drafts WHERE account_id=?`
	args := []any{p.AccountID}
	if inboxID != "" {
		q += ` AND inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := []string{}
		for id, role := range p.MailboxRoles {
			if role == "assistant" || role == "owner" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return 0, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
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

func (s *Store) UpdateDraft(ctx context.Context, p model.Principal, d model.Draft) (model.Draft, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Draft{}, err
	}
	defer tx.Rollback()
	old, err := getDraftTx(ctx, tx, p.AccountID, d.ID)
	if err != nil {
		return model.Draft{}, err
	}
	if !p.CanAssist(old.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	if d.InboxID == "" {
		d.InboxID = old.InboxID
	}
	if d.InboxID != old.InboxID && !p.CanAssist(d.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	// A pending draft is frozen: the approval must apply to the exact version
	// reviewed, so the caller must cancel the send request before editing.
	// Editing a rejected draft is the documented way to revise and resubmit.
	if old.Status == model.DraftStatusPendingApproval {
		return model.Draft{}, ErrConflict
	}
	from, _, err := resolveSenderQuery(ctx, tx, p.AccountID, d.InboxID, d.FromAddress)
	if err != nil {
		return model.Draft{}, err
	}
	status := model.DraftStatusDraft
	if err := adjustStorageTx(ctx, tx, p.AccountID, draftBodyBytes(d)-draftBodyBytes(old)); err != nil {
		return model.Draft{}, err
	}
	now := nowText()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET inbox_id=?,reply_to_message_id=?,from_address=?,from_name=?,to_json=?,cc_json=?,bcc_json=?,subject=?,text_body=?,html_body=?,status=?,updated_at=? WHERE id=? AND account_id=?`, d.InboxID, d.ReplyToMessageID, from.Address, from.Name, jsonString(d.To), jsonString(d.CC), jsonString(d.BCC), d.Subject, d.Text, d.HTML, status, now, d.ID, p.AccountID); err != nil {
		return model.Draft{}, err
	}
	d.FromAddress = from.Address
	d.FromName = from.Name
	if err := tx.Commit(); err != nil {
		return model.Draft{}, err
	}
	d.Status = status
	d.CreatedAt = old.CreatedAt
	d.UpdatedAt = parseTime(now)
	return d, nil
}

// DeleteDraftCascade removes a draft and its attachment rows and returns the
// attachment file paths so the caller can remove them from disk.
func (s *Store) DeleteDraftCascade(ctx context.Context, p model.Principal, id string) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, id)
	if err != nil {
		return nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return nil, ErrForbidden
	}
	// Deleting a draft invalidates any outstanding send request; the request row
	// itself is retained as history.
	if _, err := tx.ExecContext(ctx, `UPDATE draft_send_requests SET status=?,updated_at=? WHERE draft_id=? AND account_id=? AND status=?`, model.SendRequestCancelled, nowText(), id, p.AccountID, model.SendRequestPending); err != nil {
		return nil, err
	}
	paths, attTotal, err := draftAttachmentPathsTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND account_id=?`, id, p.AccountID); err != nil {
		return nil, err
	}
	if err := adjustStorageTx(ctx, tx, p.AccountID, -(draftBodyBytes(d) + attTotal)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}

func (s *Store) DeleteDraft(ctx context.Context, p model.Principal, id string) error {
	_, err := s.DeleteDraftCascade(ctx, p, id)
	return err
}
func cleanSubject(v string) string { return strings.TrimSpace(v) }
