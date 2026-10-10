package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// This file is the durable remote-metadata index for standalone (IMAP) mailboxes.
// It owns the cached header/thread/folder/label view of a remote account and the
// on-demand reconcile that the application layer drives from the remote adapter.
//
// Invariants:
//
//   - A body is never archived. Only headers, flags, thread keys, folder paths,
//     UIDVALIDITY and local labels are stored.
//   - UIDVALIDITY is the per-folder anchor. A cached row is only a valid locator
//     while (account_id, inbox_id, folder_path, remote_uid_validity) still matches
//     the live folder; when it changes the UID is stale and the row must be
//     re-resolved by Message-ID, never fetched by the old UID.
//   - Threads are account- and inbox-scoped: every thread query constrains both.
//   - The index-completeness state is independent of reachability: it records how
//     much of the remote mailbox has ever been enumerated, so a scoped read can
//     report honestly whether its local view is trustworthy.

// Remote index status values, stored in inboxes.remote_index_status.
const (
	// RemoteIndexNeverStarted: no reconcile has ever completed for the inbox.
	RemoteIndexNeverStarted = "never_started"
	// RemoteIndexPartial: some folders/messages are indexed but not the whole
	// scope, so the local view may be missing messages.
	RemoteIndexPartial = "partial"
	// RemoteIndexComplete: the whole selected root scope has been enumerated.
	RemoteIndexComplete = "complete"
	// RemoteIndexError: the last reconcile failed; the cached view may be stale.
	RemoteIndexError = "error"
)

// RemoteIndexStatus is the index-completeness record of a standalone inbox.
type RemoteIndexStatus struct {
	Status    string
	IndexedAt time.Time
	Error     string
}

// SetRemoteIndexStatus records the index-completeness state of an inbox. It is
// called by the reconcile driver; it never touches message/folder rows.
func (s *Store) SetRemoteIndexStatus(ctx context.Context, accountID, inboxID, status, errText string) error {
	if !validRemoteIndexStatus(status) {
		return fmt.Errorf("unknown remote index status %q", status)
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET remote_index_status=?,remote_indexed_at=?,remote_index_error=? WHERE id=? AND account_id=?`,
		status, nowText(), publicRemoteIndexError(errText), inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func validRemoteIndexStatus(status string) bool {
	switch status {
	case RemoteIndexNeverStarted, RemoteIndexPartial, RemoteIndexComplete, RemoteIndexError:
		return true
	default:
		return false
	}
}

// publicRemoteIndexError keeps only a short, safe description; a raw provider
// error is never persisted in the index status (it could echo an address or a
// server banner).
func publicRemoteIndexError(errText string) string {
	errText = strings.TrimSpace(errText)
	if len(errText) > 200 {
		errText = errText[:200]
	}
	return errText
}

// GetRemoteIndexStatus returns the index state of a standalone inbox.
func (s *Store) GetRemoteIndexStatus(ctx context.Context, accountID, inboxID string) (RemoteIndexStatus, error) {
	var st RemoteIndexStatus
	var indexed string
	err := s.read.QueryRowContext(ctx, `SELECT remote_index_status,remote_indexed_at,remote_index_error FROM inboxes WHERE id=? AND account_id=? AND kind='standalone'`, inboxID, accountID).
		Scan(&st.Status, &indexed, &st.Error)
	if err == sql.ErrNoRows {
		return RemoteIndexStatus{}, ErrNotFound
	}
	if err != nil {
		return RemoteIndexStatus{}, err
	}
	st.IndexedAt = parseTime(indexed)
	return st, nil
}

// DeleteRemoteFolderRow removes a single remote-owned folder row and its cached
// message metadata after the provider confirmed the folder was deleted. It is
// scoped to origin='remote' so a locally-seeded or operator folder sharing the
// path is never removed by a remote mutation.
func (s *Store) DeleteRemoteFolderRow(ctx context.Context, accountID, inboxID, path string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=?`, inboxID, path); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_folders WHERE account_id=? AND inbox_id=? AND path=? AND origin='remote'`, accountID, inboxID, path); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoteFolderSync is one folder to reconcile: its path/hierarchy/role, the live
// UIDVALIDITY observed for it, and its selectability. It is the input the remote
// adapter's DiscoverFolders maps onto.
type RemoteFolderSync struct {
	Path         string
	Name         string
	ParentPath   string
	Role         string
	Selectable   bool
	UIDValidity  uint32
	HasChildren  bool
	SpecialUse   []string
	IndexedAt    time.Time
	RemoteStatus string
}

// ReconcileRemoteFolders replaces the remote-owned folder subset of an inbox from
// a sync pass, recording each folder's live UIDVALIDITY. Locally-owned folders
// (origin='local', seeded system or custom) are never touched. It is the
// folder-index half of a reconcile and runs in one transaction.
func (s *Store) ReconcileRemoteFolders(ctx context.Context, accountID, inboxID string, folders []RemoteFolderSync) ([]model.Folder, error) {
	if kind, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return nil, err
	} else if kind != model.InboxKindStandalone {
		return nil, ErrStandaloneRequired
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowText()
	keep := map[string]bool{}
	for _, f := range folders {
		path := strings.TrimSpace(f.Path)
		if path == "" {
			continue
		}
		keep[path] = true
		name := strings.TrimSpace(f.Name)
		if name == "" {
			name = remoteLastSegment(path)
		}
		role := f.Role
		if role == "" {
			role = FolderRoleForName(name)
		}
		indexed := now
		if !f.IndexedAt.IsZero() {
			indexed = timeText(f.IndexedAt)
		}
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM inbox_folders WHERE inbox_id=? AND path=?`, inboxID, path).Scan(&existingID)
		switch {
		case err == sql.ErrNoRows:
			id := idgen.New("fld")
			if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_folders(id,account_id,inbox_id,path,name,parent_path,role,selectable,is_system,origin,remote_uid_validity,remote_indexed_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,0,'remote',?,?,?,?)`,
				id, accountID, inboxID, path, name, strings.TrimSpace(f.ParentPath), role, boolInt(f.Selectable), f.UIDValidity, indexed, now, now); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			// Only a remote-owned folder is updated; a locally-owned folder that
			// shares the path keeps its own name, role and protection. A role the
			// operator locked explicitly (role_locked=1) is never overwritten by a
			// name-inferred role, so an arbitrary remote folder stays mapped to
			// its chosen role across reconciles.
			if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET name=?,parent_path=?,role=CASE WHEN role_locked=1 THEN role ELSE ? END,selectable=?,origin='remote',remote_uid_validity=?,remote_indexed_at=?,updated_at=? WHERE id=? AND origin='remote'`,
				name, strings.TrimSpace(f.ParentPath), role, boolInt(f.Selectable), f.UIDValidity, indexed, now, existingID); err != nil {
				return nil, err
			}
		}
	}
	// Prune remote-owned folders that are no longer in scope. A folder that still
	// holds cached message rows is only pruned with them (FK cascade); a folder
	// whose messages are not cached is dropped cleanly.
	rows, err := tx.QueryContext(ctx, `SELECT id,path FROM inbox_folders WHERE inbox_id=? AND origin='remote'`, inboxID)
	if err != nil {
		return nil, err
	}
	var prune []string
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		if !keep[path] {
			prune = append(prune, id)
		}
	}
	rows.Close()
	for _, id := range prune {
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE inbox_id=? AND folder_path IN (SELECT path FROM inbox_folders WHERE id=?)`, inboxID, id); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_folders WHERE id=? AND origin='remote'`, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListFolders(ctx, accountID, inboxID)
}

// RemoteFolderBackfill is the persisted per-folder backfill cursor for a remote
// inbox. Generation is the UIDVALIDITY the cursor belongs to; BeforeUID is the
// high-water such that every UID strictly greater than it has been indexed;
// Complete is true once the downward backfill reached the bottom of the folder.
// A cursor whose Generation differs from the folder's live UIDVALIDITY is stale
// and must be treated as unstarted.
type RemoteFolderBackfill struct {
	Generation uint32
	BeforeUID  uint32
	Complete   bool
	Known      bool
}

// GetRemoteFolderBackfill returns a folder's persisted backfill cursor.
func (s *Store) GetRemoteFolderBackfill(ctx context.Context, accountID, inboxID, folderPath string) (RemoteFolderBackfill, error) {
	var b RemoteFolderBackfill
	var complete int
	err := s.read.QueryRowContext(ctx, `SELECT remote_uid_validity,backfill_before_uid,backfill_complete FROM inbox_folders WHERE account_id=? AND inbox_id=? AND path=?`, accountID, inboxID, folderPath).
		Scan(&b.Generation, &b.BeforeUID, &complete)
	if err == sql.ErrNoRows {
		return RemoteFolderBackfill{}, ErrNotFound
	}
	if err != nil {
		return RemoteFolderBackfill{}, err
	}
	b.Complete = complete != 0
	b.Known = true
	return b, nil
}

// ReconcileRemoteFolderBatch applies one bounded reconcile batch to a folder in a
// single transaction: it upserts the batch's message metadata, optionally prunes
// cached rows whose UID is absent from a COMPLETE snapshot (keepUIDs), and records
// the folder's UIDVALIDITY and the advanced backfill cursor. When the batch's
// UIDVALIDITY differs from the stored one, every cached row in the folder is
// dropped first so no wrong UID is ever served, and the backfill cursor resets.
//
// prune must be true only when keepUIDs is the complete UID set of the folder; a
// partial view must never prune. backfillComplete marks the folder as fully
// enumerated for this generation. Returns the number of rows upserted.
func (s *Store) ReconcileRemoteFolderBatch(ctx context.Context, accountID, inboxID, folderPath string, uidValidity uint32, messages []RemoteMessageInput, keepUIDs []uint32, prune bool, backfillBefore uint32, backfillComplete bool) (int, error) {
	if kind, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return 0, err
	} else if kind != model.InboxKindStandalone {
		return 0, ErrStandaloneRequired
	}
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		return 0, fmt.Errorf("remote folder path is required")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowText()

	// Invalidate stale rows when UIDVALIDITY changed: a UID from a prior
	// generation no longer names the same message. Reset the backfill cursor too.
	var storedValidity uint32
	var haveStored bool
	err = tx.QueryRowContext(ctx, `SELECT remote_uid_validity FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? LIMIT 1`, inboxID, folderPath).Scan(&storedValidity)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return 0, err
	default:
		haveStored = true
	}
	if haveStored && storedValidity != uidValidity {
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=?`, inboxID, folderPath); err != nil {
			return 0, err
		}
		backfillBefore = 0
		backfillComplete = false
	}

	written := 0
	for _, m := range messages {
		if m.UID == 0 {
			continue
		}
		in := m
		in.FolderPath = folderPath
		in.UIDValidity = uidValidity
		threadKey := remoteThreadKeyTx(ctx, tx, inboxID, in)
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,in_reply_to,references_json,thread_key,from_name,from_address,to_json,cc_json,subject,snippet,size_bytes,has_attachments,is_read,is_flagged,is_answered,is_draft,flags_json,received_at,sent_at,internal_date,indexed_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(inbox_id,folder_path,remote_uid_validity,remote_uid) DO UPDATE SET
				rfc_message_id=excluded.rfc_message_id,in_reply_to=excluded.in_reply_to,references_json=excluded.references_json,thread_key=excluded.thread_key,
				from_name=excluded.from_name,from_address=excluded.from_address,to_json=excluded.to_json,cc_json=excluded.cc_json,subject=excluded.subject,
				snippet=excluded.snippet,size_bytes=excluded.size_bytes,has_attachments=excluded.has_attachments,is_read=excluded.is_read,is_flagged=excluded.is_flagged,
				is_answered=excluded.is_answered,is_draft=excluded.is_draft,flags_json=excluded.flags_json,
				received_at=excluded.received_at,sent_at=excluded.sent_at,internal_date=excluded.internal_date,indexed_at=excluded.indexed_at,updated_at=excluded.updated_at`,
			idgen.New("rm"), accountID, inboxID, folderPath, uidValidity, in.UID, in.RFCMessageID, in.InReplyTo, jsonString(in.References), threadKey,
			in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, boolInt(in.HasAttach), boolInt(in.Read), boolInt(in.Flagged),
			boolInt(in.Answered), boolInt(in.Draft), jsonString(in.Flags), nullStringPtr(in.ReceivedAt), nullStringPtr(in.SentAt), nullStringPtr(in.InternalDate), now, now, now); err != nil {
			return 0, err
		}
		written++
		// Merge any thread relatives now that this message exists: a reply that
		// arrived before its parent is folded into the parent's thread.
		if err = mergeRemoteThreadTx(ctx, tx, inboxID, in); err != nil {
			return 0, err
		}
	}
	if prune {
		keep := make(map[uint32]bool, len(keepUIDs))
		for _, u := range keepUIDs {
			keep[u] = true
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,remote_uid FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? AND remote_uid_validity=?`, inboxID, folderPath, uidValidity)
		if err != nil {
			return 0, err
		}
		var drop []string
		for rows.Next() {
			var id string
			var uid uint32
			if err = rows.Scan(&id, &uid); err != nil {
				rows.Close()
				return 0, err
			}
			if !keep[uid] {
				drop = append(drop, id)
			}
		}
		rows.Close()
		for _, id := range drop {
			if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE id=?`, id); err != nil {
				return 0, err
			}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE inbox_folders SET remote_uid_validity=?,remote_indexed_at=?,backfill_before_uid=?,backfill_complete=?,updated_at=? WHERE inbox_id=? AND path=?`,
		uidValidity, now, backfillBefore, boolInt(backfillComplete), now, inboxID, folderPath)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The folder row is absent (for example pruned by a concurrent folder
		// reconcile between discovery and this batch). Fail rather than commit
		// orphan message metadata with no owning folder; the caller records the
		// pass partial and retries after the next folder reconcile.
		return 0, fmt.Errorf("remote folder %q is not indexed for inbox %s", folderPath, inboxID)
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// ReconcileRemoteFolderMessages replaces the cached message metadata of one folder
// with the supplied set, dropping rows no longer present. It is scoped to the
// folder's UIDVALIDITY: if the live UIDVALIDITY differs from the one the caller
// expected, the whole folder's stale rows are dropped first so no wrong UID is
// ever served. Bodies are never stored.
//
// messages is the set of message metadata to upsert. prune controls whether
// cached rows whose UID is absent from messages are removed: it must be true only
// when messages is the COMPLETE enumeration of the folder. A partial (windowed)
// enumeration must pass prune=false so it does not delete cached metadata for
// messages outside the window. Returns the number of rows written.
func (s *Store) ReconcileRemoteFolderMessages(ctx context.Context, accountID, inboxID, folderPath string, uidValidity uint32, messages []RemoteMessageInput, prune bool) (int, error) {
	if kind, err := s.inboxKind(ctx, accountID, inboxID); err != nil {
		return 0, err
	} else if kind != model.InboxKindStandalone {
		return 0, ErrStandaloneRequired
	}
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		return 0, fmt.Errorf("remote folder path is required")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowText()

	// Invalidate stale rows when UIDVALIDITY changed: a UID from a prior
	// UIDVALIDITY generation no longer names the same message.
	var storedValidity uint32
	var haveStored bool
	err = tx.QueryRowContext(ctx, `SELECT remote_uid_validity FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? LIMIT 1`, inboxID, folderPath).Scan(&storedValidity)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return 0, err
	default:
		haveStored = true
	}
	if haveStored && storedValidity != uidValidity {
		if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=?`, inboxID, folderPath); err != nil {
			return 0, err
		}
	}

	keep := map[uint32]bool{}
	written := 0
	for _, m := range messages {
		if m.UID == 0 {
			continue
		}
		keep[m.UID] = true
		in := m
		in.FolderPath = folderPath
		in.UIDValidity = uidValidity
		threadKey := remoteThreadKeyTx(ctx, tx, inboxID, in)
		_, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,in_reply_to,references_json,thread_key,from_name,from_address,to_json,cc_json,subject,snippet,size_bytes,has_attachments,is_read,is_flagged,is_answered,is_draft,flags_json,received_at,sent_at,internal_date,indexed_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(inbox_id,folder_path,remote_uid_validity,remote_uid) DO UPDATE SET
				rfc_message_id=excluded.rfc_message_id,in_reply_to=excluded.in_reply_to,references_json=excluded.references_json,thread_key=excluded.thread_key,
				from_name=excluded.from_name,from_address=excluded.from_address,to_json=excluded.to_json,cc_json=excluded.cc_json,subject=excluded.subject,
				snippet=excluded.snippet,size_bytes=excluded.size_bytes,has_attachments=excluded.has_attachments,is_read=excluded.is_read,is_flagged=excluded.is_flagged,
				is_answered=excluded.is_answered,is_draft=excluded.is_draft,flags_json=excluded.flags_json,
				received_at=excluded.received_at,sent_at=excluded.sent_at,internal_date=excluded.internal_date,indexed_at=excluded.indexed_at,updated_at=excluded.updated_at`,
			idgen.New("rm"), accountID, inboxID, folderPath, uidValidity, in.UID, in.RFCMessageID, in.InReplyTo, jsonString(in.References), threadKey,
			in.FromName, in.FromAddress, jsonString(in.To), jsonString(in.CC), in.Subject, in.Snippet, in.SizeBytes, boolInt(in.HasAttach), boolInt(in.Read), boolInt(in.Flagged),
			boolInt(in.Answered), boolInt(in.Draft), jsonString(in.Flags), nullStringPtr(in.ReceivedAt), nullStringPtr(in.SentAt), nullStringPtr(in.InternalDate), now, now, now)
		if err != nil {
			return 0, err
		}
		written++
	}
	// Prune cached rows in this folder whose UID is no longer present, but only
	// from a complete enumeration. A partial window must not delete metadata for
	// messages outside it.
	if prune {
		rows, err := tx.QueryContext(ctx, `SELECT id,remote_uid FROM inbox_remote_messages WHERE inbox_id=? AND folder_path=? AND remote_uid_validity=?`, inboxID, folderPath, uidValidity)
		if err != nil {
			return 0, err
		}
		var drop []string
		for rows.Next() {
			var id string
			var uid uint32
			if err = rows.Scan(&id, &uid); err != nil {
				rows.Close()
				return 0, err
			}
			if !keep[uid] {
				drop = append(drop, id)
			}
		}
		rows.Close()
		for _, id := range drop {
			if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE id=?`, id); err != nil {
				return 0, err
			}
		}
	}
	// Record the folder's UIDVALIDITY and index time on the folder row so a
	// scoped read knows its metadata is current for this generation.
	if _, err = tx.ExecContext(ctx, `UPDATE inbox_folders SET remote_uid_validity=?,remote_indexed_at=?,updated_at=? WHERE inbox_id=? AND path=?`, uidValidity, now, now, inboxID, folderPath); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// RemoteFolderValidity returns the cached UIDVALIDITY and last index time of a
// folder, and whether it is known.
func (s *Store) RemoteFolderValidity(ctx context.Context, accountID, inboxID, folderPath string) (uint32, time.Time, bool, error) {
	var validity uint32
	var indexed string
	err := s.read.QueryRowContext(ctx, `SELECT remote_uid_validity,remote_indexed_at FROM inbox_folders WHERE account_id=? AND inbox_id=? AND path=?`, accountID, inboxID, folderPath).Scan(&validity, &indexed)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return validity, parseTime(indexed), true, nil
}

// threadQueryer is the read surface remoteThreadKey and mergeRemoteThread need;
// it is satisfied by *sql.Tx and *sql.DB.
type threadQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// remoteAncestorRefs returns the ordered, de-duplicated Message-ID references of a
// message, root-first: the References header in order, then In-Reply-To. They are
// the candidate parent links used to thread a reply with its conversation.
func remoteAncestorRefs(in RemoteMessageInput) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in.References)+1)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, r := range in.References {
		add(r)
	}
	add(in.InReplyTo)
	return out
}

// remoteThreadKey resolves a remote message's thread key within one inbox. It is
// robust to arbitrary arrival order: a reply whose parent is not yet cached is
// anchored at the root-most reference (which may not exist yet), so when the
// parent later arrives from either direction they converge (mergeRemoteThread
// folds the whole sub-thread). An already-supplied ThreadKey is honoured. When a
// referenced Message-ID is duplicated (more than one cached row shares it) the
// reference is NOT followed, so two distinct messages with the same RFC Message-ID
// are never conflated; the message anchors at its own Message-ID instead.
func remoteThreadKey(ctx context.Context, q threadQueryer, inboxID string, in RemoteMessageInput) string {
	if k := strings.TrimSpace(in.ThreadKey); k != "" {
		return k
	}
	refs := remoteAncestorRefs(in)
	own := strings.TrimSpace(in.RFCMessageID)
	for _, ref := range refs {
		var threadKey string
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(thread_key),'') FROM inbox_remote_messages WHERE inbox_id=? AND rfc_message_id=?`, inboxID, ref).Scan(&count, &threadKey); err != nil {
			continue
		}
		if count == 1 && strings.TrimSpace(threadKey) != "" {
			return threadKey
		}
		// count == 0 (missing parent) or count > 1 (duplicate RFC id): do not
		// follow a cached thread; the root-most reference becomes the anchor so a
		// later-arriving parent can still join. Only the first reference is used as
		// the root anchor, which keeps duplicates from conflating.
		if count == 0 {
			return ref
		}
	}
	return own
}

// remoteThreadKeyTx is remoteThreadKey against a transaction.
func remoteThreadKeyTx(ctx context.Context, tx *sql.Tx, inboxID string, in RemoteMessageInput) string {
	return remoteThreadKey(ctx, tx, inboxID, in)
}

// mergeRemoteThread folds a message's whole thread component onto one canonical
// key after the message row is written. It looks one level up (a cached ancestor's
// deeper root) and one level down (direct replies that reference this message),
// updating every row sharing a merged key so an entire sub-thread travels together.
// It is bounded by the thread component's size and never crosses the inbox scope.
// Duplicate RFC Message-IDs are left alone (remoteThreadKey did not join them).
func mergeRemoteThread(ctx context.Context, q threadQueryer, inboxID string, in RemoteMessageInput) error {
	msgID := strings.TrimSpace(in.RFCMessageID)
	if msgID == "" {
		return nil
	}
	var canonical string
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MIN(thread_key),'') FROM inbox_remote_messages WHERE inbox_id=? AND rfc_message_id=?`, inboxID, msgID).Scan(&canonical); err != nil {
		return err
	}
	if canonical == "" {
		return nil
	}
	// Prefer a cached ancestor's (deeper) root. A duplicate ancestor Message-ID is
	// skipped, matching remoteThreadKey's duplicate guard.
	for _, ref := range remoteAncestorRefs(in) {
		var k string
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(thread_key),'') FROM inbox_remote_messages WHERE inbox_id=? AND rfc_message_id=?`, inboxID, ref).Scan(&count, &k); err != nil {
			continue
		}
		if count == 1 && k != "" && k != canonical {
			if _, err := q.ExecContext(ctx, `UPDATE inbox_remote_messages SET thread_key=? WHERE inbox_id=? AND thread_key=?`, k, inboxID, canonical); err != nil {
				return err
			}
			canonical = k
			break
		}
	}
	// Fold direct replies that reference this message onto the canonical key.
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT thread_key FROM inbox_remote_messages WHERE inbox_id=? AND thread_key<>'' AND thread_key<>? AND (in_reply_to=? OR EXISTS (SELECT 1 FROM json_each(references_json) WHERE value=?))`, inboxID, canonical, msgID, msgID)
	if err != nil {
		return err
	}
	var childKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		childKeys = append(childKeys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range childKeys {
		if k == canonical {
			continue
		}
		if _, err := q.ExecContext(ctx, `UPDATE inbox_remote_messages SET thread_key=? WHERE inbox_id=? AND thread_key=?`, canonical, inboxID, k); err != nil {
			return err
		}
	}
	// Ensure the message's own rows carry the canonical key.
	if _, err := q.ExecContext(ctx, `UPDATE inbox_remote_messages SET thread_key=? WHERE inbox_id=? AND rfc_message_id=? AND thread_key<>?`, canonical, inboxID, msgID, canonical); err != nil {
		return err
	}
	return nil
}

// mergeRemoteThreadTx is mergeRemoteThread against a transaction.
func mergeRemoteThreadTx(ctx context.Context, tx *sql.Tx, inboxID string, in RemoteMessageInput) error {
	return mergeRemoteThread(ctx, tx, inboxID, in)
}

func remoteLastSegment(path string) string {
	if idx := strings.LastIndexAny(path, "/."); idx >= 0 && idx+1 < len(path) {
		return path[idx+1:]
	}
	return path
}

// RemoteMessageFilter selects cached remote messages within one inbox. An empty
// FolderPath matches every indexed folder. Label, when set, intersects the result
// with rows carrying that local label.
//
// Before is the opaque keyset cursor: a remote message's metadata id. The row's
// (received_at, id) tuple is the total ordering key (see the ORDER BY), so the
// cursor is a single unique id rather than a bare timestamp. A timestamp-only
// cursor cannot resume a page when several messages share a received_at (it
// would skip every same-second sibling), and it silently collides with a
// metadata id (the UI's own "load older" cursor), so the id is required here.
type RemoteMessageFilter struct {
	FolderPath string
	Label      string
	UnreadOnly bool
	Flagged    bool
	Limit      int
	Before     string
}

// ListRemoteMessagesFiltered lists cached remote message metadata for an inbox,
// newest first, with optional folder and label scoping. The ordering is a total
// order on (received_at DESC, id DESC); id is unique so a page boundary is never
// ambiguous.
func (s *Store) ListRemoteMessagesFiltered(ctx context.Context, accountID, inboxID string, f RemoteMessageFilter) ([]RemoteMessage, error) {
	q := `SELECT ` + remoteMessageColumns + ` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=?`
	args := []any{accountID, inboxID}
	if strings.TrimSpace(f.FolderPath) != "" {
		q += ` AND folder_path=?`
		args = append(args, f.FolderPath)
	}
	if f.UnreadOnly {
		q += ` AND is_read=0`
	}
	if f.Flagged {
		q += ` AND is_flagged=1`
	}
	if f.Before != "" {
		// Resolve the cursor to its ordering tuple. An unknown cursor is treated
		// as empty (no rows) rather than silently restarting from the newest,
		// which would loop a pager forever.
		var beforeRecv sql.NullString
		cerr := s.read.QueryRowContext(ctx, `SELECT received_at FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, f.Before).Scan(&beforeRecv)
		switch {
		case cerr == sql.ErrNoRows:
			return []RemoteMessage{}, nil
		case cerr != nil:
			return nil, cerr
		default:
			recv := beforeRecv.String
			q += ` AND (COALESCE(received_at,'') < ? OR (COALESCE(received_at,'') = ? AND id < ?))`
			args = append(args, recv, recv, f.Before)
		}
	}
	if strings.TrimSpace(f.Label) != "" {
		q += ` AND id IN (SELECT remote_message_id FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? AND label=?)`
		args = append(args, accountID, inboxID, strings.TrimSpace(f.Label))
	}
	q += ` ORDER BY COALESCE(received_at,'') DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteMessage{}
	for rows.Next() {
		m, scanErr := s.scanRemoteMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetRemoteMessageWithLabels loads a cached remote message and its local labels.
func (s *Store) GetRemoteMessageWithLabels(ctx context.Context, accountID, inboxID, id string) (RemoteMessage, error) {
	m, err := s.GetRemoteMessage(ctx, accountID, inboxID, id)
	if err != nil {
		return RemoteMessage{}, err
	}
	labels, err := s.RemoteMessageLabels(ctx, accountID, inboxID, id)
	if err != nil {
		return RemoteMessage{}, err
	}
	m.Labels = labels
	return m, nil
}

// RemoteMessageLabels returns the local labels of a cached remote message.
func (s *Store) RemoteMessageLabels(ctx context.Context, accountID, inboxID, messageID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT label FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? AND remote_message_id=? ORDER BY label`, accountID, inboxID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var label string
		if err = rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// SetRemoteMessageLabels replaces the local labels of a cached remote message.
// Labels are free-text metadata, never a folder. It returns the resulting set.
func (s *Store) SetRemoteMessageLabels(ctx context.Context, accountID, inboxID, messageID string, labels []string) ([]string, error) {
	var exists int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, messageID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	clean := normalizeRemoteLabels(labels)
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? AND remote_message_id=?`, accountID, inboxID, messageID); err != nil {
		return nil, err
	}
	now := nowText()
	for _, label := range clean {
		// A label is shared across the account's remote mailboxes only when the
		// same account/inbox pair is used; the unique key is per message+label.
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_labels(id,account_id,inbox_id,remote_message_id,label,created_at) VALUES(?,?,?,?,?,?)`,
			idgen.New("rlb"), accountID, inboxID, messageID, label, now); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return clean, nil
}

// AddRemoteMessageLabels merges labels onto a cached remote message.
func (s *Store) AddRemoteMessageLabels(ctx context.Context, accountID, inboxID, messageID string, labels []string) ([]string, error) {
	current, err := s.RemoteMessageLabels(ctx, accountID, inboxID, messageID)
	if err != nil {
		return nil, err
	}
	merged := current
	for _, l := range normalizeRemoteLabels(labels) {
		if !containsFold(merged, l) {
			merged = append(merged, l)
		}
	}
	return s.SetRemoteMessageLabels(ctx, accountID, inboxID, messageID, merged)
}

// RemoveRemoteMessageLabels removes labels from a cached remote message, returning
// the remaining set. Removing the last label is allowed: a message with no labels
// is the common case, not an error.
func (s *Store) RemoveRemoteMessageLabels(ctx context.Context, accountID, inboxID, messageID string, labels []string) ([]string, error) {
	current, err := s.RemoteMessageLabels(ctx, accountID, inboxID, messageID)
	if err != nil {
		return nil, err
	}
	drop := map[string]bool{}
	for _, l := range normalizeRemoteLabels(labels) {
		drop[strings.ToLower(l)] = true
	}
	kept := make([]string, 0, len(current))
	for _, l := range current {
		if !drop[strings.ToLower(l)] {
			kept = append(kept, l)
		}
	}
	return s.SetRemoteMessageLabels(ctx, accountID, inboxID, messageID, kept)
}

// normalizeRemoteLabels trims, de-duplicates case-insensitively and drops empties.
func normalizeRemoteLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]bool{}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" || len(l) > 128 {
			continue
		}
		key := strings.ToLower(l)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	return out
}

func containsFold(list []string, v string) bool {
	v = strings.TrimSpace(v)
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

// RemoteMessageFlags updates the flag-derived metadata of a cached remote message
// after a live flag change, so the local mirror matches what was just applied.
func (s *Store) UpdateRemoteMessageFlags(ctx context.Context, accountID, inboxID, id string, read, flagged, answered, draft bool, flags []string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inbox_remote_messages SET is_read=?,is_flagged=?,is_answered=?,is_draft=?,flags_json=?,indexed_at=?,updated_at=? WHERE account_id=? AND inbox_id=? AND id=?`,
		boolInt(read), boolInt(flagged), boolInt(answered), boolInt(draft), jsonString(flags), nowText(), nowText(), accountID, inboxID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveRemoteMessageMetadata relocates a cached remote message's metadata row to a
// new folder/UID after a move the application performed, recording the new
// locator. It is the local half of a remote move: the caller has already moved the
// message on the server and holds the destination UID (or re-resolved by
// Message-ID). It never fetches a body. Returns ErrNotFound for an unknown id.
func (s *Store) MoveRemoteMessageMetadata(ctx context.Context, accountID, inboxID, id, destFolder string, destUIDValidity, destUID uint32) (RemoteMessage, error) {
	m, err := s.GetRemoteMessage(ctx, accountID, inboxID, id)
	if err != nil {
		return RemoteMessage{}, err
	}
	if strings.TrimSpace(destFolder) == "" {
		return RemoteMessage{}, fmt.Errorf("destination folder is required")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return RemoteMessage{}, err
	}
	defer tx.Rollback()
	now := nowText()
	// Preserve local labels across the relocation: the message row's DELETE
	// cascades the label rows, so capture them first and restore afterwards.
	labels, err := s.RemoteMessageLabels(ctx, accountID, inboxID, id)
	if err != nil {
		return RemoteMessage{}, err
	}
	// Delete the old locator row (its UID no longer exists in the source folder)
	// and insert at the new locator. Labels travel with the message.
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_remote_messages WHERE id=? AND account_id=? AND inbox_id=?`, id, accountID, inboxID); err != nil {
		return RemoteMessage{}, err
	}
	// Preserve the stable metadata id so a client's cached id remains valid across
	// a move (IDs the service performs are stable).
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_messages(id,account_id,inbox_id,folder_path,remote_uid_validity,remote_uid,rfc_message_id,in_reply_to,references_json,thread_key,from_name,from_address,to_json,cc_json,subject,snippet,size_bytes,has_attachments,is_read,is_flagged,is_answered,is_draft,flags_json,received_at,sent_at,internal_date,indexed_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, accountID, inboxID, destFolder, destUIDValidity, destUID, m.RFCMessageID, m.InReplyTo, jsonString(m.References), m.ThreadKey,
		m.FromName, m.FromAddress, jsonString(m.To), jsonString(m.CC), m.Subject, m.Snippet, m.SizeBytes, boolInt(m.HasAttach), boolInt(m.Read), boolInt(m.Flagged),
		boolInt(m.Answered), boolInt(m.Draft), jsonString(m.Flags), nullStringPtr(m.ReceivedAt), nullStringPtr(m.SentAt), nullStringPtr(m.InternalDate), now, m.CreatedAt, now)
	if err != nil {
		return RemoteMessage{}, err
	}
	for _, label := range labels {
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_labels(id,account_id,inbox_id,remote_message_id,label,created_at) VALUES(?,?,?,?,?,?)`,
			idgen.New("rlb"), accountID, inboxID, m.ID, label, now); err != nil {
			return RemoteMessage{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return RemoteMessage{}, err
	}
	return s.GetRemoteMessageWithLabels(ctx, accountID, inboxID, m.ID)
}

// RemoteThread is a thread in the cached remote index: its stable key and the
// aggregate of its messages. Threads are account- and inbox-scoped.
type RemoteThread struct {
	Key          string
	InboxID      string
	Subject      string
	MessageCount int
	UnreadCount  int
	LatestAt     *string
}

// ListRemoteThreads lists the cached remote threads of an inbox, newest first,
// scoped to the account and inbox. An empty folderPath aggregates across every
// indexed folder (a thread may span folders); a non-empty folderPath restricts to
// messages currently in that folder. beforeKey, when non-empty, is a keyset
// cursor: a thread's stable key, resuming strictly after its
// (latest_received_at, thread_key) ordering tuple. The ordering is a total order
// on (latest DESC, thread_key DESC), so a page boundary is never ambiguous.
func (s *Store) ListRemoteThreads(ctx context.Context, accountID, inboxID, folderPath string, limit int, beforeKey string) ([]RemoteThread, error) {
	q := `SELECT thread_key, inb_id, COALESCE(subject,''), count(*), sum(CASE WHEN is_read=0 THEN 1 ELSE 0 END), max(COALESCE(received_at,'')) FROM (
		SELECT thread_key, inbox_id AS inb_id, subject, is_read, received_at FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND thread_key<>''`
	args := []any{accountID, inboxID}
	if strings.TrimSpace(folderPath) != "" {
		q += ` AND folder_path=?`
		args = append(args, folderPath)
	}
	q += `) GROUP BY thread_key, inb_id`
	if strings.TrimSpace(beforeKey) != "" {
		// Resolve the cursor key to its (latest, thread_key) tuple. Grouped
		// aggregates cannot reference a scalar subquery cleanly, so resolve the
		// tuple in Go first and compare the aggregate against it.
		var latest sql.NullString
		cerr := s.read.QueryRowContext(ctx, `SELECT MAX(COALESCE(received_at,'')) FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND thread_key=?`, accountID, inboxID, beforeKey).Scan(&latest)
		if cerr != nil {
			return nil, cerr
		}
		cur := latest.String
		q += ` HAVING (max(COALESCE(received_at,'')) < ? OR (max(COALESCE(received_at,'')) = ? AND thread_key < ?))`
		args = append(args, cur, cur, beforeKey)
	}
	q += ` ORDER BY max(COALESCE(received_at,'')) DESC, thread_key DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteThread{}
	for rows.Next() {
		var t RemoteThread
		var latest string
		if err = rows.Scan(&t.Key, &t.InboxID, &t.Subject, &t.MessageCount, &t.UnreadCount, &latest); err != nil {
			return nil, err
		}
		if latest != "" {
			t.LatestAt = &latest
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListRemoteThreadMessages returns the cached messages of a thread, oldest first,
// scoped to the account and inbox.
func (s *Store) ListRemoteThreadMessages(ctx context.Context, accountID, inboxID, threadKey string) ([]RemoteMessage, error) {
	threadKey = strings.TrimSpace(threadKey)
	if threadKey == "" {
		return []RemoteMessage{}, nil
	}
	rows, err := s.read.QueryContext(ctx, `SELECT `+remoteMessageColumns+` FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? AND thread_key=? ORDER BY COALESCE(received_at,'') ASC, remote_uid ASC`, accountID, inboxID, threadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RemoteMessage{}
	for rows.Next() {
		m, scanErr := s.scanRemoteMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountRemoteFolderMessages returns the per-folder message and unread counts of the
// cached remote index, so a folder listing can show counts without a live status
// round-trip.
func (s *Store) CountRemoteFolderMessages(ctx context.Context, accountID, inboxID string) (map[string][2]int, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT folder_path, count(*), sum(CASE WHEN is_read=0 THEN 1 ELSE 0 END) FROM inbox_remote_messages WHERE account_id=? AND inbox_id=? GROUP BY folder_path`, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var path string
		var total, unread int
		if err = rows.Scan(&path, &total, &unread); err != nil {
			return nil, err
		}
		out[path] = [2]int{total, unread}
	}
	return out, rows.Err()
}

// RemoteLabelsForInbox returns every distinct local label in use across an inbox's
// cached remote messages, ordered case-insensitively.
func (s *Store) RemoteLabelsForInbox(ctx context.Context, accountID, inboxID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT label FROM inbox_remote_labels WHERE account_id=? AND inbox_id=? ORDER BY label COLLATE NOCASE`, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var label string
		if err = rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// ResetRemoteInboxState drops every cached remote artefact of a standalone inbox:
// the header/thread index, local labels, folders, detection cursors, arrivals,
// notification baselines and auto-action state. It is called when the inbox is
// rebound to a different mailbox (host, login or sync root), because a UID or
// Message-ID from the old mailbox may coincide with an unrelated message in the
// new one. The index status returns to never_started so the next read re-indexes
// from scratch. Pending outbound jobs are intentionally not touched here.
func (s *Store) ResetRemoteInboxState(ctx context.Context, accountID, inboxID string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`DELETE FROM inbox_remote_actions WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_remote_arrivals WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_remote_notifications WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_remote_cursors WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_remote_labels WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_remote_messages WHERE account_id=? AND inbox_id=?`,
		`DELETE FROM inbox_folders WHERE account_id=? AND inbox_id=? AND origin='remote'`,
	}
	for _, q := range stmts {
		if _, err = tx.ExecContext(ctx, q, accountID, inboxID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET remote_index_status='',remote_index_error='',remote_indexed_at='' WHERE id=? AND account_id=?`, inboxID, accountID); err != nil {
		return err
	}
	return tx.Commit()
}
