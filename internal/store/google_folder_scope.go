package store

import (
	"fmt"
	"strings"
)

// Gmail gives a message several system labels at once, but a mailbox shows one
// folder per message. The view a message belongs to is the first system label
// it carries in this order: Trash, Spam, Drafts, Inbox, Sent.
//
// Every folder-scoped read resolves the view the same way, so a message is
// never listed in one view and counted in another.
//
// A Gmail user label is not a folder view: it is an additive label, reachable
// through a label filter, and it never appears as a folder scope. A message
// that carries no system label therefore belongs to no folder view at all —
// there is no derived Archive view.
var GoogleFolderPrecedence = []string{"TRASH", "SPAM", "DRAFT", "INBOX", "SENT"}

// googleFolderScopeSQL returns the predicate and arguments that select the
// cached Gmail messages of one folder view. idExpr names the message id
// column ("id" or "m.id"); the arguments are the inbox id and, where the view
// excludes higher-precedence views, the inbox id again.
//
// A scope that is not a system folder view selects nothing: Gmail's user
// labels are additive labels, not locations.
func googleFolderScopeSQL(inboxID, idExpr, path string) (string, []any) {
	path = strings.TrimSpace(path)
	for i, label := range GoogleFolderPrecedence {
		if label != path {
			continue
		}
		clause := fmt.Sprintf(`%s IN (SELECT g.message_id FROM google_label_memberships g WHERE g.inbox_id=? AND g.label_id=?)`, idExpr)
		args := []any{inboxID, path}
		if higher := GoogleFolderPrecedence[:i]; len(higher) > 0 {
			clause += fmt.Sprintf(` AND NOT EXISTS (SELECT 1 FROM google_label_memberships g WHERE g.inbox_id=? AND g.message_id=%s AND g.label_id IN (%s))`,
				idExpr, googleLabelList(higher))
			args = append(args, inboxID)
		}
		return clause, args
	}
	return `1=0`, nil
}

// googleLabelList renders label ids as a quoted SQL list.
func googleLabelList(labels []string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, "'"+l+"'")
	}
	return strings.Join(quoted, ",")
}
