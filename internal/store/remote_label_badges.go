package store

import "context"

// RemoteInboxLabelUnreadCounts reads the cached provider metadata, including
// Gmail user labels and Outlook categories, without fetching live messages.
// IMAP local-only tags are retained for legacy clients but not exposed in the
// unified Labels sidebar.
func (s *Store) RemoteInboxLabelUnreadCounts(ctx context.Context, accountID, inboxID string) (map[string]int, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT ml.label, count(*) FROM inbox_remote_labels ml
		JOIN inbox_remote_messages m ON m.id=ml.remote_message_id
		WHERE ml.account_id=? AND ml.inbox_id=? AND m.account_id=? AND m.inbox_id=? AND m.is_read=0
		GROUP BY ml.label`, accountID, inboxID, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err = rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}
