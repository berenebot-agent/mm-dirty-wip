package store

import (
	"context"
	"strings"
)

// RemoteMessageFolder returns the provider folder a cached remote row really
// sits in, in the same terms as the inbox_folders path. IMAP rows carry that
// path themselves. Gmail rows are filed under "gmail:<id>" and Graph rows under
// "microsoft:<id>", so those are resolved through the provider's own membership
// data: the first Gmail system label in precedence order, or the Graph folder id.
// The empty string means the row is in no folder view.
func (s *Store) RemoteMessageFolder(ctx context.Context, account, inbox string, rec RemoteMessage) string {
	switch {
	case strings.HasPrefix(rec.FolderPath, "microsoft:"):
		if fp, err := s.MicrosoftMessageFolder(ctx, account, inbox, rec.ID); err == nil {
			return fp
		}
	case strings.HasPrefix(rec.FolderPath, "gmail:"):
		for _, label := range GoogleFolderPrecedence {
			var n int
			err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM google_label_memberships WHERE inbox_id=? AND message_id=? AND label_id=?`, inbox, rec.ID, label).Scan(&n)
			if err == nil && n > 0 {
				return label
			}
		}
		return ""
	}
	return rec.FolderPath
}
