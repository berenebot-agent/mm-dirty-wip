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
	id := idgen.New("drf")
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO drafts(id,account_id,inbox_id,reply_to_message_id,to_json,cc_json,bcc_json,subject,text_body,html_body,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, p.AccountID, d.InboxID, d.ReplyToMessageID, jsonString(d.To), jsonString(d.CC), jsonString(d.BCC), d.Subject, d.Text, d.HTML, now, now)
	if err != nil {
		return model.Draft{}, err
	}
	d.ID = id
	d.CreatedAt = parseTime(now)
	d.UpdatedAt = d.CreatedAt
	return d, nil
}
func scanDraft(row interface{ Scan(...any) error }) (model.Draft, error) {
	var d model.Draft
	var to, cc, bcc, created, updated string
	err := row.Scan(&d.ID, &d.InboxID, &d.ReplyToMessageID, &to, &cc, &bcc, &d.Subject, &d.Text, &d.HTML, &created, &updated)
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
	d, err := scanDraft(s.read.QueryRowContext(ctx, `SELECT id,inbox_id,reply_to_message_id,to_json,cc_json,bcc_json,subject,text_body,html_body,created_at,updated_at FROM drafts WHERE id=? AND account_id=?`, id, p.AccountID))
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	return d, nil
}
func (s *Store) ListDrafts(ctx context.Context, p model.Principal, inboxID string) ([]model.Draft, error) {
	if inboxID != "" && !p.CanAssist(inboxID) {
		return nil, ErrForbidden
	}
	q := `SELECT id,inbox_id,reply_to_message_id,to_json,cc_json,bcc_json,subject,text_body,html_body,created_at,updated_at FROM drafts WHERE account_id=?`
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
	return out, rows.Err()
}
func (s *Store) UpdateDraft(ctx context.Context, p model.Principal, d model.Draft) (model.Draft, error) {
	old, err := s.GetDraft(ctx, p, d.ID)
	if err != nil {
		return model.Draft{}, err
	}
	if d.InboxID == "" {
		d.InboxID = old.InboxID
	}
	if d.InboxID != old.InboxID && !p.CanAssist(d.InboxID) {
		return model.Draft{}, ErrForbidden
	}
	now := nowText()
	_, err = s.write.ExecContext(ctx, `UPDATE drafts SET inbox_id=?,reply_to_message_id=?,to_json=?,cc_json=?,bcc_json=?,subject=?,text_body=?,html_body=?,updated_at=? WHERE id=? AND account_id=?`, d.InboxID, d.ReplyToMessageID, jsonString(d.To), jsonString(d.CC), jsonString(d.BCC), d.Subject, d.Text, d.HTML, now, d.ID, p.AccountID)
	if err != nil {
		return model.Draft{}, err
	}
	return s.GetDraft(ctx, p, d.ID)
}
func (s *Store) DeleteDraft(ctx context.Context, p model.Principal, id string) error {
	d, err := s.GetDraft(ctx, p, id)
	if err != nil {
		return err
	}
	if !p.CanAssist(d.InboxID) {
		return ErrForbidden
	}
	_, err = s.write.ExecContext(ctx, `DELETE FROM drafts WHERE id=?`, id)
	return err
}
func cleanSubject(v string) string { return strings.TrimSpace(v) }
