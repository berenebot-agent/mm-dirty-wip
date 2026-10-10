package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

// RemoteMessageView is the read projection of a cached remote message: the header
// metadata the store holds, plus its local labels. It carries no body; a body is
// fetched transiently on demand by FetchRemoteRaw and is never archived.
type RemoteMessageView struct {
	ID           string        `json:"id"`
	InboxID      string        `json:"inbox_id"`
	FolderPath   string        `json:"folder_path"`
	UIDValidity  uint32        `json:"uid_validity"`
	UID          uint32        `json:"uid"`
	RFCMessageID string        `json:"message_id,omitempty"`
	InReplyTo    string        `json:"in_reply_to,omitempty"`
	References   []string      `json:"references,omitempty"`
	ThreadKey    string        `json:"thread_id,omitempty"`
	From         model.Address `json:"from"`
	To           []string      `json:"to,omitempty"`
	CC           []string      `json:"cc,omitempty"`
	Subject      string        `json:"subject"`
	Snippet      string        `json:"snippet,omitempty"`
	SizeBytes    int64         `json:"size_bytes"`
	HasAttach    bool          `json:"has_attachments"`
	Read         bool          `json:"read"`
	Flagged      bool          `json:"flagged"`
	Answered     bool          `json:"answered"`
	Draft        bool          `json:"draft"`
	Flags        []string      `json:"flags,omitempty"`
	Labels       []string      `json:"labels,omitempty"`
	ReceivedAt   *string       `json:"received_at,omitempty"`
	SentAt       *string       `json:"sent_at,omitempty"`
	InternalDate *string       `json:"internal_date,omitempty"`
	IndexedAt    time.Time     `json:"indexed_at,omitempty"`
}

// remoteView projects a store.RemoteMessage onto the read DTO.
func remoteView(m store.RemoteMessage) RemoteMessageView {
	return RemoteMessageView{
		ID:           m.ID,
		InboxID:      m.InboxID,
		FolderPath:   m.FolderPath,
		UIDValidity:  m.UIDValidity,
		UID:          m.UID,
		RFCMessageID: m.RFCMessageID,
		InReplyTo:    m.InReplyTo,
		References:   m.References,
		ThreadKey:    m.ThreadKey,
		From:         model.Address{Name: m.FromName, Address: m.FromAddress},
		To:           m.To,
		CC:           m.CC,
		Subject:      m.Subject,
		Snippet:      m.Snippet,
		SizeBytes:    m.SizeBytes,
		HasAttach:    m.HasAttach,
		Read:         m.Read,
		Flagged:      m.Flagged,
		Answered:     m.Answered,
		Draft:        m.Draft,
		Flags:        m.Flags,
		Labels:       m.Labels,
		ReceivedAt:   m.ReceivedAt,
		SentAt:       m.SentAt,
		InternalDate: m.InternalDate,
		IndexedAt:    parseStoreTime(m.IndexedAt),
	}
}

// DefaultRemoteReconcileLimit bounds how many messages a single folder reconcile
// indexes, so a huge remote mailbox cannot exhaust memory in one pass. A folder
// larger than this is reported partial.
// DefaultRemoteReconcileLimit is the number of older messages one reconcile pass
// backfills per folder. Together with the per-folder cursor it bounds the work of
// a single pass while still letting the whole folder be indexed over successive
// passes; it is NOT a permanent cap. The newest window is refreshed every pass.
const DefaultRemoteReconcileLimit = 2000

// remoteNewestRefresh is the number of newest messages whose headers are
// refreshed on every reconcile pass, so recent mail and flag changes are always
// current even while an older backlog is still being backfilled.
const remoteNewestRefresh = 500

// remoteUIDSetCap bounds the full UID set a reconcile will hold to prune against.
// A set is four bytes per message; a folder beyond this is left un-pruned for the
// pass (reported partial) rather than allocating an unbounded slice.
const remoteUIDSetCap = 500000

// ReconcileRemote refreshes the cached metadata index of a standalone inbox: the
// folder tree with each folder's live UIDVALIDITY, then each selectable folder's
// message headers. It never stores a body. It is safe to run on demand (a scoped
// read calls it when its cached view is stale) and from the sync worker.
//
// A folder is indexed progressively: every pass refreshes the newest window and
// advances the folder's persisted backfill cursor downward by one bounded batch,
// so a folder larger than one batch is eventually indexed in full — it is never
// permanently truncated. Removals are pruned only against a complete UID set
// (a full SEARCH), so a partial view never deletes cached mail.
//
// The result reports whether the whole scope was enumerated; an interrupted pass,
// a folder whose backfill has not yet reached the bottom, or a provider error
// leaves the status partial, so a caller can report completeness honestly.
func (m *RemoteMailboxService) ReconcileRemote(ctx context.Context, accountID, inboxID string) (result store.RemoteIndexStatus, resultErr error) {
	key := accountID + ":" + inboxID
	m.refreshMu.Lock()
	if call, ok := m.reconciling[key]; ok {
		m.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return store.RemoteIndexStatus{}, ctx.Err()
		case <-call.done:
			return call.status, call.err
		}
	}
	call := &remoteReconcileCall{done: make(chan struct{})}
	m.reconciling[key] = call
	m.refreshMu.Unlock()
	defer func() {
		m.refreshMu.Lock()
		delete(m.reconciling, key)
		call.status, call.err = result, resultErr
		close(call.done)
		m.refreshMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	sess, resolved, err := m.open(ctx, accountID, inboxID)
	if err != nil {
		_ = m.Service.Store.SetRemoteIndexStatus(context.WithoutCancel(ctx), accountID, inboxID, store.RemoteIndexError, "could not open the remote connection")
		return store.RemoteIndexStatus{}, err
	}
	defer sess.Close()

	scope := resolved.Inbox.Namespace
	folders, discoveredScope, err := sess.DiscoverFolders(ctx, scope)
	if err != nil {
		_ = m.Service.Store.SetRemoteIndexStatus(context.WithoutCancel(ctx), accountID, inboxID, store.RemoteIndexError, "folder discovery failed")
		return store.RemoteIndexStatus{}, normalizeRemoteError(err)
	}

	// Resolve each folder's live UIDVALIDITY (EXAMINE) and record it with the
	// folder set, then persist the reconciled folder tree.
	folderSync := make([]store.RemoteFolderSync, 0, len(folders))
	now := time.Now().UTC()
	validityResolved := true
	for _, f := range folders {
		validity := uint32(0)
		if f.Selectable {
			v, verr := sess.EnsureFolderExists(ctx, f.Path)
			if verr != nil {
				// A selectable folder whose UIDVALIDITY cannot be resolved must not
				// be claimed complete: leave validity 0 so the index pass skips it
				// and the overall status is partial.
				validityResolved = false
			} else {
				validity = v
			}
		}
		folderSync = append(folderSync, store.RemoteFolderSync{
			Path:        f.Path,
			Name:        f.Name,
			ParentPath:  parentOf(f.Path, discoveredScope.Delimiter),
			Role:        f.Role,
			Selectable:  f.Selectable,
			UIDValidity: validity,
			HasChildren: f.HasChildren,
			SpecialUse:  f.SpecialUse,
			IndexedAt:   now,
		})
	}
	if _, err := m.Service.Store.ReconcileRemoteFolders(ctx, accountID, inboxID, folderSync); err != nil {
		return store.RemoteIndexStatus{}, mapStoreError(err)
	}

	// Index the header metadata of each selectable folder progressively. A folder
	// index failure is reported partial rather than aborting the whole reconcile.
	complete := validityResolved
	for _, f := range folderSync {
		if !f.Selectable || f.UIDValidity == 0 {
			continue
		}
		folderComplete, err := m.indexRemoteFolder(ctx, sess, accountID, inboxID, f.Path, f.UIDValidity)
		if err != nil {
			m.Service.Log.Debug("remote folder index", "inbox_id", inboxID, "folder", f.Path, "error", err)
			complete = false
			continue
		}
		if !folderComplete {
			complete = false
		}
	}
	status := store.RemoteIndexComplete
	if !complete {
		status = store.RemoteIndexPartial
	}
	if err := m.Service.Store.SetRemoteIndexStatus(ctx, accountID, inboxID, status, ""); err != nil {
		return store.RemoteIndexStatus{}, mapStoreError(err)
	}
	return store.RemoteIndexStatus{Status: status, IndexedAt: now}, nil
}

// indexRemoteFolder performs one bounded reconcile pass over a folder: it reads
// the folder's complete UID set, refreshes the newest window, advances the
// persisted backfill cursor by one batch, and prunes cached rows against the
// complete set when it is known. It never fetches a body. A folder larger than the
// per-pass batch is indexed over successive passes, so it is never permanently
// truncated.
func (m *RemoteMailboxService) indexRemoteFolder(ctx context.Context, sess RemoteSession, accountID, inboxID, folderPath string, validity uint32) (bool, error) {
	// The complete matching UID set. A UID SEARCH returns every match; a UID set is
	// only four bytes per message, so this is the cheapest way to obtain the full
	// snapshot needed to prune removals safely.
	fullSearch, err := sess.Search(ctx, folderPath, imap.SearchQuery{NoLimit: true})
	if err != nil {
		return false, normalizeRemoteError(err)
	}
	allUIDs := fullSearch.UIDs // ascending
	tooLarge := len(allUIDs) > remoteUIDSetCap
	fullKnown := fullSearch.Completeness == imap.CompletenessComplete && !tooLarge
	if tooLarge {
		// Too large to hold in full: index the newest window only, skip prune, and
		// never advance the backfill cursor or claim completeness (the cursor is
		// relative to the full set, which we do not have).
		allUIDs = allUIDs[len(allUIDs)-DefaultRemoteReconcileLimit:]
	}

	bf, berr := m.Service.Store.GetRemoteFolderBackfill(ctx, accountID, inboxID, folderPath)
	if berr != nil && !errors.Is(berr, store.ErrNotFound) {
		return false, mapStoreError(berr)
	}
	started := bf.Known && bf.Generation == validity
	isComplete := started && bf.Complete

	// Newest window: always refreshed so recent mail and flag changes are current.
	newestStart := len(allUIDs) - remoteNewestRefresh
	if newestStart < 0 {
		newestStart = 0
	}
	newest := allUIDs[newestStart:]

	var older []uint32
	newBefore := uint32(0)
	newComplete := isComplete
	if tooLarge {
		// Index the newest window only and leave the cursor / completeness as it
		// was, so a folder too large to snapshot is never falsely reported done.
		older = nil
		newBefore = bf.BeforeUID
		newComplete = false
	} else if !isComplete {
		// Backfill batch: the newest DefaultRemoteReconcileLimit UIDs strictly
		// below the cursor. If fewer exist, the bottom has been reached.
		effectiveBefore := bf.BeforeUID
		if !started || effectiveBefore == 0 {
			effectiveBefore = ^uint32(0) // not started: begin from the newest
		}
		olderStart := -1
		for i := len(allUIDs) - 1; i >= 0; i-- {
			if allUIDs[i] < effectiveBefore {
				olderStart = i
				break
			}
		}
		if olderStart < 0 {
			// Nothing left below the cursor: backfill is complete.
			newComplete = true
		} else {
			lo := olderStart - DefaultRemoteReconcileLimit + 1
			if lo < 0 {
				lo = 0
			}
			older = allUIDs[lo : olderStart+1]
			newBefore = older[0]
			newComplete = lo == 0
			if newComplete {
				newBefore = 0
			}
		}
	}

	fetch := unionUIDs(newest, older)
	if len(fetch) > 0 {
		headers, liveValidity, herr := sess.ListHeaders(ctx, folderPath, fetch, 0)
		if herr != nil {
			return false, normalizeRemoteError(herr)
		}
		// A UIDVALIDITY change mid-pass means the snapshot is stale; leave the
		// folder for the next pass rather than writing mismatched rows.
		if liveValidity != 0 && liveValidity != validity {
			return false, nil
		}
		inputs := make([]store.RemoteMessageInput, 0, len(headers))
		for _, h := range headers {
			inputs = append(inputs, remoteMessageInputFromHeader(h))
		}
		if _, werr := m.Service.Store.ReconcileRemoteFolderBatch(ctx, accountID, inboxID, folderPath, validity, inputs, allUIDs, fullKnown, newBefore, newComplete); werr != nil {
			return false, mapStoreError(werr)
		}
		return fullKnown && newComplete, nil
	}
	// An empty fetch still advances/prunes: an emptied folder must be pruned and
	// marked complete.
	if _, werr := m.Service.Store.ReconcileRemoteFolderBatch(ctx, accountID, inboxID, folderPath, validity, nil, allUIDs, fullKnown, newBefore, newComplete); werr != nil {
		return false, mapStoreError(werr)
	}
	return fullKnown && newComplete, nil
}

// unionUIDs merges two ascending UID slices into a de-duplicated ascending slice.
func unionUIDs(a, b []uint32) []uint32 {
	if len(b) == 0 {
		return append([]uint32(nil), a...)
	}
	if len(a) == 0 {
		return append([]uint32(nil), b...)
	}
	out := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i] < b[j]):
			out = append(out, a[i])
			i++
		case i >= len(a) || b[j] < a[i]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// remoteMessageInputFromHeader maps an adapter MessageHeader onto the store's
// metadata input. Snippet is a short prefix of the subject when no body is known.
func remoteMessageInputFromHeader(h imap.MessageHeader) store.RemoteMessageInput {
	var received *string
	if !h.InternalDate.IsZero() {
		s := h.InternalDate.UTC().Format(time.RFC3339Nano)
		received = &s
	} else if !h.Date.IsZero() {
		s := h.Date.UTC().Format(time.RFC3339Nano)
		received = &s
	}
	var sent *string
	if !h.Date.IsZero() {
		s := h.Date.UTC().Format(time.RFC3339Nano)
		sent = &s
	}
	var internal *string
	if !h.InternalDate.IsZero() {
		s := h.InternalDate.UTC().Format(time.RFC3339Nano)
		internal = &s
	}
	// ThreadKey is deliberately left empty: the store resolves it against the
	// parent message already indexed (by In-Reply-To/References), so a reply joins
	// its conversation instead of starting a new thread. When no parent is found
	// the store falls back to the Message-ID.
	return store.RemoteMessageInput{
		FolderPath:   h.FolderPath,
		UIDValidity:  h.UIDValidity,
		UID:          h.UID,
		RFCMessageID: h.MessageID,
		InReplyTo:    h.InReplyTo,
		References:   h.References,
		FromName:     h.From.Name,
		FromAddress:  h.From.Address,
		To:           h.To,
		CC:           h.CC,
		Subject:      h.Subject,
		Snippet:      "",
		SizeBytes:    h.Size,
		HasAttach:    h.HasAttach,
		Read:         h.Read,
		Flagged:      h.Flagged,
		Answered:     h.Answered,
		Draft:        h.Draft,
		Flags:        h.Flags,
		ReceivedAt:   received,
		SentAt:       sent,
		InternalDate: internal,
	}
}

func parentOf(path string, delim rune) string {
	if delim == 0 {
		return ""
	}
	if idx := strings.LastIndexByte(path, byte(delim)); idx > 0 {
		return path[:idx]
	}
	return ""
}

// RemoteListResult is a page of cached remote messages plus its completeness and
// the folder's UIDVALIDITY.
type RemoteListResult struct {
	Items        []RemoteMessageView    `json:"items"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Completeness model.ListCompleteness `json:"completeness"`
	UIDValidity  uint32                 `json:"uid_validity"`
	Reconciled   bool                   `json:"reconciled"`
}

// ListRemoteMessages returns a folder's cached message metadata, newest first. It
// reconciles the folder on demand when the cached view is stale (its
// UIDVALIDITY is unknown, or it has never been indexed), so a scoped read is
// always against a trustworthy index. scope is the folder path; the caller has
// already enforced that the folder is within the inbox's selected root scope.
func (m *RemoteMailboxService) ListRemoteMessages(ctx context.Context, p model.Principal, inboxID, folderPath string, limit int, before string) (RemoteListResult, error) {
	return m.listRemoteMessages(ctx, p, inboxID, folderPath, limit, before, false)
}

// ListRemoteMessagesCached serves UI navigation without waiting for initial IMAP
// indexing. API clients retain their initial-index bootstrap behaviour.
func (m *RemoteMailboxService) ListRemoteMessagesCached(ctx context.Context, p model.Principal, inboxID, folderPath string, limit int, before string) (RemoteListResult, error) {
	return m.listRemoteMessages(ctx, p, inboxID, folderPath, limit, before, true)
}

func (m *RemoteMailboxService) listRemoteMessages(ctx context.Context, p model.Principal, inboxID, folderPath string, limit int, before string, cachedOnly bool) (RemoteListResult, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return RemoteListResult{}, err
	}
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		folderPath = inbox.Namespace
		if folderPath == "" {
			folderPath = model.NamespaceINBOX
		}
	}
	// Bootstrap API reads on first use; UI reads schedule the same work and return
	// the cache immediately. Discovery still enforces the personal namespace.
	reconciled := false
	status, serr := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, inboxID)
	if serr != nil {
		return RemoteListResult{}, mapStoreError(serr)
	}
	validity, indexedAt, known, verr := m.Service.Store.RemoteFolderValidity(ctx, p.AccountID, inboxID, folderPath)
	if verr != nil {
		return RemoteListResult{}, mapStoreError(verr)
	}
	if !known || indexedAt.IsZero() || status.Status == store.RemoteIndexNeverStarted {
		if cachedOnly {
			m.ScheduleRefresh(p.AccountID, inboxID)
		} else {
			status, err = m.ReconcileRemote(ctx, p.AccountID, inboxID)
			if err != nil {
				return RemoteListResult{}, err
			}
			reconciled = true
			validity, indexedAt, known, _ = m.Service.Store.RemoteFolderValidity(ctx, p.AccountID, inboxID, folderPath)
		}
	} else if bf, berr := m.Service.Store.GetRemoteFolderBackfill(ctx, p.AccountID, inboxID, folderPath); berr == nil && !bf.Complete {
		// The folder's backfill has not reached the bottom. Advance it one bounded
		// batch on demand (the UI paging through older mail drives this), so old
		// mail beyond the first batch is never permanently hidden.
		if before != "" {
			m.ScheduleRefresh(p.AccountID, inboxID)
		}
	}
	// Enforce that the folder is within the inbox's selected root scope after the
	// reconcile: a path from outside the scope (for example a shared namespace)
	// is not addressable.
	if !m.folderInScope(inbox, folderPath) {
		return RemoteListResult{}, model.NewMailboxError(model.ErrKindNotFound, "folder is not in this mailbox's scope", false, nil)
	}
	msgs, err := m.Service.Store.ListRemoteMessagesFiltered(ctx, p.AccountID, inboxID, store.RemoteMessageFilter{FolderPath: folderPath, Limit: limit, Before: before})
	if err != nil {
		return RemoteListResult{}, mapStoreError(err)
	}
	out := RemoteListResult{UIDValidity: validity, Reconciled: reconciled, Completeness: model.CompletenessComplete}
	if !known || indexedAt.IsZero() || status.Status != store.RemoteIndexComplete {
		out.Completeness = model.CompletenessPartial
	}
	if limit > 0 && len(msgs) == limit {
		// Another page may exist. The cursor is the metadata id of the last
		// returned message, which the Before filter resolves to that row's
		// (received_at, id) ordering tuple. A bare received_at is not unique (a
		// burst of same-second mail shares one) and would drop every sibling on
		// the next page, so the id is the cursor.
		if last := msgs[len(msgs)-1].ID; strings.TrimSpace(last) != "" {
			out.NextCursor = last
		}
	}
	for _, msg := range msgs {
		view := remoteView(msg)
		labels, lerr := m.Service.Store.RemoteMessageLabels(ctx, p.AccountID, inboxID, msg.ID)
		if lerr == nil {
			view.Labels = labels
		}
		out.Items = append(out.Items, view)
	}
	if out.Items == nil {
		out.Items = []RemoteMessageView{}
	}
	return out, nil
}

// GetRemoteMessage returns one cached remote message with its local labels. It
// refreshes the header from the live server when the cached row is stale; a live
// refresh never marks the message seen (BODY.PEEK).
func (m *RemoteMailboxService) GetRemoteMessage(ctx context.Context, p model.Principal, inboxID, messageID string) (RemoteMessageView, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return RemoteMessageView{}, err
	}
	rec, err := m.Service.Store.GetRemoteMessageWithLabels(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	if !m.folderInScope(inbox, rec.FolderPath) {
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindNotFound, "message is not in this mailbox's scope", false, nil)
	}
	view := remoteView(rec)
	// Best-effort live refresh: a fetch failure returns the cached view rather
	// than failing the read, because the cache is the durable source of truth.
	m.refreshRemoteHeader(ctx, p.AccountID, inboxID, &view)
	return view, nil
}

// refreshRemoteHeader updates the cached header from the live server if the
// session can be opened and the UIDVALIDITY still matches. It never marks the
// message seen.
func (m *RemoteMailboxService) refreshRemoteHeader(ctx context.Context, accountID, inboxID string, view *RemoteMessageView) {
	sess, _, err := m.open(ctx, accountID, inboxID)
	if err != nil {
		return
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: view.FolderPath, UIDValidity: view.UIDValidity, UID: view.UID, MessageID: view.RFCMessageID}
	h, err := sess.FetchHeader(ctx, loc)
	if err != nil {
		var mb *model.MailboxError
		if errors.As(err, &mb) && mb.Kind == model.ErrKindConflict && strings.TrimSpace(view.RFCMessageID) != "" {
			// UIDVALIDITY changed: the cached UID no longer names this message.
			// Re-resolve it by Message-ID and relocate the cached row to the new
			// locator, preserving its stable id and local labels. A message with
			// no Message-ID cannot be re-resolved and is left as cached.
			if newLoc, ferr := sess.FindByMessageID(ctx, view.FolderPath, view.RFCMessageID); ferr == nil {
				if updated, uerr := m.Service.Store.MoveRemoteMessageMetadata(ctx, accountID, inboxID, view.ID, newLoc.FolderPath, newLoc.UIDValidity, newLoc.UID); uerr == nil {
					*view = remoteView(updated)
				}
			}
		}
		return
	}
	if h.UID != 0 {
		if updated, uerr := m.Service.Store.UpsertRemoteMessage(ctx, accountID, inboxID, remoteMessageInputFromHeader(h)); uerr == nil {
			labels := view.Labels
			*view = remoteView(updated)
			view.Labels = labels
		}
	}
}

// authorizeRead resolves a standalone inbox and checks the principal can read it.
func (m *RemoteMailboxService) authorizeRead(ctx context.Context, p model.Principal, inboxID string) (model.Inbox, error) {
	if !p.CanRead(inboxID) && !p.Admin {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	inbox, err := m.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Inbox{}, mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, ErrRemoteNotBound)
	}
	return inbox, nil
}

// authorizeAssist resolves a standalone inbox and checks the principal can assist
// (Assistant or Owner), the authority a state change needs.
func (m *RemoteMailboxService) authorizeAssist(ctx context.Context, p model.Principal, inboxID string) (model.Inbox, error) {
	if !p.CanAssist(inboxID) && !p.Admin {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	return m.authorizeRead(ctx, p, inboxID)
}

// folderInScope reports whether a folder path is inside the inbox's selected root
// scope. An explicit root covers only that folder and its children. The default
// personal root (INBOX) covers INBOX plus every folder the scope-correct
// reconcile admitted into this inbox's tree: discovery excludes the server's
// shared/other namespaces, so this never falls back to the whole login. A folder
// that has never been admitted (for example a shared namespace) is out of scope.
// This is the single place folder scoping is enforced for remote operations.
func (m *RemoteMailboxService) folderInScope(inbox model.Inbox, folderPath string) bool {
	folderPath = strings.TrimSpace(folderPath)
	if folderPath == "" {
		return false
	}
	root := strings.TrimSpace(inbox.Namespace)
	if root != "" && !strings.EqualFold(root, model.NamespaceINBOX) {
		if strings.EqualFold(folderPath, root) {
			return true
		}
		// Accept either delimiter convention ("/" and ".") for a child path.
		for _, delim := range []string{"/", "."} {
			if strings.HasPrefix(folderPath, root+delim) {
				return true
			}
		}
		return false
	}
	// Personal namespace: INBOX itself, or a folder admitted into this inbox's
	// scope-correct tree (an exact folder or a child of one).
	if strings.EqualFold(folderPath, model.NamespaceINBOX) {
		return true
	}
	folders, err := m.Service.Store.ListFolders(context.Background(), inbox.AccountID, inbox.ID)
	if err != nil {
		return false
	}
	for _, f := range folders {
		fp := strings.TrimSpace(f.Path)
		if fp == "" {
			continue
		}
		if strings.EqualFold(fp, folderPath) {
			return true
		}
		for _, delim := range []string{"/", "."} {
			if strings.HasPrefix(folderPath, fp+delim) {
				return true
			}
		}
	}
	return false
}

// InScope reports whether a record's folder is inside the inbox's selected root
// scope. It is the exported form of folderInScope for the HTTP layer, so an
// arrival-resolved message is scope-checked in exactly one place.
func (m *RemoteMailboxService) InScope(accountID, inboxID, folderPath string) bool {
	inbox, err := m.Service.Store.GetInboxInternal(context.WithoutCancel(context.Background()), accountID, inboxID)
	if err != nil {
		return false
	}
	return m.folderInScope(inbox, folderPath)
}

// ViewOf projects a cached metadata record onto the public read DTO, loading its
// local labels. It performs no provider call, so an arrival-resolved read never
// touches the network.
func (m *RemoteMailboxService) ViewOf(rec store.RemoteMessage) RemoteMessageView {
	view := remoteView(rec)
	labels, err := m.Service.Store.RemoteMessageLabels(context.WithoutCancel(context.Background()), rec.AccountID, rec.InboxID, rec.ID)
	if err == nil {
		view.Labels = labels
	}
	return view
}

// FetchRemoteRaw streams a remote message's raw RFC5322 MIME to a temporary file
// and returns its absolute path together with the file's size. The body is
// transient: the caller must call CleanupRemoteRaw after consuming it. The stream
// is bounded by the configured message-size cap; an over-limit body aborts and is
// removed. The message is never marked seen (BODY.PEEK).
func (m *RemoteMailboxService) FetchRemoteRaw(ctx context.Context, p model.Principal, inboxID, messageID string) (string, int64, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return "", 0, err
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return "", 0, mapStoreError(err)
	}
	if !m.folderInScope(inbox, rec.FolderPath) {
		return "", 0, model.NewMailboxError(model.ErrKindNotFound, "message is not in this mailbox's scope", false, nil)
	}
	return m.fetchRemoteRawFor(ctx, p.AccountID, inboxID, messageID, &rec)
}

func (m *RemoteMailboxService) fetchRemoteRawFor(ctx context.Context, accountID, inboxID, messageID string, rec *store.RemoteMessage) (string, int64, error) {
	if rec == nil {
		loaded, err := m.Service.Store.GetRemoteMessage(ctx, accountID, inboxID, messageID)
		if err != nil {
			return "", 0, mapStoreError(err)
		}
		rec = &loaded
	}
	sess, _, err := m.open(ctx, accountID, inboxID)
	if err != nil {
		return "", 0, err
	}
	defer sess.Close()
	dir := filepath.Join(m.Service.Config.DataDir, "remote", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	f, err := os.CreateTemp(dir, "body-*.eml")
	if err != nil {
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	limit := m.remoteBodyLimit()
	lw := &limitedWriter{w: f, limit: limit}
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	ferr := sess.FetchRawMIME(ctx, loc, lw)
	if ferr != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", 0, normalizeRemoteError(ferr)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	return f.Name(), lw.n, nil
}

// CleanupRemoteRaw removes a transient remote body file. It is safe to call on a
// path that is already gone.
func (m *RemoteMailboxService) CleanupRemoteRaw(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = os.Remove(path)
}

// limitedWriter writes at most limit bytes and then fails, so a remote body over
// the size cap stops the stream instead of exhausting memory or disk.
type limitedWriter struct {
	w     io.Writer
	limit int64
	n     int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.limit {
		return 0, fmt.Errorf("remote body exceeds the maximum message size")
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return n, err
}

// RemoteAttachment is a transient downloaded attachment. The caller streams the
// file to the client and calls CleanupRemoteRaw on Path.
type RemoteAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Path        string `json:"-"`
}

// FetchRemoteAttachment streams one MIME part of a remote message to a temporary
// file. The part is identified structurally (a MIME part path), so it never
// depends on the provider naming a folder. The content-type and filename come from
// the cached BODYSTRUCTURE when known; the caller sets Content-Disposition:
// attachment + nosniff when serving it.
func (m *RemoteMailboxService) FetchRemoteAttachment(ctx context.Context, p model.Principal, inboxID, messageID string, part []int, filename, contentType string) (RemoteAttachment, error) {
	inbox, err := m.authorizeRead(ctx, p, inboxID)
	if err != nil {
		return RemoteAttachment{}, err
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return RemoteAttachment{}, mapStoreError(err)
	}
	if !m.folderInScope(inbox, rec.FolderPath) {
		return RemoteAttachment{}, model.NewMailboxError(model.ErrKindNotFound, "message is not in this mailbox's scope", false, nil)
	}
	return m.fetchRemoteAttachmentFor(ctx, p.AccountID, inboxID, messageID, part, filename, contentType)
}

func (m *RemoteMailboxService) fetchRemoteAttachmentFor(ctx context.Context, accountID, inboxID, messageID string, part []int, filename, contentType string) (RemoteAttachment, error) {
	rec, err := m.Service.Store.GetRemoteMessage(ctx, accountID, inboxID, messageID)
	if err != nil {
		return RemoteAttachment{}, mapStoreError(err)
	}
	if len(part) == 0 {
		return RemoteAttachment{}, model.NewMailboxError(model.ErrKindInvalid, "a MIME part path is required", false, nil)
	}
	sess, _, err := m.open(ctx, accountID, inboxID)
	if err != nil {
		return RemoteAttachment{}, err
	}
	defer sess.Close()
	dir := filepath.Join(m.Service.Config.DataDir, "remote", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return RemoteAttachment{}, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	f, err := os.CreateTemp(dir, "att-*")
	if err != nil {
		return RemoteAttachment{}, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	limit := m.remoteBodyLimit()
	lw := &limitedWriter{w: f, limit: limit}
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	ferr := sess.FetchBodyPart(ctx, loc, part, lw)
	if ferr != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return RemoteAttachment{}, normalizeRemoteError(ferr)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return RemoteAttachment{}, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	return RemoteAttachment{Filename: filename, ContentType: contentType, Size: lw.n, Path: f.Name()}, nil
}

// PurgeRemoteMessage permanently erases a standalone inbox's remote message with
// a UID-targeted expunge (UID EXPUNGE, never a blanket EXPUNGE) and removes its
// cached metadata row. It requires Owner on the inbox: permanent erasure is an
// irreversible provider operation. The message must currently live in a
// Trash-role folder, so a purge can never erase live mail straight out of the
// Inbox: the caller must have trashed it first (or the server's Trash folder is
// the mapped Trash role). When the cached metadata is already gone the purge is
// a no-op success (idempotent), because the server may have expunged it already.
func (m *RemoteMailboxService) PurgeRemoteMessage(ctx context.Context, p model.Principal, inboxID, messageID string) error {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return err
	}
	if !p.CanOwn(inboxID) && !p.Admin {
		return model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return mapStoreError(err)
	}
	if !m.folderInScope(inbox, rec.FolderPath) {
		return model.NewMailboxError(model.ErrKindNotFound, "message is not in this mailbox's scope", false, nil)
	}
	// The message must be in a Trash-role folder: a permanent purge is only ever
	// the last step of the trash flow, never a shortcut around it.
	trash, ok := m.trashFolder(ctx, p.AccountID, inboxID)
	if !ok {
		return model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote Trash folder mapped; trash the message first", false, nil)
	}
	if !strings.EqualFold(rec.FolderPath, trash) {
		return model.NewMailboxError(model.ErrKindConflict, "only a trashed message can be permanently purged", false, nil)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return err
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	if derr := sess.DeleteMessage(ctx, loc); derr != nil {
		if errors.Is(derr, imap.ErrNotFound) {
			// The server already expunged it: drop the stale cache row.
			_ = m.Service.Store.DeleteRemoteMessage(ctx, p.AccountID, inboxID, rec.ID)
			return nil
		}
		return normalizeRemoteError(derr)
	}
	if derr := m.Service.Store.DeleteRemoteMessage(ctx, p.AccountID, inboxID, rec.ID); derr != nil && !errors.Is(derr, store.ErrNotFound) {
		// The expunge succeeded; a stale cache row is reindexed by the next
		// reconcile, so a delete failure is not surfaced as a purge failure.
		m.Service.Log.Warn("purge remote metadata", "message_id", messageID, "error", derr)
	}
	return nil
}

// ResolveRemoteReply resolves a remote message's header metadata as a
// model.Message for a reply source. It performs no provider call: the cached
// metadata already holds the thread key, Message-ID, References and addresses the
// reply is built from.
func (m *RemoteMailboxService) ResolveRemoteReply(ctx context.Context, accountID, id string) (model.Message, bool, error) {
	rec, ok, err := m.resolveRemoteRecord(ctx, accountID, id)
	if err != nil {
		return model.Message{}, false, err
	}
	if !ok {
		return model.Message{}, false, nil
	}
	return remoteMessageToModelMessage(rec), true, nil
}

// resolveRemoteRecord resolves a remote message id (a metadata id, or an arrival
// event's entity id) to its cached metadata record, across the account's
// standalone inboxes. It performs no provider call: an arrival id is bridged to
// its metadata row (materializing it from the arrival's durable header when the
// index has not yet recorded it), so a reply or forward can follow a remote
// event id without the client first translating it.
func (m *RemoteMailboxService) resolveRemoteRecord(ctx context.Context, accountID, id string) (store.RemoteMessage, bool, error) {
	boxes, err := m.Service.Store.ListStandaloneInboxes(ctx, accountID)
	if err != nil {
		return store.RemoteMessage{}, false, mapStoreError(err)
	}
	for _, box := range boxes {
		rec, gerr := m.Service.Store.GetRemoteMessageWithLabels(ctx, accountID, box.ID, id)
		if gerr != nil {
			continue
		}
		return rec, true, nil
	}
	rec, aerr := m.Service.Store.RemoteMessageForArrival(ctx, accountID, id)
	if aerr == nil {
		return rec, true, nil
	}
	if !errors.Is(aerr, store.ErrNotFound) {
		return store.RemoteMessage{}, false, mapStoreError(aerr)
	}
	return store.RemoteMessage{}, false, nil
}

// ResolveRemoteForward resolves a remote message for a forward: it fetches the
// body transiently, parses it to Text/HTML, and extracts its attachments, then
// removes the transient file. The body is never archived. It returns ok=false
// when the id is not a remote message of the account.
func (m *RemoteMailboxService) ResolveRemoteForward(ctx context.Context, accountID, id string) (model.Message, []SendAttachment, bool, error) {
	rec, ok, err := m.resolveRemoteRecord(ctx, accountID, id)
	if err != nil {
		return model.Message{}, nil, false, err
	}
	if !ok {
		return model.Message{}, nil, false, nil
	}
	msg := remoteMessageToModelMessage(rec)
	path, _, ferr := m.fetchRemoteRawFor(ctx, accountID, rec.InboxID, rec.ID, &rec)
	if ferr != nil {
		// Metadata is known; a body fetch failure still allows a metadata-only
		// forward rather than failing the whole send.
		return msg, nil, true, nil
	}
	defer m.CleanupRemoteRaw(path)
	parsed, perr := mailparse.ParseFile(path, m.Service.mimeLimits())
	if perr != nil {
		return msg, nil, true, nil
	}
	msg.Text = parsed.Text
	msg.HTML = parsed.HTML
	var atts []SendAttachment
	_ = mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		var buf bytes.Buffer
		if _, cerr := io.Copy(&buf, r); cerr != nil {
			return cerr
		}
		atts = append(atts, SendAttachment{Filename: a.Filename, ContentType: a.ContentType, Content: buf.Bytes()})
		return nil
	}, m.Service.mimeLimits())
	return msg, atts, true, nil
}

// remoteMessageToModelMessage projects cached remote metadata onto a model.Message
// for a reply/forward source. The body is not included (a forward fills it from a
// transient fetch).
func remoteMessageToModelMessage(rec store.RemoteMessage) model.Message {
	msg := model.Message{
		ID:             rec.ID,
		AccountID:      rec.AccountID,
		InboxID:        rec.InboxID,
		ThreadID:       rec.ThreadKey,
		RFCMessageID:   rec.RFCMessageID,
		InReplyTo:      rec.InReplyTo,
		References:     rec.References,
		From:           model.Address{Name: rec.FromName, Address: rec.FromAddress},
		To:             rec.To,
		CC:             rec.CC,
		Subject:        rec.Subject,
		SizeBytes:      rec.SizeBytes,
		HasAttachments: rec.HasAttach,
		Read:           rec.Read,
		Labels:         rec.Labels,
		FolderPath:     rec.FolderPath,
		Direction:      "inbound",
	}
	if rec.ReceivedAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *rec.ReceivedAt); err == nil {
			msg.CreatedAt = t
			msg.ReceivedAt = &t
		}
	}
	if rec.SentAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *rec.SentAt); err == nil {
			msg.SentAt = &t
		}
	}
	return msg
}

// trashFolder resolves the path of the inbox's Trash-role folder. It prefers an
// explicit/locked or remote-discovered mapping, then falls back to any folder
// carrying the role (for example a seeded system folder whose conventional name
// is also the live server path). The path, not the folder's origin, is what a
// remote move/expunge addresses.
func (m *RemoteMailboxService) trashFolder(ctx context.Context, accountID, inboxID string) (string, bool) {
	folders, err := m.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return "", false
	}
	fallback := ""
	for _, f := range folders {
		if f.Role != model.FolderRoleTrash || strings.TrimSpace(f.Path) == "" {
			continue
		}
		if f.Origin == "remote" || f.RoleLocked {
			return f.Path, true
		}
		if fallback == "" {
			fallback = f.Path
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

// SetRemoteRead sets or clears the \Seen flag on a remote message and mirrors the
// change into the cached metadata. Reading a message is a state change, so it
// requires Assistant or Owner.
func (m *RemoteMailboxService) SetRemoteRead(ctx context.Context, p model.Principal, inboxID, messageID string, read bool) (RemoteMessageView, error) {
	if _, err := m.authorizeAssist(ctx, p, inboxID); err != nil {
		return RemoteMessageView{}, err
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return RemoteMessageView{}, err
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	var add, remove []string
	if read {
		add = []string{imap.FlagSeen}
	} else {
		remove = []string{imap.FlagSeen}
	}
	if _, err := sess.SetFlags(ctx, loc, add, remove); err != nil {
		return RemoteMessageView{}, normalizeRemoteError(err)
	}
	if err := m.Service.Store.UpdateRemoteMessageFlags(ctx, p.AccountID, inboxID, rec.ID, read, rec.Flagged, rec.Answered, rec.Draft, rec.Flags); err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, messageID)
}

// SetRemoteFlagged sets or clears \Flagged on a remote message and mirrors it.
func (m *RemoteMailboxService) SetRemoteFlagged(ctx context.Context, p model.Principal, inboxID, messageID string, flagged bool) (RemoteMessageView, error) {
	if _, err := m.authorizeAssist(ctx, p, inboxID); err != nil {
		return RemoteMessageView{}, err
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return RemoteMessageView{}, err
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	var add, remove []string
	if flagged {
		add = []string{imap.FlagFlagged}
	} else {
		remove = []string{imap.FlagFlagged}
	}
	if _, err := sess.SetFlags(ctx, loc, add, remove); err != nil {
		return RemoteMessageView{}, normalizeRemoteError(err)
	}
	if err := m.Service.Store.UpdateRemoteMessageFlags(ctx, p.AccountID, inboxID, rec.ID, rec.Read, flagged, rec.Answered, rec.Draft, rec.Flags); err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, messageID)
}

// MoveRemoteMessage moves a remote message to a destination folder and relocates
// its cached metadata to the new locator, preserving the stable metadata id. The
// destination must be a selectable folder within the inbox scope. When the server
// does not report the destination UID (no COPYUID), the service re-resolves by
// Message-ID; if that is ambiguous the message is left cached in its source until
// the next reconcile, never falsely located.
func (m *RemoteMailboxService) MoveRemoteMessage(ctx context.Context, p model.Principal, inboxID, messageID, destPath string) (RemoteMessageView, error) {
	inbox, err := m.authorizeAssist(ctx, p, inboxID)
	if err != nil {
		return RemoteMessageView{}, err
	}
	destPath = strings.TrimSpace(destPath)
	if destPath == "" {
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindInvalid, "destination folder is required", false, nil)
	}
	if !m.folderInScope(inbox, destPath) {
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindNotFound, "destination folder is not in this mailbox's scope", false, nil)
	}
	rec, err := m.Service.Store.GetRemoteMessage(ctx, p.AccountID, inboxID, messageID)
	if err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	if !m.folderInScope(inbox, rec.FolderPath) {
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindNotFound, "message is not in this mailbox's scope", false, nil)
	}
	sess, _, err := m.open(ctx, p.AccountID, inboxID)
	if err != nil {
		return RemoteMessageView{}, err
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: rec.FolderPath, UIDValidity: rec.UIDValidity, UID: rec.UID, MessageID: rec.RFCMessageID}
	res, err := sess.MoveMessage(ctx, loc, destPath)
	if err != nil {
		return RemoteMessageView{}, normalizeRemoteError(err)
	}
	destUIDValidity := res.UIDValidity
	if destUIDValidity == 0 {
		if v, verr := sess.EnsureFolderExists(ctx, destPath); verr == nil {
			destUIDValidity = v
		}
	}
	if res.DestinationUID == 0 {
		// The server did not report COPYUID. Re-resolve by Message-ID so we never
		// cache a wrong UID. When the Message-ID is absent or ambiguous, drop the
		// stale source row and let the next reconcile re-index the destination.
		if strings.TrimSpace(rec.RFCMessageID) != "" {
			if found, ferr := sess.FindByMessageID(ctx, destPath, rec.RFCMessageID); ferr == nil && found.UID != 0 {
				updated, merr := m.Service.Store.MoveRemoteMessageMetadata(ctx, p.AccountID, inboxID, rec.ID, destPath, found.UIDValidity, found.UID)
				if merr == nil {
					return m.GetRemoteMessage(ctx, p, inboxID, updated.ID)
				}
			}
		}
		if derr := m.Service.Store.DeleteRemoteMessage(ctx, p.AccountID, inboxID, rec.ID); derr != nil {
			return RemoteMessageView{}, mapStoreError(derr)
		}
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindRetryable, "message moved; its new location will be indexed shortly", true, nil)
	}
	updated, err := m.Service.Store.MoveRemoteMessageMetadata(ctx, p.AccountID, inboxID, rec.ID, destPath, destUIDValidity, res.DestinationUID)
	if err != nil {
		return RemoteMessageView{}, mapStoreError(err)
	}
	return m.GetRemoteMessage(ctx, p, inboxID, updated.ID)
}
