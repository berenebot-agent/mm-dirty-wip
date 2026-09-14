package store

import (
	"context"
	"database/sql"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

// AddDraftAttachment records a draft attachment file and charges its bytes to
// the account. The caller is responsible for writing the raw file to disk at
// rawPath.
func (s *Store) AddDraftAttachment(ctx context.Context, p model.Principal, draftID string, a model.DraftAttachment) (model.DraftAttachment, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.DraftAttachment{}, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return model.DraftAttachment{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.DraftAttachment{}, ErrForbidden
	}
	if d.Status == model.DraftStatusPendingApproval {
		return model.DraftAttachment{}, ErrConflict
	}
	if err := adjustStorageTx(ctx, tx, p.AccountID, a.Size); err != nil {
		return model.DraftAttachment{}, err
	}
	a.ID = idgen.New("dat")
	a.DraftID = draftID
	now := nowText()
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_attachments(id,draft_id,filename,content_type,size_bytes,content_hash,raw_path,created_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, draftID, a.Filename, a.ContentType, a.Size, a.ContentHash, a.RawPath, now); err != nil {
		return model.DraftAttachment{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.DraftAttachment{}, err
	}
	a.CreatedAt = parseTime(now)
	return a, nil
}

func scanDraftAttachment(row interface{ Scan(...any) error }) (model.DraftAttachment, error) {
	var a model.DraftAttachment
	var created string
	err := row.Scan(&a.ID, &a.DraftID, &a.Filename, &a.ContentType, &a.Size, &a.ContentHash, &a.RawPath, &created)
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
	return s.ListDraftAttachmentsInternal(ctx, p.AccountID, draftID)
}

// ListDraftAttachmentsInternal lists a draft's attachments without a principal.
func (s *Store) ListDraftAttachmentsInternal(ctx context.Context, accountID, draftID string) ([]model.DraftAttachment, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,draft_id,filename,content_type,size_bytes,content_hash,raw_path,created_at FROM draft_attachments WHERE draft_id=? ORDER BY created_at`, draftID)
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

// SetDraftAttachmentHashes persists computed content hashes for a draft's
// attachments, keyed by attachment id. It is used to backfill hashes that were
// empty when a send request froze the draft.
func (s *Store) SetDraftAttachmentHashes(ctx context.Context, accountID, draftID string, hashes map[string]string) error {
	if len(hashes) == 0 {
		return nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := getDraftTx(ctx, tx, accountID, draftID); err != nil {
		return err
	}
	for id, hash := range hashes {
		if _, err := tx.ExecContext(ctx, `UPDATE draft_attachments SET content_hash=? WHERE id=? AND draft_id=?`, hash, id, draftID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetDraftAttachment returns one draft attachment after checking the caller can
// assist the owning inbox.
func (s *Store) GetDraftAttachment(ctx context.Context, p model.Principal, draftID, id string) (model.DraftAttachment, error) {
	d, err := s.GetDraft(ctx, p, draftID)
	if err != nil {
		return model.DraftAttachment{}, err
	}
	if !p.CanAssist(d.InboxID) {
		return model.DraftAttachment{}, ErrForbidden
	}
	a, err := scanDraftAttachment(s.read.QueryRowContext(ctx, `SELECT id,draft_id,filename,content_type,size_bytes,content_hash,raw_path,created_at FROM draft_attachments WHERE id=? AND draft_id=?`, id, draftID))
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	return a, err
}

// draftAttachmentPathsTx returns the raw paths and total byte size of a draft's
// attachments inside the caller's transaction.
func draftAttachmentPathsTx(ctx context.Context, tx *sql.Tx, draftID string) ([]string, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT raw_path,size_bytes FROM draft_attachments WHERE draft_id=?`, draftID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var paths []string
	var total int64
	for rows.Next() {
		var path string
		var size int64
		if err := rows.Scan(&path, &size); err != nil {
			return nil, 0, err
		}
		paths = append(paths, path)
		total += size
	}
	return paths, total, rows.Err()
}

// DeleteDraftAttachment removes a draft attachment row and refunds its bytes.
// The caller removes the raw file.
func (s *Store) DeleteDraftAttachment(ctx context.Context, p model.Principal, draftID, id string) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return "", err
	}
	if !p.CanAssist(d.InboxID) {
		return "", ErrForbidden
	}
	if d.Status == model.DraftStatusPendingApproval {
		return "", ErrConflict
	}
	var raw string
	var size int64
	err = tx.QueryRowContext(ctx, `SELECT raw_path,size_bytes FROM draft_attachments WHERE id=? AND draft_id=?`, id, draftID).Scan(&raw, &size)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_attachments WHERE id=? AND draft_id=?`, id, draftID); err != nil {
		return "", err
	}
	if err := adjustStorageTx(ctx, tx, p.AccountID, -size); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return raw, nil
}

// DeleteDraftAttachments removes all attachment rows for a draft, refunds their
// bytes, and returns their raw paths so the caller can remove the files.
func (s *Store) DeleteDraftAttachments(ctx context.Context, p model.Principal, draftID string) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	d, err := getDraftTx(ctx, tx, p.AccountID, draftID)
	if err != nil {
		return nil, err
	}
	if !p.CanAssist(d.InboxID) {
		return nil, ErrForbidden
	}
	if d.Status == model.DraftStatusPendingApproval {
		return nil, ErrConflict
	}
	paths, total, err := draftAttachmentPathsTx(ctx, tx, draftID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_attachments WHERE draft_id=?`, draftID); err != nil {
		return nil, err
	}
	if err := adjustStorageTx(ctx, tx, p.AccountID, -total); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}
