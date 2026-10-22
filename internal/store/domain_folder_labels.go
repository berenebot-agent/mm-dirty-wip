package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// This file holds the one-time conversion of a domain inbox's custom folders
// into additive labels.
//
// A domain mailbox organises mail with labels: its folder tree is the
// universal set of system folders (Inbox, Sent, Drafts, Archive, Outbox, Spam,
// Trash), exactly like every other mailbox.
//
// The conversion adds labels; it never removes anything. The folder record and
// the filed mail's folder membership are preserved, so the organisation stays
// reversible and no filed mail silently reappears in the Inbox. Each converted
// folder's label is recorded in inbox_folder_labels, so a rerun reuses the same
// label instead of deriving a second one.

// domainCustomFolder is one custom folder of a domain inbox, awaiting
// conversion.
type domainCustomFolder struct {
	id        string
	accountID string
	inboxID   string
	path      string
}

// migrateDomainFoldersToLabels gives every custom folder of a domain inbox an
// additive label and adds it to the mail filed there. Folder records and message
// folder membership are retained, so the conversion is reversible.
//
// The label name is derived from the folder path, so the conversion is
// deterministic: the same path always yields the same name. A name already in
// use in that inbox (an existing label, a label another folder already maps
// to, or another folder that flattens to the same name) takes the first free
// numeric suffix, so no folder is dropped and no label collides.
//
// The conversion runs in one transaction and is idempotent: a folder that
// already has a recorded label is left alone.
func (s *Store) migrateDomainFoldersToLabels(ctx context.Context) error {
	rows, err := s.read.QueryContext(ctx, `SELECT f.id,f.account_id,f.inbox_id,f.path
		FROM inbox_folders f JOIN inboxes i ON i.id=f.inbox_id
		LEFT JOIN inbox_folder_labels fl ON fl.folder_id=f.id
		WHERE i.kind='domain' AND f.is_system=0 AND f.role IN ('folder','label') AND fl.folder_id IS NULL
		ORDER BY f.inbox_id,f.path`)
	if err != nil {
		return err
	}
	var folders []domainCustomFolder
	for rows.Next() {
		var f domainCustomFolder
		if err = rows.Scan(&f.id, &f.accountID, &f.inboxID, &f.path); err != nil {
			rows.Close()
			return err
		}
		folders = append(folders, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		return nil
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	used, err := domainFolderLabelNames(ctx, tx, folders)
	if err != nil {
		return err
	}
	now := nowText()
	for _, f := range folders {
		name := freeDomainLabelName(domainFolderLabelName(f.path), used[f.inboxID])
		used[f.inboxID][strings.ToLower(name)] = true
		// The mapping is the durable record of the conversion: it keeps the
		// label stable across restarts and makes the conversion reversible.
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_folder_labels(inbox_id,folder_id,label,created_at) VALUES(?,?,?,?)`, f.inboxID, f.id, name, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_defined_labels(account_id,inbox_id,name) VALUES(?,?,?)`, f.accountID, f.inboxID, name); err != nil {
			return err
		}
		// The filed mail keeps its folder and gains the label that names it.
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id,label,created_at)
			SELECT id,?,? FROM messages WHERE account_id=? AND inbox_id=? AND mailbox_id=?`, name, now, f.accountID, f.inboxID, f.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// domainFolderLabelNames collects the label names already in use in each
// affected inbox, so a converted folder never takes a name that is taken.
func domainFolderLabelNames(ctx context.Context, tx *sql.Tx, folders []domainCustomFolder) (map[string]map[string]bool, error) {
	used := map[string]map[string]bool{}
	for _, f := range folders {
		if used[f.inboxID] == nil {
			used[f.inboxID] = map[string]bool{}
		}
	}
	marks := make([]string, 0, len(used))
	args := make([]any, 0, len(used))
	for id := range used {
		marks = append(marks, "?")
		args = append(args, id)
	}
	inList := strings.Join(marks, ",")
	collect := func(query string) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var inboxID, name string
			if err = rows.Scan(&inboxID, &name); err != nil {
				return err
			}
			used[inboxID][strings.ToLower(name)] = true
		}
		return rows.Err()
	}
	// The labels a folder already maps to.
	if err := collect(`SELECT inbox_id,label FROM inbox_folder_labels WHERE inbox_id IN (` + inList + `)`); err != nil {
		return nil, err
	}
	// The catalogue names.
	if err := collect(`SELECT inbox_id,name FROM inbox_defined_labels WHERE inbox_id IN (` + inList + `)`); err != nil {
		return nil, err
	}
	// The names already in use on messages.
	if err := collect(`SELECT m.inbox_id,ml.label FROM message_labels ml JOIN messages m ON m.id=ml.message_id WHERE m.inbox_id IN (` + inList + `) GROUP BY m.inbox_id,ml.label`); err != nil {
		return nil, err
	}
	return used, nil
}

// domainFolderLabelName flattens a folder path into a flat label name: the
// path separators become dashes, so the hierarchy stays readable in the name.
// The original path stays on the folder record.
func domainFolderLabelName(path string) string {
	flat := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '-'
		}
		return r
	}, path)
	flat = strings.Join(strings.Fields(flat), " ")
	if runes := []rune(flat); len(runes) > model.MaxLabelLength {
		flat = string(runes[:model.MaxLabelLength])
	}
	return flat
}

// freeDomainLabelName returns the first free name for a converted folder: the
// base name, or the base name with the first free numeric suffix. The result
// never exceeds the label length limit: the base is shortened to leave room
// for the suffix, so the name stays a valid label that can be applied and
// renamed later.
func freeDomainLabelName(base string, used map[string]bool) string {
	if base == "" {
		base = "Folder"
	}
	if !used[strings.ToLower(base)] {
		return base
	}
	for k := 2; ; k++ {
		suffix := "-" + strconv.Itoa(k)
		candidate := shortenLabel(base, len([]rune(suffix))) + suffix
		if !used[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

// shortenLabel shortens a label to the limit, leaving room for a suffix.
func shortenLabel(base string, room int) string {
	runes := []rune(base)
	if len(runes)+room <= model.MaxLabelLength {
		return base
	}
	keep := model.MaxLabelLength - room
	if keep < 1 {
		keep = 1
	}
	return strings.TrimRight(string(runes[:keep]), " ")
}
