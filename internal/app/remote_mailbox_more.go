package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

// RemoteThreadView is a thread in the cached remote index.
type RemoteThreadView struct {
	Key           string    `json:"id"`
	InboxID       string    `json:"inbox_id"`
	Subject       string    `json:"subject"`
	MessageCount  int       `json:"message_count"`
	UnreadCount   int       `json:"unread_count"`
	LastMessageAt time.Time `json:"last_message_at"`
}

// ListRemoteThreads returns the cached remote threads of an inbox, newest first.
// Threads are account- and inbox-scoped. It reconciles on demand when the index
// has never been built, so the first call after setup returns real threads.
func (m *RemoteMailboxService) ListRemoteThreads(ctx context.Context, p model.Principal, inboxID, folderPath string, limit int, beforeKey string) ([]RemoteThreadView, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(folderPath) != "" && !m.folderInScope(inbox, folderPath) {
		return nil, model.NewMailboxError(model.ErrKindNotFound, "folder is not in this mailbox's scope", false, nil)
	}
	if err := m.ensureIndexed(ctx, p.AccountID, inboxID); err != nil {
		return nil, err
	}
	threads, err := m.Service.Store.ListRemoteThreads(ctx, p.AccountID, inboxID, folderPath, limit, beforeKey)
	if err != nil {
		return nil, mapStoreError(err)
	}
	out := make([]RemoteThreadView, 0, len(threads))
	for _, t := range threads {
		v := RemoteThreadView{Key: t.Key, InboxID: t.InboxID, Subject: t.Subject, MessageCount: t.MessageCount, UnreadCount: t.UnreadCount}
		if t.LatestAt != nil {
			v.LastMessageAt = parseStoreTime(*t.LatestAt)
		}
		out = append(out, v)
	}
	return out, nil
}

// GetRemoteThread returns a thread's cached messages, oldest first, scoped to the
// account and inbox.
func (m *RemoteMailboxService) GetRemoteThread(ctx context.Context, p model.Principal, inboxID, threadKey string) (RemoteThreadView, []RemoteMessageView, error) {
	if _, err := m.authorizeRead(ctx, p, inboxID); err != nil {
		return RemoteThreadView{}, nil, err
	}
	if err := m.ensureIndexed(ctx, p.AccountID, inboxID); err != nil {
		return RemoteThreadView{}, nil, err
	}
	msgs, err := m.Service.Store.ListRemoteThreadMessages(ctx, p.AccountID, inboxID, threadKey)
	if err != nil {
		return RemoteThreadView{}, nil, mapStoreError(err)
	}
	if len(msgs) == 0 {
		return RemoteThreadView{}, nil, model.NewMailboxError(model.ErrKindNotFound, "thread not found", false, nil)
	}
	view := RemoteThreadView{Key: threadKey, InboxID: inboxID, Subject: msgs[len(msgs)-1].Subject}
	views := make([]RemoteMessageView, 0, len(msgs))
	unread := 0
	for _, msg := range msgs {
		if !msg.Read {
			unread++
		}
		v := remoteView(msg)
		labels, lerr := m.Service.Store.RemoteMessageLabels(ctx, p.AccountID, inboxID, msg.ID)
		if lerr == nil {
			v.Labels = labels
		}
		views = append(views, v)
	}
	view.MessageCount = len(msgs)
	view.UnreadCount = unread
	if msgs[len(msgs)-1].ReceivedAt != nil {
		view.LastMessageAt = parseStoreTime(*msgs[len(msgs)-1].ReceivedAt)
	}
	return view, views, nil
}

// ensureIndexed reconciles the remote index once if it has never been built.
func (m *RemoteMailboxService) ensureIndexed(ctx context.Context, accountID, inboxID string) error {
	status, err := m.Service.Store.GetRemoteIndexStatus(ctx, accountID, inboxID)
	if err != nil {
		return mapStoreError(err)
	}
	if status.Status == store.RemoteIndexNeverStarted || status.Status == "" {
		if _, rerr := m.ReconcileRemote(ctx, accountID, inboxID); rerr != nil {
			return rerr
		}
	}
	return nil
}

// RemoteSearchQuery is a provider-neutral search over a standalone inbox's remote
// messages. Filters map onto the adapter's SearchQuery; Label is intersected
// locally. Only the common filters are exposed; the adapter supports more.
type RemoteSearchQuery struct {
	FolderPath string
	From       string
	To         string
	Subject    string
	Text       string
	Since      time.Time
	Before     time.Time
	Unread     *bool
	Flagged    *bool
	Label      string
	Limit      int
	// Cursor is the opaque pagination token from a prior page (the lowest UID of
	// the prior, newest-first page). A zero cursor starts at the newest match.
	Cursor uint32
}

// SearchRemote runs a live search on the remote server and intersects the result
// with the local metadata cache and any local label filter. The result carries the
// cached message views for the matching UIDs, so a caller sees headers without a
// second round-trip. Pagination beyond the limit is offered through the adapter's
// NextCursor via NextCursor.
type RemoteSearchResult struct {
	Items        []RemoteMessageView    `json:"items"`
	NextCursor   uint32                 `json:"next_cursor,omitempty"`
	Completeness model.ListCompleteness `json:"completeness"`
}

const remoteSearchDefaultLimit = 200

// SearchRemote performs a live server search restricted to one folder in scope.
func (m *RemoteMailboxService) SearchRemote(ctx context.Context, p model.Principal, inboxID string, q RemoteSearchQuery) (RemoteSearchResult, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return RemoteSearchResult{}, err
	}
	folderPath := strings.TrimSpace(q.FolderPath)
	if folderPath == "" {
		folderPath = inbox.Namespace
		if folderPath == "" {
			folderPath = model.NamespaceINBOX
		}
	}
	if !m.folderInScope(inbox, folderPath) {
		return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindNotFound, "folder is not in this mailbox's scope", false, nil)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return RemoteSearchResult{}, err
	}
	defer sess.Close()
	limit := q.Limit
	if limit <= 0 {
		limit = remoteSearchDefaultLimit
	}
	search := imap.SearchQuery{
		From:        q.From,
		To:          q.To,
		Subject:     q.Subject,
		Text:        q.Text,
		Since:       q.Since,
		Before:      q.Before,
		Seen:        invertBoolPtr(q.Unread),
		Limit:       limit,
		NewestFirst: true,
		BeforeUID:   q.Cursor,
	}
	if q.Flagged != nil {
		if *q.Flagged {
			search.Flags = append(search.Flags, imap.FlagFlagged)
		} else {
			search.NotFlags = append(search.NotFlags, imap.FlagFlagged)
		}
	}
	res, err := sess.Search(ctx, folderPath, search)
	if err != nil {
		return RemoteSearchResult{}, normalizeRemoteError(err)
	}
	// Map the returned UIDs to their cached metadata. A UID with no cached row is
	// fetched live in one header pass so a newly arrived message is still visible.
	views, err := m.viewsForUIDs(ctx, p.AccountID, inboxID, folderPath, res.UIDs, q.Label, sess)
	if err != nil {
		return RemoteSearchResult{}, err
	}
	out := RemoteSearchResult{Items: views, NextCursor: res.NextCursor}
	switch res.Completeness {
	case imap.CompletenessComplete:
		out.Completeness = model.CompletenessComplete
	case imap.CompletenessPartial:
		out.Completeness = model.CompletenessPartial
	default:
		out.Completeness = model.CompletenessUnknown
	}
	if out.Items == nil {
		out.Items = []RemoteMessageView{}
	}
	return out, nil
}

// invertBoolPtr maps an "unread" filter onto the adapter's "seen" filter: unread
// true means seen=false. A nil filter is left unset.
func invertBoolPtr(unread *bool) *bool {
	if unread == nil {
		return nil
	}
	seen := !*unread
	return &seen
}

// viewsForUIDs resolves a live UID result set into message views, using the cache
// where present and a single header fetch for the rest. When label is set, only
// rows carrying that local label are returned (the local-label intersection).
func (m *RemoteMailboxService) viewsForUIDs(ctx context.Context, accountID, inboxID, folderPath string, uids []uint32, label string, sess RemoteSession) ([]RemoteMessageView, error) {
	if len(uids) == 0 {
		return []RemoteMessageView{}, nil
	}
	byUID := map[uint32]store.RemoteMessage{}
	var missing []uint32
	for _, uid := range uids {
		rec, err := m.Service.Store.GetRemoteMessageByUID(ctx, accountID, inboxID, folderPath, 0, uid)
		if err == nil {
			byUID[uid] = rec
			continue
		}
		if errors.Is(err, store.ErrNotFound) {
			missing = append(missing, uid)
			continue
		}
		return nil, mapStoreError(err)
	}
	if len(missing) > 0 {
		headers, uidValidity, herr := sess.ListHeaders(ctx, folderPath, missing, len(missing))
		if herr != nil {
			return nil, normalizeRemoteError(herr)
		}
		for _, h := range headers {
			updated, uerr := m.Service.Store.UpsertRemoteMessage(ctx, accountID, inboxID, remoteMessageInputFromHeader(h))
			if uerr == nil {
				byUID[h.UID] = updated
			}
		}
		_ = uidValidity
	}
	out := make([]RemoteMessageView, 0, len(uids))
	for _, uid := range uids {
		rec, ok := byUID[uid]
		if !ok {
			continue
		}
		if strings.TrimSpace(label) != "" {
			labels, lerr := m.Service.Store.RemoteMessageLabels(ctx, accountID, inboxID, rec.ID)
			if lerr != nil {
				return nil, mapStoreError(lerr)
			}
			if !containsFoldApp(labels, label) {
				continue
			}
			rec.Labels = labels
		}
		out = append(out, remoteView(rec))
	}
	return out, nil
}

func containsFoldApp(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

// SetRemoteLabels replaces the local labels of a cached remote message. Labels are
// local metadata, independent of folders, and never require a provider call.
func (m *RemoteMailboxService) SetRemoteLabels(ctx context.Context, p model.Principal, inboxID, messageID string, labels []string) (RemoteMessageView, error) {
	if _, err := m.authorizeAssist(ctx, p, inboxID); err != nil {
		return RemoteMessageView{}, err
	}
	if _, err := m.Service.Store.SetRemoteMessageLabels(ctx, p.AccountID, inboxID, messageID, labels); err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, messageID)
}

// AddRemoteLabels merges labels onto a cached remote message.
func (m *RemoteMailboxService) AddRemoteLabels(ctx context.Context, p model.Principal, inboxID, messageID string, labels []string) (RemoteMessageView, error) {
	if _, err := m.authorizeAssist(ctx, p, inboxID); err != nil {
		return RemoteMessageView{}, err
	}
	if _, err := m.Service.Store.AddRemoteMessageLabels(ctx, p.AccountID, inboxID, messageID, labels); err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, messageID)
}

// RemoveRemoteLabels removes labels from a cached remote message.
func (m *RemoteMailboxService) RemoveRemoteLabels(ctx context.Context, p model.Principal, inboxID, messageID string, labels []string) (RemoteMessageView, error) {
	if _, err := m.authorizeAssist(ctx, p, inboxID); err != nil {
		return RemoteMessageView{}, err
	}
	if _, err := m.Service.Store.RemoveRemoteMessageLabels(ctx, p.AccountID, inboxID, messageID, labels); err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, messageID)
}

// ListRemoteLabels returns every distinct local label used in an inbox's cached
// remote messages.
func (m *RemoteMailboxService) ListRemoteLabels(ctx context.Context, p model.Principal, inboxID string) ([]string, error) {
	if _, err := m.authorizeRead(ctx, p, inboxID); err != nil {
		return nil, err
	}
	rows, err := m.Service.Store.RemoteLabelsForInbox(ctx, p.AccountID, inboxID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return rows, nil
}

// CreateRemoteFolder creates a folder on the live server and mirrors it into the
// cached index. The new folder is within the inbox's selected root scope.
func (m *RemoteMailboxService) CreateRemoteFolder(ctx context.Context, p model.Principal, inboxID, path string, name string) (model.Folder, error) {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return model.Folder{}, model.NewMailboxError(model.ErrKindInvalid, "folder path is required", false, nil)
	}
	if !m.folderInScopeCreate(inbox, path) {
		return model.Folder{}, model.NewMailboxError(model.ErrKindInvalid, "folder is not in this mailbox's scope", false, nil)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	defer sess.Close()
	if err := sess.CreateFolder(ctx, path); err != nil {
		return model.Folder{}, normalizeRemoteError(err)
	}
	// Mirror the new folder locally so it is immediately addressable.
	display := strings.TrimSpace(name)
	if display == "" {
		display = lastRemoteSegment(path)
	}
	parent := ""
	delim := m.scopeDelimiter(inbox)
	if delim != 0 {
		parent = parentOf(path, delim)
	}
	folders, err := m.Service.Store.ReconcileRemoteFolders(ctx, p.AccountID, inboxID, []store.RemoteFolderSync{{
		Path: path, Name: display, ParentPath: parent, Role: model.FolderRoleFolder, Selectable: true, IndexedAt: time.Now().UTC(),
	}})
	if err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	for _, f := range folders {
		if f.Path == path {
			return f, nil
		}
	}
	return model.Folder{InboxID: inboxID, Path: path, Name: display, Role: model.FolderRoleFolder, Selectable: true}, nil
}

// RenameRemoteFolder renames a remote folder on the live server and re-mirrors the
// folder tree. A local custom folder (origin='local') is renamed locally without a
// provider call. A protected system folder cannot be renamed.
func (m *RemoteMailboxService) RenameRemoteFolder(ctx context.Context, p model.Principal, inboxID, folderID, newName string) (model.Folder, error) {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	folders, err := m.Service.Store.ListFolders(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	var target *model.Folder
	for i := range folders {
		if folders[i].ID == folderID {
			target = &folders[i]
			break
		}
	}
	if target == nil {
		return model.Folder{}, model.NewMailboxError(model.ErrKindNotFound, "folder not found", false, store.ErrNotFound)
	}
	if target.IsSystem {
		return model.Folder{}, model.NewMailboxError(model.ErrKindForbidden, "system folder cannot be renamed", false, store.ErrFolderProtected)
	}
	newName = strings.TrimSpace(newName)
	if newName == "" || strings.ContainsAny(newName, "/\\\t\r\n\x00") {
		return model.Folder{}, model.NewMailboxError(model.ErrKindInvalid, "invalid folder name", false, nil)
	}
	// A remote-owned folder is renamed on the provider (its path changes); a
	// local-owned folder is renamed locally. A remote folder rename changes the
	// path and the server moves any children with the parent.
	if target.IsSystem {
		return model.Folder{}, model.NewMailboxError(model.ErrKindForbidden, "folder role is protected", false, store.ErrFolderProtected)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	defer sess.Close()
	delim := m.scopeDelimiter(inbox)
	oldPath := target.Path
	var newPath string
	if delim != 0 {
		newPath = joinFolderPath(parentOf(oldPath, delim), newName, delim)
	} else {
		newPath = newName
	}
	if !m.folderInScope(inbox, newPath) {
		return model.Folder{}, model.NewMailboxError(model.ErrKindInvalid, "renamed folder would leave the mailbox scope", false, nil)
	}
	if err := sess.RenameFolder(ctx, oldPath, newPath); err != nil {
		return model.Folder{}, normalizeRemoteError(err)
	}
	// Re-mirror: drop the old path and index the new one. A reconcile will pick
	// up children whose paths the server moved with the parent.
	if _, err := m.Service.Store.ReconcileRemoteFolders(ctx, p.AccountID, inboxID, []store.RemoteFolderSync{{
		Path: newPath, Name: newName, ParentPath: parentOf(newPath, delim), Role: model.FolderRoleFolder, Selectable: true, IndexedAt: time.Now().UTC(),
	}}); err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	if _, err := m.ReconcileRemote(ctx, p.AccountID, inboxID); err != nil {
		return model.Folder{}, err
	}
	got, err := m.Service.Store.GetFolder(ctx, p.AccountID, inboxID, newPath)
	if err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	return got, nil
}

// DeleteRemoteFolder deletes a remote folder on the live server. The server
// refuses a non-empty folder; the adapter reports that as a conflict, which the
// caller surfaces rather than cascading a delete.
func (m *RemoteMailboxService) DeleteRemoteFolder(ctx context.Context, p model.Principal, inboxID, folderID string) error {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return err
	}
	folders, err := m.Service.Store.ListFolders(ctx, p.AccountID, inboxID)
	if err != nil {
		return mapStoreError(err)
	}
	var target *model.Folder
	for i := range folders {
		if folders[i].ID == folderID {
			target = &folders[i]
			break
		}
	}
	if target == nil {
		return model.NewMailboxError(model.ErrKindNotFound, "folder not found", false, store.ErrNotFound)
	}
	if target.IsSystem {
		return model.NewMailboxError(model.ErrKindForbidden, "system folder cannot be deleted", false, store.ErrFolderProtected)
	}
	if !m.folderInScope(inbox, target.Path) {
		return model.NewMailboxError(model.ErrKindNotFound, "folder is not in this mailbox's scope", false, nil)
	}
	// Refuse to delete a non-empty folder before touching the provider: a cached
	// child folder, or any cached message still in this folder, makes it non-empty.
	// The emptiness test is explicit and never cascades; the server also refuses a
	// non-empty delete, but checking first avoids a destructive round-trip and gives
	// a stable, provider-independent error.
	for _, f := range folders {
		if f.ParentPath == target.Path {
			return model.NewMailboxError(model.ErrKindConflict, "folder has child folders and is not empty", false, store.ErrFolderNotEmpty)
		}
	}
	if cached, lerr := m.Service.Store.ListRemoteMessagesFiltered(ctx, p.AccountID, inboxID, store.RemoteMessageFilter{FolderPath: target.Path, Limit: 1}); lerr == nil && len(cached) > 0 {
		return model.NewMailboxError(model.ErrKindConflict, "folder still contains messages and is not empty", false, store.ErrFolderNotEmpty)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return err
	}
	defer sess.Close()
	if err := sess.DeleteFolder(ctx, target.Path); err != nil {
		if errors.Is(err, imap.ErrFolderNotEmpty) {
			return model.NewMailboxError(model.ErrKindConflict, "folder is not empty", false, store.ErrFolderNotEmpty)
		}
		return normalizeRemoteError(err)
	}
	// Drop the deleted folder's cached rows so a stale folder does not linger
	// until the next reconcile prunes it.
	if perr := m.Service.Store.DeleteRemoteFolderRow(ctx, p.AccountID, inboxID, target.Path); perr != nil {
		m.Service.Log.Warn("prune deleted remote folder", "inbox_id", inboxID, "error", perr)
	}
	return nil
}

// SetRemoteFolderRole explicitly maps an existing folder of a standalone inbox to
// a mailbox role, persisting the mapping so a later reconcile never resets it by
// re-inferring the role from the folder name. The folder is addressed by its
// stable id; the caller must hold Owner on the inbox. The role must be a known
// FolderRole*. A custom/remote folder may be mapped to a special role (for
// example an arbitrarily-named "Old Mail" folder mapped to Trash); a seeded
// system folder's role is protected. It performs no provider call: the role is
// local metadata over a folder the server already has.
func (m *RemoteMailboxService) SetRemoteFolderRole(ctx context.Context, p model.Principal, inboxID, folderID, role string) (model.Folder, error) {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return model.Folder{}, err
	}
	if !p.CanOwn(inboxID) && !p.Admin {
		return model.Folder{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	// The folder must already exist in this inbox's tree before it can be mapped:
	// a role is asserted for a real folder, never invented for a missing one.
	folders, err := m.Service.Store.ListFolders(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	var target *model.Folder
	for i := range folders {
		if folders[i].ID == folderID {
			target = &folders[i]
			break
		}
	}
	if target == nil {
		return model.Folder{}, model.NewMailboxError(model.ErrKindNotFound, "folder not found", false, store.ErrNotFound)
	}
	// A remote-scope folder must still be within the selected root; a local folder
	// (origin='local' custom) is always in scope.
	if target.Path != "" && !m.folderInScope(inbox, target.Path) && target.Origin != "local" {
		return model.Folder{}, model.NewMailboxError(model.ErrKindNotFound, "folder is not in this mailbox's scope", false, nil)
	}
	updated, err := m.Service.Store.SetFolderRole(ctx, p, inboxID, folderID, role)
	if err != nil {
		return model.Folder{}, mapStoreError(err)
	}
	return updated, nil
}

// scopeDelimiter resolves the delimiter for an inbox's scope. It prefers the
// discovered folders' delimiter; when none is known it falls back to "/", the
// conventional IMAP delimiter. The adapter reconciles the real one on the next
// discover.
func (m *RemoteMailboxService) scopeDelimiter(inbox model.Inbox) rune {
	// The store does not persist the delimiter, so use the conventional '/'. A
	// path is always the literal IMAP path; the delimiter only affects the
	// derived parent/child segments.
	return '/'
}

// folderInScopeCreate reports whether a new folder path would be in scope. A
// not-yet-created folder is allowed when it is already in scope, when its parent
// folder is in scope (creating a child), or when it is a new top-level personal
// folder (a sibling of INBOX) under the default root. A custom explicit root is
// handled by folderInScope's prefix rule.
func (m *RemoteMailboxService) folderInScopeCreate(inbox model.Inbox, path string) bool {
	if m.folderInScope(inbox, path) {
		return true
	}
	path = strings.TrimSpace(path)
	root := strings.TrimSpace(inbox.Namespace)
	if root == "" || strings.EqualFold(root, model.NamespaceINBOX) {
		if parent := parentOf(path, '/'); parent != "" {
			return m.folderInScope(inbox, parent)
		}
		// A top-level folder (sibling of INBOX) is an explicit personal choice.
		return !strings.ContainsAny(path, "./")
	}
	return false
}

func joinFolderPath(parent, name string, delim rune) string {
	parent = strings.Trim(strings.TrimSpace(parent), string(delim))
	name = strings.Trim(strings.TrimSpace(name), string(delim))
	if parent == "" {
		return name
	}
	return parent + string(delim) + name
}

// lastRemoteSegment returns the final path segment of a folder path.
func lastRemoteSegment(path string) string {
	if idx := strings.LastIndexAny(path, "/."); idx >= 0 && idx+1 < len(path) {
		return path[idx+1:]
	}
	return path
}

// ---- RemoteDraft handoff publisher ----

// RemoteHandoffPublisher implements HandoffPublisher against the live remote
// server: it appends the frozen draft to the inbox's remote Drafts folder and
// verifies the append by the handoff correlation header and Message-ID. It is
// installed onto the app Service by cmd/server; tests inject a fake.
type RemoteHandoffPublisher struct {
	Service *RemoteMailboxService
}

var _ HandoffPublisher = (*RemoteHandoffPublisher)(nil)

// Append appends raw MIME to the inbox's remote Drafts folder. The Drafts folder
// is resolved from the inbox's stored Sent/Drafts role folder rather than a fixed
// name, so a provider-specific folder name still works. It returns Confirmed when
// the server reported an APPENDUID; otherwise the caller verifies by Lookup.
func (p *RemoteHandoffPublisher) Append(ctx context.Context, inboxID string, raw []byte, messageID, handoffID string) (HandoffOutcome, error) {
	accountID, err := p.Service.Service.Store.InboxAccountID(ctx, inboxID)
	if err != nil {
		return HandoffOutcome{}, mapStoreError(err)
	}
	inbox, err := p.Service.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return HandoffOutcome{}, mapStoreError(err)
	}
	folder, ferr := p.Service.Service.Store.GetSystemFolder(ctx, accountID, inboxID, model.FolderRoleDrafts)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return HandoffOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote Drafts folder", false, store.ErrHandoffUnsupported)
	}
	if !p.Service.folderInScope(inbox, folder.Path) {
		return HandoffOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "the Drafts folder is outside the mailbox scope", false, store.ErrHandoffUnsupported)
	}
	sess, _, err := p.Service.open(ctx, accountID, inboxID)
	if err != nil {
		return HandoffOutcome{}, err
	}
	defer sess.Close()
	res, err := sess.AppendReader(ctx, folder.Path, bytes.NewReader(raw), int64(len(raw)), []string{imap.FlagDraft, imap.FlagSeen}, time.Now().UTC())
	if err != nil {
		return HandoffOutcome{}, normalizeRemoteError(err)
	}
	out := HandoffOutcome{RemoteFolder: folder.Path}
	if res.Confirmed && res.DestinationUID != 0 {
		out.Confirmed = true
		out.RemoteUID = res.DestinationUID
	}
	return out, nil
}

// Lookup searches the inbox's remote Drafts folder for a previously appended handoff
// by its handoff correlation header, falling back to the Message-ID. It returns
// Confirmed only when exactly one match is found, so an ambiguous result is never
// reported as success.
func (p *RemoteHandoffPublisher) Lookup(ctx context.Context, inboxID, handoffID, messageID string) (HandoffOutcome, error) {
	accountID, err := p.Service.Service.Store.InboxAccountID(ctx, inboxID)
	if err != nil {
		return HandoffOutcome{}, mapStoreError(err)
	}
	folder, ferr := p.Service.Service.Store.GetSystemFolder(ctx, accountID, inboxID, model.FolderRoleDrafts)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return HandoffOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote Drafts folder", false, store.ErrHandoffUnsupported)
	}
	sess, _, err := p.Service.open(ctx, accountID, inboxID)
	if err != nil {
		return HandoffOutcome{}, err
	}
	defer sess.Close()
	if strings.TrimSpace(handoffID) != "" {
		locs, lerr := sess.FindByHeader(ctx, folder.Path, model.HandoffHeader, handoffID)
		if lerr != nil {
			return HandoffOutcome{}, normalizeRemoteError(lerr)
		}
		if len(locs) == 1 {
			return HandoffOutcome{Confirmed: true, RemoteUID: locs[0].UID, RemoteFolder: folder.Path}, nil
		}
		if len(locs) > 1 {
			return HandoffOutcome{RemoteFolder: folder.Path}, nil
		}
	}
	if strings.TrimSpace(messageID) != "" {
		loc, lerr := sess.FindByMessageID(ctx, folder.Path, messageID)
		if lerr == nil {
			return HandoffOutcome{Confirmed: true, RemoteUID: loc.UID, RemoteFolder: folder.Path}, nil
		}
		var mb *model.MailboxError
		if errors.As(lerr, &mb) && mb.Kind == model.ErrKindConflict {
			// More than one match: ambiguous, not confirmed.
			return HandoffOutcome{RemoteFolder: folder.Path}, nil
		}
		if !errors.Is(lerr, imap.ErrNotFound) {
			return HandoffOutcome{}, normalizeRemoteError(lerr)
		}
	}
	return HandoffOutcome{RemoteFolder: folder.Path}, nil
}

// ---- Remote Sent-copy publisher ----

// RemoteSentCopyPublisher copies a sent message into the inbox's remote Sent
// folder. It is separate from SMTP: it is invoked by the copy worker after a send
// has committed and never triggers a resend.
type RemoteSentCopyPublisher struct {
	Service *RemoteMailboxService
}

// RemoteCopyOutcome is the result of a sent-copy append.
type RemoteCopyOutcome struct {
	Confirmed    bool
	RemoteUID    uint32
	UIDValidity  uint32
	RemoteFolder string
}

// Copy appends the frozen raw MIME to the inbox's remote Sent folder. When the
// inbox has no Sent folder the copy is reported unsupported (the caller marks the
// job failed, never re-sending). folderOverride, when non-empty, names an explicit
// destination folder path (the inbox's configured sent-copy folder); otherwise the
// inbox's Sent-role folder is resolved.
func (p *RemoteSentCopyPublisher) Copy(ctx context.Context, accountID, inboxID string, raw []byte, messageID, folderOverride string) (RemoteCopyOutcome, error) {
	inbox, err := p.Service.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return RemoteCopyOutcome{}, mapStoreError(err)
	}
	folderPath := strings.TrimSpace(folderOverride)
	if folderPath == "" {
		folder, ferr := p.Service.Service.Store.GetSystemFolder(ctx, accountID, inboxID, model.FolderRoleSent)
		if ferr != nil || strings.TrimSpace(folder.Path) == "" {
			return RemoteCopyOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote Sent folder", false, ErrNoSentFolderApp)
		}
		folderPath = folder.Path
	}
	if !p.Service.folderInScope(inbox, folderPath) {
		return RemoteCopyOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "the Sent folder is outside the mailbox scope", false, ErrNoSentFolderApp)
	}
	sess, _, err := p.Service.open(ctx, accountID, inboxID)
	if err != nil {
		return RemoteCopyOutcome{}, err
	}
	defer sess.Close()
	res, err := sess.AppendReader(ctx, folderPath, bytes.NewReader(raw), int64(len(raw)), []string{imap.FlagSeen}, time.Now().UTC())
	if err != nil {
		return RemoteCopyOutcome{}, normalizeRemoteError(err)
	}
	out := RemoteCopyOutcome{RemoteFolder: folderPath, UIDValidity: res.UIDValidity}
	if res.Confirmed && res.DestinationUID != 0 {
		out.Confirmed = true
		out.RemoteUID = res.DestinationUID
	}
	return out, nil
}

// FindCopiedByMessageID verifies a sent copy by Message-ID after an unconfirmed
// append returned no APPENDUID, so the worker can settle Copied vs Ambiguous.
func (p *RemoteSentCopyPublisher) FindCopiedByMessageID(ctx context.Context, accountID, inboxID, messageID string) (RemoteCopyOutcome, error) {
	folder, ferr := p.Service.Service.Store.GetSystemFolder(ctx, accountID, inboxID, model.FolderRoleSent)
	if ferr != nil || strings.TrimSpace(folder.Path) == "" {
		return RemoteCopyOutcome{}, model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote Sent folder", false, ErrNoSentFolderApp)
	}
	sess, _, err := p.Service.open(ctx, accountID, inboxID)
	if err != nil {
		return RemoteCopyOutcome{}, err
	}
	defer sess.Close()
	loc, lerr := sess.FindByMessageID(ctx, folder.Path, messageID)
	if lerr != nil {
		var mb *model.MailboxError
		if errors.As(lerr, &mb) && mb.Kind == model.ErrKindConflict {
			return RemoteCopyOutcome{RemoteFolder: folder.Path}, nil
		}
		return RemoteCopyOutcome{}, nil
	}
	return RemoteCopyOutcome{Confirmed: true, RemoteUID: loc.UID, UIDValidity: loc.UIDValidity, RemoteFolder: folder.Path}, nil
}

// ErrNoSentFolderApp mirrors store.ErrNoSentFolder for the app layer's terminal
// classification.
var ErrNoSentFolderApp = errors.New("standalone inbox has no remote Sent folder")

// CopyPendingSentCopies drives the durable sent-copy queue: it appends each due
// frozen message to the inbox's remote Sent folder and settles the job state. It
// NEVER re-runs an SMTP send; a copy failure is confined to the copy job. It is
// called by the outbox worker.
func (m *RemoteMailboxService) CopyPendingSentCopies(ctx context.Context) {
	pub := &RemoteSentCopyPublisher{Service: m}
	attempted := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, ok, err := m.Service.Store.ClaimNextRemoteSentCopy(ctx, time.Now().UTC(), "sent-copy", 2*time.Minute)
		if err != nil {
			m.Service.Log.Error("sent-copy claim", "error", err)
			return
		}
		if !ok {
			return
		}
		if attempted[job.ID] {
			return
		}
		attempted[job.ID] = true
		m.copyOneSentCopy(ctx, pub, job)
	}
}

func (m *RemoteMailboxService) copyOneSentCopy(ctx context.Context, pub *RemoteSentCopyPublisher, job store.RemoteSentCopy) {
	raw, err := readFrozenRaw(m.Service, job.RawPath)
	if err != nil {
		// The frozen file is gone: verify by Message-ID and settle, never retry
		// the append with different bytes.
		m.verifySentCopyByLookup(ctx, pub, job)
		return
	}
	// Prefer the job's recorded folder (frozen at enqueue) then the inbox's
	// configured sent-copy folder; otherwise the Sent-role folder is resolved.
	override := strings.TrimSpace(job.FolderPath)
	if override == "" {
		if inbox, ierr := m.Service.Store.GetInboxInternal(ctx, job.AccountID, job.InboxID); ierr == nil {
			override = strings.TrimSpace(inbox.RemoteSentCopyFolder)
		}
	}
	outcome, err := pub.Copy(ctx, job.AccountID, job.InboxID, raw, job.RFCMessageID, override)
	if err != nil {
		m.settleSentCopyError(ctx, job, err)
		return
	}
	if !outcome.Confirmed {
		m.verifySentCopyByLookup(ctx, pub, job)
		return
	}
	if err := m.Service.Store.MarkRemoteSentCopyDone(ctx, job.AccountID, job.ID, outcome.UIDValidity, outcome.RemoteUID); err != nil {
		m.Service.Log.Warn("mark sent-copy done", "copy_id", job.ID, "error", err)
	}
}

func (m *RemoteMailboxService) verifySentCopyByLookup(ctx context.Context, pub *RemoteSentCopyPublisher, job store.RemoteSentCopy) {
	if strings.TrimSpace(job.RFCMessageID) == "" {
		_ = m.Service.Store.MarkRemoteSentCopyAmbiguous(ctx, job.AccountID, job.ID, "append result could not be verified")
		return
	}
	outcome, err := pub.FindCopiedByMessageID(ctx, job.AccountID, job.InboxID, job.RFCMessageID)
	if err != nil || !outcome.Confirmed {
		_ = m.Service.Store.MarkRemoteSentCopyAmbiguous(ctx, job.AccountID, job.ID, "append result could not be verified")
		return
	}
	_ = m.Service.Store.MarkRemoteSentCopyDone(ctx, job.AccountID, job.ID, outcome.UIDValidity, outcome.RemoteUID)
}

func (m *RemoteMailboxService) settleSentCopyError(ctx context.Context, job store.RemoteSentCopy, err error) {
	if errors.Is(err, ErrNoSentFolderApp) || errors.Is(err, store.ErrHandoffUnsupported) {
		_ = m.Service.Store.MarkRemoteSentCopyFailed(ctx, job.AccountID, job.ID, "this inbox has no remote Sent folder")
		return
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) && (mb.Kind == model.ErrKindForbidden || mb.Kind == model.ErrKindNotFound || mb.Kind == model.ErrKindUnsupported) {
		_ = m.Service.Store.MarkRemoteSentCopyFailed(ctx, job.AccountID, job.ID, "sent-copy was refused by the remote server")
		return
	}
	// A transient copy that has exhausted the bounded retry budget is terminal:
	// the sent copy is a convenience, not the delivery, so a persistent failure
	// must not pin the queue forever. The SMTP send already succeeded and is never
	// repeated.
	if job.Attempts >= maxRemoteSentCopyAttempts {
		_ = m.Service.Store.MarkRemoteSentCopyFailed(ctx, job.AccountID, job.ID, "sent-copy retries exhausted")
		return
	}
	// Transient: return to the queue with backoff. The SMTP send already
	// succeeded and is never repeated.
	next := time.Now().UTC().Add(time.Duration(job.Attempts+1) * time.Minute)
	if rerr := m.Service.Store.RetryRemoteSentCopy(ctx, job.AccountID, job.ID, "transient copy failure", next); rerr != nil {
		m.Service.Log.Warn("retry sent-copy", "copy_id", job.ID, "error", rerr)
	}
}

// maxRemoteSentCopyAttempts bounds how many transient append attempts a sent-copy
// job makes before it is terminally failed, so a persistent fault cannot grow the
// queue without limit.
const maxRemoteSentCopyAttempts = 8

// readFrozenRaw reads a data-dir-relative frozen raw MIME file for a copy job.
func readFrozenRaw(s *Service, rel string) ([]byte, error) {
	if strings.TrimSpace(rel) == "" {
		return nil, fmt.Errorf("no frozen raw path")
	}
	path, err := s.dataPath(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
