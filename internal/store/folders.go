package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// This file is the local/domain custom-folder surface. It reuses the shared
// inbox_folders table (introduced for standalone mailboxes in migration 052)
// for BOTH inbox kinds. Folders are a single hierarchical tree per inbox; a
// message belongs to exactly one folder (messages.mailbox_id), and labels remain
// independent free-text message metadata that is never a folder.
//
// Ownership: the store owns folder rows and single-folder membership. The remote
// adapter owns remote folder reconciliation (UpsertFolders) and any provider
// mutation: remote folder operations must be routed in the application layer,
// not performed by silently mutating provider-backed folder rows here. The store
// may carry opaque adapter metadata (inbox_folders.remote_metadata_json) but
// never interprets it.

// ErrFolderNotEmpty is returned when a custom folder that still contains messages
// (including trashed messages) or child folders is deleted. Empty the folder or
// remove its children first; a non-empty delete is never silently cascaded.
var ErrFolderNotEmpty = errors.New("folder is not empty")

// ErrFolderProtected is returned when a rename/delete targets a seeded system
// folder whose role is protected (Inbox, Sent, Drafts, Trash, Spam, Outbox,
// Archive). System folders are managed by the mailbox, not freely editable.
var ErrFolderProtected = errors.New("folder role is protected")

// ErrFolderNameConflict is returned when a folder name collides with a sibling
// folder in the same parent.
var ErrFolderNameConflict = errors.New("a folder with that name already exists")

// systemFolderRoles is the ordered set of seeded system folders for a local or
// domain inbox. The order is the sidebar order; the Inbox is first and carries
// the implicit default membership bucket.
var systemFolderRoles = []struct {
	role string
	name string
}{
	{model.FolderRoleInbox, "Inbox"},
	{model.FolderRoleSent, "Sent"},
	{model.FolderRoleDrafts, "Drafts"},
	{model.FolderRoleArchive, "Archive"},
	{model.FolderRoleOutbox, "Outbox"},
	{model.FolderRoleSpam, "Spam"},
	{model.FolderRoleTrash, "Trash"},
}

// systemRoleProtected reports whether a role is a seeded, protected system role.
func systemRoleProtected(role string) bool {
	for _, s := range systemFolderRoles {
		if s.role == role {
			return true
		}
	}
	return false
}

// SetFolderRole explicitly maps an existing folder to a mailbox role. The folder
// may be arbitrarily named (for example a remote "Old Mail" folder mapped to
// Trash); the mapping is recorded with role_locked=1 so a later remote reconcile
// never resets it by re-inferring the role from the folder name. It requires
// Owner on the inbox (or an account Admin). The role must be a known FolderRole*
// constant; an empty/unknown role is rejected. A seeded system folder may be
// re-affirmed but not reassigned away from its protected role.
func (s *Store) SetFolderRole(ctx context.Context, p model.Principal, inboxID, folderID, role string) (model.Folder, error) {
	if _, err := s.inboxKind(ctx, p.AccountID, inboxID); err != nil {
		return model.Folder{}, err
	}
	if !p.CanOwn(inboxID) && !p.Admin {
		return model.Folder{}, ErrForbidden
	}
	role = strings.TrimSpace(role)
	if !validFolderRole(role) {
		return model.Folder{}, fmt.Errorf("unknown folder role %q", role)
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Folder{}, err
	}
	defer tx.Rollback()
	folder, err := scanFolder(tx.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE inbox_id=? AND id=?`, inboxID, folderID))
	if err == sql.ErrNoRows {
		return model.Folder{}, ErrNotFound
	}
	if err != nil {
		return model.Folder{}, err
	}
	// A seeded system folder's protected role is asserted, never reassigned: a
	// caller must not repurpose the Inbox as, say, Trash. Re-affirming the same
	// role is allowed and locks it.
	if (folder.IsSystem || systemRoleProtected(folder.Role)) && folder.Role != role {
		return model.Folder{}, ErrFolderProtected
	}
	// A role that another folder in the same inbox already holds locked would be
	// ambiguous: a role maps to at most one folder.
	if role != model.FolderRoleFolder && role != model.FolderRoleLabel {
		var clash int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND role=? AND role_locked=1 AND id<>?`, inboxID, role, folderID).Scan(&clash); err != nil {
			return model.Folder{}, err
		}
		if clash != 0 {
			return model.Folder{}, fmt.Errorf("%w: another folder is already mapped to the %s role", ErrConflict, role)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET role=?,role_locked=1,updated_at=? WHERE id=? AND inbox_id=?`, role, nowText(), folderID, inboxID); err != nil {
		return model.Folder{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Folder{}, err
	}
	folder.Role = role
	folder.RoleLocked = true
	return folder, nil
}

// validFolderRole reports whether a string is a known FolderRole* constant.
func validFolderRole(role string) bool {
	switch role {
	case model.FolderRoleFolder, model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleDrafts,
		model.FolderRoleTrash, model.FolderRoleSpam, model.FolderRoleArchive, model.FolderRoleOutbox, model.FolderRoleLabel:
		return true
	default:
		return false
	}
}

// GetFolderByPath resolves one folder of an inbox by its stable path.
func (s *Store) GetFolderByPath(ctx context.Context, accountID, inboxID, path string) (model.Folder, error) {
	f, err := scanFolder(s.read.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE account_id=? AND inbox_id=? AND path=?`, accountID, inboxID, path))
	if err == sql.ErrNoRows {
		return f, ErrNotFound
	}
	return f, err
}

// inboxKind returns the kind of an inbox in the account.
func (s *Store) inboxKind(ctx context.Context, accountID, inboxID string) (string, error) {
	var kind string
	err := s.read.QueryRowContext(ctx, `SELECT kind FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&kind)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return kind, err
}

// EnsureSystemFolders idempotently seeds the protected system folders of an
// inbox of either kind. Each is created with origin='local', is_system=1 and its
// role, so a later remote reconcile never prunes or overwrites it. It returns the
// inbox's full folder list in hierarchical order.
func (s *Store) EnsureSystemFolders(ctx context.Context, accountID, inboxID string) ([]model.Folder, error) {
	if _, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowText()
	for _, sf := range systemFolderRoles {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM inbox_folders WHERE inbox_id=? AND role=?`, inboxID, sf.role).Scan(&existing)
		switch {
		case err == sql.ErrNoRows:
			// A user may already hold a custom folder at this path; do not
			// collide. Fall back to the role as a unique path.
			path := sf.name
			var atPath int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND path=?`, inboxID, path).Scan(&atPath); err != nil {
				return nil, err
			}
			if atPath != 0 {
				continue
			}
			id := idgen.New("fld")
			if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,parent_path,role,selectable,is_system,origin,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, accountID, inboxID, path, sf.name, "", sf.role, 1, 1, "local", now, now); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListFolders(ctx, accountID, inboxID)
}

// GetSystemFolder resolves the seeded system folder of an inbox by role, seeding
// the system set first if needed. It returns ErrNotFound when the inbox has no
// such folder (which cannot happen after seeding, but callers must not assume).
func (s *Store) GetSystemFolder(ctx context.Context, accountID, inboxID, role string) (model.Folder, error) {
	folders, err := s.EnsureSystemFolders(ctx, accountID, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	for _, f := range folders {
		if f.Role == role {
			return f, nil
		}
	}
	return model.Folder{}, ErrNotFound
}

// FolderCreate describes a new custom folder. Path may carry a hierarchy; Name
// is the display name of the final segment (defaulted from Path when empty).
type FolderCreate struct {
	Path   string
	Name   string
	Parent string
}

// validateFolderPath normalizes and validates a custom folder path. A path is a
// slash-separated, non-empty hierarchy of non-empty segments; each segment is
// trimmed and may not contain path separators or control bytes. The reserved
// system-role names are not rejected here: a custom folder whose final name
// happens to match "Inbox" is a plain folder because it is not seeded.
func validateFolderPath(raw string) (string, string, error) {
	p := strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(raw), "/")), "")
	if p == "" {
		return "", "", fmt.Errorf("folder path is required")
	}
	segments := strings.Split(p, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("invalid folder path")
		}
		if strings.ContainsAny(seg, "\\\t\r\n\x00") {
			return "", "", fmt.Errorf("invalid folder path")
		}
	}
	return p, segments[len(segments)-1], nil
}

// CreateFolder creates a custom local folder in an inbox of either kind. It
// requires Assistant or Owner on the inbox. A system-role name is not treated as
// a system folder: the new folder is an ordinary custom folder with role
// FolderRoleFolder and is_system=0. A clashing sibling path returns
// ErrFolderNameConflict.
func (s *Store) CreateFolder(ctx context.Context, p model.Principal, inboxID string, in FolderCreate) (model.Folder, error) {
	if _, err := s.inboxKind(ctx, p.AccountID, inboxID); err != nil {
		return model.Folder{}, err
	}
	if !p.CanAssist(inboxID) {
		return model.Folder{}, ErrForbidden
	}
	path, name, err := validateFolderPath(in.Path)
	if err != nil {
		return model.Folder{}, err
	}
	if strings.TrimSpace(in.Name) != "" {
		name = strings.TrimSpace(in.Name)
		if strings.ContainsAny(name, "/\\\t\r\n\x00") {
			return model.Folder{}, fmt.Errorf("invalid folder name")
		}
	}
	parent := strings.Trim(strings.TrimSpace(in.Parent), "/")
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Folder{}, err
	}
	defer tx.Rollback()
	var existing int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND path=?`, inboxID, path).Scan(&existing); err != nil {
		return model.Folder{}, err
	}
	if existing != 0 {
		return model.Folder{}, ErrFolderNameConflict
	}
	// A child folder requires its parent to already exist (an explicit tree).
	if parent != "" {
		var parentExists int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND path=?`, inboxID, parent).Scan(&parentExists); err != nil {
			return model.Folder{}, err
		}
		if parentExists == 0 {
			return model.Folder{}, fmt.Errorf("%w: parent folder does not exist", ErrNotFound)
		}
	}
	id := idgen.New("fld")
	now := nowText()
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,parent_path,role,selectable,is_system,origin,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, p.AccountID, inboxID, path, name, parent, model.FolderRoleFolder, 1, 0, "local", now, now); err != nil {
		return model.Folder{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Folder{}, err
	}
	return s.GetFolder(ctx, p.AccountID, inboxID, path)
}

// RenameFolder renames a custom folder and moves its whole subtree. The folder's
// path and the path/parent_path of every descendant are rewritten under the new
// name; every row keeps its stable id (so message membership and any client-held
// id survive). A rename that would collide with an existing folder path is
// refused before anything is written (ErrFolderNameConflict), and the whole
// rewrite is atomic. A protected system folder (is_system=1 or a protected role)
// cannot be renamed: ErrFolderProtected. It requires Assistant or Owner.
func (s *Store) RenameFolder(ctx context.Context, p model.Principal, inboxID, folderID, newName string) (model.Folder, error) {
	if _, err := s.inboxKind(ctx, p.AccountID, inboxID); err != nil {
		return model.Folder{}, err
	}
	if !p.CanAssist(inboxID) {
		return model.Folder{}, ErrForbidden
	}
	newName = strings.TrimSpace(newName)
	if newName == "" || strings.ContainsAny(newName, "/\\\t\r\n\x00") {
		return model.Folder{}, fmt.Errorf("invalid folder name")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Folder{}, err
	}
	defer tx.Rollback()
	folder, err := scanFolder(tx.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE inbox_id=? AND id=?`, inboxID, folderID))
	if err == sql.ErrNoRows {
		return model.Folder{}, ErrNotFound
	}
	if err != nil {
		return model.Folder{}, err
	}
	if folder.IsSystem || systemRoleProtected(folder.Role) {
		return model.Folder{}, ErrFolderProtected
	}
	oldPath := folder.Path
	newPath := newName
	if folder.ParentPath != "" {
		newPath = folder.ParentPath + "/" + newName
	}
	if newPath == oldPath {
		// Same name in the same parent: still guard for a case-only change below.
		var clash int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND path=? COLLATE NOCASE AND id<>?`, inboxID, newPath, folderID).Scan(&clash); err != nil {
			return model.Folder{}, err
		}
		if clash != 0 {
			return model.Folder{}, ErrFolderNameConflict
		}
	}
	// Detect a collision before writing, so the move is refused atomically rather
	// than failing mid-update. A descendant path cannot equal newPath (it is
	// prefixed with oldPath), so only an outside folder can collide on the exact
	// path; the unique key is (inbox_id, path).
	var clash int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND id<>? AND path=? COLLATE NOCASE`, inboxID, folderID, newPath).Scan(&clash); err != nil {
		return model.Folder{}, err
	}
	if clash != 0 {
		return model.Folder{}, ErrFolderNameConflict
	}
	// A sibling with the same display name in the same parent would be ambiguous.
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND parent_path=? AND name=? COLLATE NOCASE AND id<>? AND path NOT LIKE ? ESCAPE '\'`,
		inboxID, folder.ParentPath, newName, folderID, escapeLike(oldPath)+"/%").Scan(&clash); err != nil {
		return model.Folder{}, err
	}
	if clash != 0 {
		return model.Folder{}, ErrFolderNameConflict
	}
	now := nowText()
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET path=?,name=?,updated_at=? WHERE id=? AND inbox_id=?`, newPath, newName, now, folderID, inboxID); err != nil {
		return model.Folder{}, err
	}
	// Rewrite every descendant's path and parent_path under the new path.
	suffixFrom := len(oldPath) + 1 // 1-based substr offset just past oldPath + '/'
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET
			path=?||substr(path,?),
			parent_path=CASE WHEN parent_path=? THEN ? ELSE ?||substr(parent_path,?) END,
			updated_at=?
		WHERE inbox_id=? AND path LIKE ? ESCAPE '\'`,
		newPath, suffixFrom, oldPath, newPath, newPath, suffixFrom, now, inboxID, escapeLike(oldPath)+"/%"); err != nil {
		return model.Folder{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Folder{}, err
	}
	folder.Path = newPath
	folder.Name = newName
	folder.ParentPath = parentPathOf(newPath)
	return folder, nil
}

// escapeLike escapes the LIKE wildcards in a literal prefix so a folder path
// containing % or _ is matched literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

// parentPathOf returns the parent of a slash-separated folder path, or "".
func parentPathOf(path string) string {
	if idx := strings.LastIndexByte(path, '/'); idx > 0 {
		return path[:idx]
	}
	return ""
}

// DeleteFolder permanently removes an empty custom folder. A protected system
// folder cannot be deleted (ErrFolderProtected). A folder that still contains
// messages (including trashed messages) or child folders is not empty and
// returns ErrFolderNotEmpty: the emptiness test is explicit and never cascades.
// Purging the messages themselves is a separate, Owner-only operation. It
// requires Assistant or Owner on the inbox.
func (s *Store) DeleteFolder(ctx context.Context, p model.Principal, inboxID, folderID string) error {
	if _, err := s.inboxKind(ctx, p.AccountID, inboxID); err != nil {
		return err
	}
	if !p.CanAssist(inboxID) {
		return ErrForbidden
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	folder, err := scanFolder(tx.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE inbox_id=? AND id=?`, inboxID, folderID))
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if folder.IsSystem || systemRoleProtected(folder.Role) {
		return ErrFolderProtected
	}
	// Children (by parent_path) and messages (including trashed ones) both keep
	// the folder non-empty. Trashed mail still counts: a message in Trash is
	// still a member of its folder for emptiness purposes.
	var children int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_folders WHERE inbox_id=? AND parent_path=?`, inboxID, folder.Path).Scan(&children); err != nil {
		return err
	}
	var messages int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE account_id=? AND inbox_id=? AND mailbox_id=?`, p.AccountID, inboxID, folderID).Scan(&messages); err != nil {
		return err
	}
	if children != 0 || messages != 0 {
		return ErrFolderNotEmpty
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_folders WHERE id=? AND inbox_id=?`, folderID, inboxID); err != nil {
		return err
	}
	return tx.Commit()
}

// MoveMessageToFolder sets a message's single folder membership. It requires
// Assistant or Owner on the message's inbox, and the destination folder must
// belong to the same inbox (a cross-inbox or cross-account move is ErrNotFound).
// Moving to the system Inbox is expressed by destFolderID="" (the implicit
// default bucket). A move to the message's current folder is a no-op that still
// returns the current message and no event. It returns a message.folder_changed
// event carrying the old and new folder ids.
func (s *Store) MoveMessageToFolder(ctx context.Context, p model.Principal, messageID, destFolderID string) (model.Message, *model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	m, err := s.getMessageTx(ctx, tx, p.AccountID, messageID)
	if err != nil {
		return model.Message{}, nil, err
	}
	if !p.CanAssist(m.InboxID) {
		return model.Message{}, nil, ErrForbidden
	}
	if m.Internal {
		return model.Message{}, nil, ErrNotFound
	}
	dest := strings.TrimSpace(destFolderID)
	destPath := ""
	if dest != "" {
		folder, ferr := scanFolder(tx.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM inbox_folders WHERE inbox_id=? AND id=?`, m.InboxID, dest))
		if ferr == sql.ErrNoRows {
			return model.Message{}, nil, ErrNotFound
		}
		if ferr != nil {
			return model.Message{}, nil, ferr
		}
		if !folder.Selectable {
			return model.Message{}, nil, fmt.Errorf("%w: folder is not selectable", ErrConflict)
		}
		destPath = folder.Path
	}
	oldID := m.MailboxID
	if oldID == dest {
		if err = tx.Commit(); err != nil {
			return model.Message{}, nil, err
		}
		return m, nil, nil
	}
	var destArg any
	if dest != "" {
		destArg = dest
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET mailbox_id=? WHERE id=? AND account_id=?`, destArg, messageID, p.AccountID); err != nil {
		return model.Message{}, nil, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, m.InboxID, model.EventMessageFolderChanged, messageID, map[string]any{
		"message_id": messageID, "inbox_id": m.InboxID, "thread_id": m.ThreadID,
		"old_folder_id": oldID, "new_folder_id": dest, "new_folder_path": destPath,
	})
	if err != nil {
		return model.Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		return model.Message{}, nil, err
	}
	m.MailboxID = dest
	m.FolderPath = destPath
	return m, &ev, nil
}

// SetFolderRemoteMetadata replaces the opaque adapter metadata of a folder. It is
// the store primitive a remote adapter uses to record bookkeeping such as a
// remote UIDVALIDITY. The store never interprets the JSON. It requires the folder
// to belong to the inbox and the caller to be the adapter (no principal check is
// performed here; the application layer owns remote routing and authorization).
func (s *Store) SetFolderRemoteMetadata(ctx context.Context, accountID, inboxID, folderID, metadataJSON string) error {
	if strings.TrimSpace(metadataJSON) == "" {
		metadataJSON = "{}"
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_folders SET remote_metadata_json=?,updated_at=? WHERE account_id=? AND inbox_id=? AND id=?`, metadataJSON, nowText(), accountID, inboxID, folderID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFolderRemoteMetadata returns the opaque adapter metadata of a folder.
func (s *Store) GetFolderRemoteMetadata(ctx context.Context, accountID, inboxID, folderID string) (string, error) {
	var meta string
	err := s.read.QueryRowContext(ctx, `SELECT remote_metadata_json FROM inbox_folders WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, folderID).Scan(&meta)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return meta, err
}

// ListInboxFolders lists an inbox's folders (both kinds) for a principal that can
// read it.
func (s *Store) ListInboxFolders(ctx context.Context, p model.Principal, inboxID string) ([]model.Folder, error) {
	if !p.CanRead(inboxID) {
		return nil, ErrForbidden
	}
	return s.ListFolders(ctx, p.AccountID, inboxID)
}

// FolderStats are the per-folder message and unread counts for an inbox.
type FolderStats struct {
	// ByID holds the total and unread counts for each custom/system folder that
	// stores membership in messages.mailbox_id.
	Total  map[string]int
	Unread map[string]int
	// Inbox is the implicit system Inbox bucket: messages with mailbox_id NULL.
	InboxTotal  int
	InboxUnread int
}

// CountFolderMessages returns the message and unread counts per folder for an
// inbox, plus the implicit Inbox bucket. Membership is by messages.mailbox_id for
// custom and role folders. The system Sent, Trash and Spam buckets are NOT
// derived here: Sent is direction='outbound', Trash is deleted_at IS NOT NULL and
// Spam is is_spam=1, which are orthogonal to folder membership and are counted by
// CountOutbound/CountTrash/CountSpam. A folder's count therefore reflects only
// mail physically filed into it, never mail that is merely outbound or trashed.
func (s *Store) CountFolderMessages(ctx context.Context, p model.Principal, inboxID string) (FolderStats, error) {
	if !p.CanRead(inboxID) {
		return FolderStats{}, ErrForbidden
	}
	stats := FolderStats{Total: map[string]int{}, Unread: map[string]int{}}
	rows, err := s.read.QueryContext(ctx, `SELECT COALESCE(mailbox_id,''), COUNT(*), SUM(CASE WHEN is_read=0 THEN 1 ELSE 0 END) FROM messages WHERE account_id=? AND inbox_id=? AND internal=0 GROUP BY mailbox_id`, p.AccountID, inboxID)
	if err != nil {
		return FolderStats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var total, unread int
		if err = rows.Scan(&id, &total, &unread); err != nil {
			return FolderStats{}, err
		}
		if id == "" {
			stats.InboxTotal, stats.InboxUnread = total, unread
			continue
		}
		stats.Total[id] = total
		stats.Unread[id] = unread
	}
	return stats, rows.Err()
}
