package store

import "context"

// DedupeMicrosoftInbox folds cached Microsoft rows that describe the same message
// into one, returning the number of rows removed. The provider can serve one
// message under two id forms, and rows written before that was handled at write
// time left a row per form: the row the list showed could no longer be deleted,
// because the provider had retired its id once the message moved. The newest row
// is kept (a provider write updates it and leaves the retired form untouched) and
// the others are dropped with their provider-id mapping and labels. A group with
// an unsettled action is left alone: the action addresses a row in it.
func (s *Store) DedupeMicrosoftInbox(ctx context.Context, accountID, inboxID string) (int, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT rfc_message_id,subject,from_address FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND rfc_message_id<>'' GROUP BY rfc_message_id,subject,from_address HAVING count(*)>1`, accountID, inboxID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type identity struct{ rfc, subject, from string }
	var groups []identity
	for rows.Next() {
		var g identity
		if err = rows.Scan(&g.rfc, &g.subject, &g.from); err != nil {
			return 0, err
		}
		groups = append(groups, g)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	removed := 0
	for _, g := range groups {
		n, e := s.dedupeMicrosoftGroup(ctx, accountID, inboxID, g.rfc, g.subject, g.from)
		if e != nil {
			return removed, e
		}
		removed += n
	}
	return removed, nil
}

// dedupeMicrosoftGroup removes every row of one identity but the newest, in one
// transaction so the metadata counters and the rows never disagree.
func (s *Store) dedupeMicrosoftGroup(ctx context.Context, accountID, inboxID, rfc, subject, from string) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT m.id,EXISTS(SELECT 1 FROM inbox_message_actions a WHERE a.message_id=m.id AND a.state IN ('pending','running','ambiguous')) FROM inbox_remote_messages m WHERE m.account_id=? AND m.inbox_id=? AND m.rfc_message_id=? AND m.subject=? AND m.from_address=? ORDER BY m.updated_at DESC,m.id DESC`, accountID, inboxID, rfc, subject, from)
	if err != nil {
		return 0, err
	}
	var ids []string
	protected := false
	for rows.Next() {
		var id string
		var busy bool
		if err = rows.Scan(&id, &busy); err != nil {
			rows.Close()
			return 0, err
		}
		protected = protected || busy
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if protected || len(ids) < 2 {
		return 0, nil
	}
	var delta int64
	for _, id := range ids[1:] {
		messageBytes, _, e := remoteMessageBytesTx(ctx, tx, accountID, inboxID, id)
		if e != nil {
			return 0, e
		}
		labelBytes, e := remoteLabelBytesTx(ctx, tx, accountID, inboxID, id)
		if e != nil {
			return 0, e
		}
		if e = deleteRemoteMessageTx(ctx, tx, accountID, inboxID, id); e != nil {
			return 0, e
		}
		delta -= messageBytes + labelBytes
	}
	if err = applyRemoteMetadataDeltaTx(ctx, tx, accountID, inboxID, delta); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids) - 1, nil
}
