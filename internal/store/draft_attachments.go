package store

import (
	"context"
	"database/sql"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

// AddDraftAttachment records a draft attachment file. The caller is responsible
// for writing the raw file to disk at rawPath.
func (s *Store) AddDraftAttachment(ctx context.Context, p model.Principal, draftID string, a model.DraftAttachment) (model.DraftAttachment, error) {
	d, err := s.GetDraft(ctx, p, draftID)
	if err != nil {
		return model.DraftAttachment{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.DraftAttachment{}, ErrForbidden
	}
	a.ID = idgen.New("dat")
	a.DraftID = draftID
	now := nowText()
	_, err = s.write.ExecContext(ctx, `INSERT INTO draft_attachments(id,draft_id,filename,content_type,size_bytes,raw_path,created_at) VALUES(?,?,?,?,?,?,?)`, a.ID, draftID, a.Filename, a.ContentType, a.Size, a.RawPath, now)
	if err != nil {
		return model.DraftAttachment{}, err
	}
	a.CreatedAt = parseTime(now)
	return a, nil
}

func scanDraftAttachment(row interface{ Scan(...any) error }) (model.DraftAttachment, error) {
	var a model.DraftAttachment
	var created string
	err := row.Scan(&a.ID, &a.DraftID, &a.Filename, &a.ContentType, &a.Size, &a.RawPath, &created)
	if err != nil {
		return a, err
	}
	a.CreatedAt = parseTime(created)
	return a, nil
}

// ListDraftAttachments lists a draft's attachments.
func (s *Store) ListDraftAttachments(ctx context.Context, p model.Principal, draftID string) ([]model.DraftAttachment, error) {
	d, err := s.GetDraft(ctx, p, draftID)
	if err != nil {
		return nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return nil, ErrForbidden
	}
	rows, err := s.read.QueryContext(ctx, `SELECT id,draft_id,filename,content_type,size_bytes,raw_path,created_at FROM draft_attachments WHERE draft_id=? ORDER BY created_at`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DraftAttachment
	for rows.Next() {
		a, err := scanDraftAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteDraftAttachment removes a draft attachment row. The caller removes the
// raw file.
func (s *Store) DeleteDraftAttachment(ctx context.Context, p model.Principal, draftID, id string) (string, error) {
	d, err := s.GetDraft(ctx, p, draftID)
	if err != nil {
		return "", err
	}
	if !p.CanAssist(d.InboxID) {
		return "", ErrForbidden
	}
	var raw string
	err = s.read.QueryRowContext(ctx, `SELECT raw_path FROM draft_attachments WHERE id=? AND draft_id=?`, id, draftID).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	_, err = s.write.ExecContext(ctx, `DELETE FROM draft_attachments WHERE id=? AND draft_id=?`, id, draftID)
	return raw, err
}

// DeleteDraftAttachments removes all attachment rows for a draft and returns
// their raw paths so the caller can remove the files.
func (s *Store) DeleteDraftAttachments(ctx context.Context, p model.Principal, draftID string) ([]string, error) {
	d, err := s.GetDraft(ctx, p, draftID)
	if err != nil {
		return nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return nil, ErrForbidden
	}
	rows, err := s.read.QueryContext(ctx, `SELECT raw_path FROM draft_attachments WHERE draft_id=?`, draftID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, raw)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	_, err = s.write.ExecContext(ctx, `DELETE FROM draft_attachments WHERE draft_id=?`, draftID)
	return paths, err
}
