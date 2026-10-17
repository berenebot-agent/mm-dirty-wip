package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

func (m *RemoteMailboxService) IsMicrosoft(ctx context.Context, account, inbox string) bool {
	return m.Service.Store.IsMicrosoft(ctx, account, inbox)
}
func MicrosoftAAD(account, inbox string) string { return "microsoft:" + account + ":" + inbox }
func (m *RemoteMailboxService) microsoftAccess(ctx context.Context, account, inbox string) (string, store.MicrosoftConnection, error) {
	m.microsoftMu.Lock()
	defer m.microsoftMu.Unlock()
	c, e := m.Service.Store.MicrosoftConnection(ctx, account, inbox)
	if e != nil {
		return "", c, e
	}
	b, e := m.Service.DecryptSecretAAD(MicrosoftAAD(account, inbox), c.EncryptedToken)
	if e != nil {
		return "", c, model.NewMailboxError(model.ErrKindAuth, "Microsoft credentials unavailable", false, nil)
	}
	var t microsoft.Token
	if json.Unmarshal(b, &t) != nil {
		return "", c, model.NewMailboxError(model.ErrKindAuth, "Microsoft credentials unavailable", false, nil)
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return t.AccessToken, c, nil
	}
	next, e := m.Microsoft.Refresh(ctx, c.Tenant, c.ClientID, t.RefreshToken)
	if e != nil {
		return "", c, e
	}
	if next.RefreshToken == "" {
		next.RefreshToken = t.RefreshToken
	}
	if next.Scope == "" {
		next.Scope = t.Scope
	}
	b, _ = json.Marshal(next)
	c.EncryptedToken, e = m.Service.EncryptSecretAAD(MicrosoftAAD(account, inbox), b)
	if e != nil {
		return "", c, e
	}
	c.ExpiresAt = time.Now().Add(time.Duration(next.ExpiresIn) * time.Second)
	e = m.Service.Store.SaveMicrosoftConnection(ctx, c)
	return next.AccessToken, c, e
}
func (m *RemoteMailboxService) SaveMicrosoftGrant(ctx context.Context, c store.MicrosoftConnection, t microsoft.Token) error {
	m.microsoftMu.Lock()
	defer m.microsoftMu.Unlock()
	if t.RefreshToken == "" || t.AccessToken == "" || t.ExpiresIn <= 0 {
		return model.NewMailboxError(model.ErrKindAuth, "Microsoft offline access was not granted", false, nil)
	}
	for _, s := range strings.Fields(microsoft.Scope(c.Shared)) {
		if s == "offline_access" {
			continue
		}
		if !containsFoldApp(strings.Fields(t.Scope), s) && !containsFoldApp(strings.Fields(t.Scope), "https://graph.microsoft.com/"+s) {
			return model.NewMailboxError(model.ErrKindAuth, "Microsoft mailbox permissions were not granted", false, nil)
		}
	}
	if old, e := m.Service.Store.MicrosoftConnection(ctx, c.AccountID, c.InboxID); e == nil {
		if old.IdentityID != c.IdentityID || old.Mailbox != c.Mailbox || old.Shared != c.Shared || old.ClientID != c.ClientID || old.Tenant != c.Tenant {
			return model.NewMailboxError(model.ErrKindInvalid, "Reconnect the original Microsoft identity and mailbox", false, nil)
		}
	} else if !errors.Is(e, store.ErrNotFound) {
		return e
	}
	b, _ := json.Marshal(t)
	var e error
	c.EncryptedToken, e = m.Service.EncryptSecretAAD(MicrosoftAAD(c.AccountID, c.InboxID), b)
	if e != nil {
		return e
	}
	c.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	err := m.Service.Store.SaveMicrosoftConnection(ctx, c)
	if err == nil {
		m.WakeMicrosoftPush()
	}
	return err
}
func microsoftInput(g microsoft.Message) store.RemoteMessageInput {
	in := store.RemoteMessageInput{RFCMessageID: g.InternetMessageID, FromName: g.From.EmailAddress.Name, FromAddress: g.From.EmailAddress.Address, Subject: g.Subject, Snippet: g.BodyPreview, Read: g.IsRead, Flagged: g.Flag.FlagStatus == "flagged", Draft: g.IsDraft, HasAttach: g.HasAttachments}
	for _, a := range g.ToRecipients {
		in.To = append(in.To, a.EmailAddress.Address)
	}
	for _, a := range g.CCRecipients {
		in.CC = append(in.CC, a.EmailAddress.Address)
	}
	for _, h := range g.InternetMessageHeaders {
		switch strings.ToLower(h.Name) {
		case "in-reply-to":
			in.InReplyTo = h.Value
		case "references":
			in.References = strings.Fields(h.Value)
		}
	}
	date := g.ReceivedDateTime
	if date == "" {
		date = g.SentDateTime
	}
	if date != "" {
		in.ReceivedAt = &date
		in.InternalDate = &date
	}
	return in
}
func (m *RemoteMailboxService) cacheMicrosoft(ctx context.Context, account, inbox string, g microsoft.Message) (RemoteMessageView, error) {
	r, e := m.Service.Store.UpsertMicrosoftMessage(ctx, account, inbox, g.ID, g.ConversationID, g.ParentFolderID, microsoftInput(g))
	if e != nil {
		return RemoteMessageView{}, e
	}
	_, e = m.Service.Store.SetRemoteMessageLabels(ctx, account, inbox, r.ID, g.Categories)
	if e != nil {
		return RemoteMessageView{}, e
	}
	r.Labels = g.Categories
	v := m.remoteView(ctx, r)
	v.FolderPath = g.ParentFolderID
	return v, nil
}
func (m *RemoteMailboxService) microsoftFolders(ctx context.Context, account, inbox, t, mailbox string) ([]model.Folder, error) {
	roles := map[string]string{}
	for well, role := range map[string]string{"inbox": model.FolderRoleInbox, "sentitems": model.FolderRoleSent, "drafts": model.FolderRoleDrafts, "deleteditems": model.FolderRoleTrash, "junkemail": model.FolderRoleSpam, "archive": model.FolderRoleArchive, "outbox": model.FolderRoleOutbox} {
		f, e := m.Microsoft.Folder(ctx, t, mailbox, well)
		if e != nil {
			var mb *model.MailboxError
			if errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
				continue
			}
			return nil, e
		}
		roles[f.ID] = role
	}
	var out []store.RemoteFolderSync
	var total int64
	queue := []string{""}
	seen := map[string]bool{}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		fs, e := m.Microsoft.Folders(ctx, t, mailbox, parent)
		if e != nil {
			return nil, e
		}
		for _, f := range fs {
			if seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			total += f.TotalItemCount
			if len(seen) > 10000 {
				return nil, model.NewMailboxError(model.ErrKindUnavailable, "Microsoft folder enumeration exceeds limit", false, nil)
			}
			role := roles[f.ID]
			if role == "" {
				role = model.FolderRoleFolder
			}
			out = append(out, store.RemoteFolderSync{Path: f.ID, Name: f.DisplayName, ParentPath: parent, Role: role, Selectable: true, IndexedAt: time.Now()})
			if f.ChildFolderCount > 0 {
				queue = append(queue, f.ID)
			}
		}
	}
	folders, err := m.Service.Store.ReconcileRemoteFolders(ctx, account, inbox, out)
	if err == nil {
		_ = m.Service.Store.SaveMetadataSyncTotal(ctx, account, inbox, total)
	}
	return folders, err
}

// reconcileMicrosoft runs one Microsoft delta pass and emits the operator-facing
// completion line. The pass reports the number of newly-detected INBOX arrivals
// so the log's "new" count is the genuinely new mail, not folder churn.
func (m *RemoteMailboxService) reconcileMicrosoft(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, error) {
	ctx = m.withPendingMicrosoftPush(ctx, account, inbox)
	start := time.Now()
	status, arrived, err := m.reconcileMicrosoftPass(ctx, account, inbox)
	result := remoteSyncResult{started: start, new: arrived, status: syncStatus(status.Status), err: err}
	if err != nil {
		result.status = "error"
	}
	m.logRemoteSync(ctx, account, inbox, result)
	if arrived > 0 {
		m.WakeMicrosoftPush()
	}
	return status, err
}

// reconcileMicrosoftQuick runs the cheap Microsoft detection pass: the Inbox
// folder's delta from its stored cursor only, with the folder tree taken from the
// cache (no Graph folder enumeration, no other folders, no prune). It emits the
// same completion line as the full pass and wakes the push worker when it finds
// new mail. The whole-mailbox reconcile (every folder, folder tree, prune)
// remains on the full-sync cadence. The caller ignores the returned status.
func (m *RemoteMailboxService) reconcileMicrosoftQuick(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, error) {
	ctx = m.withPendingMicrosoftPush(ctx, account, inbox)
	start := time.Now()
	arrived, err := m.reconcileMicrosoftQuickPass(ctx, account, inbox)
	status := "complete"
	if err != nil {
		status = "error"
	}
	result := remoteSyncResult{started: start, new: arrived, status: status, err: err}
	m.logRemoteSync(ctx, account, inbox, result)
	if arrived > 0 {
		m.WakeMicrosoftPush()
	}
	return store.RemoteIndexStatus{IndexedAt: time.Now()}, err
}

// reconcileMicrosoftQuickPass resolves the Inbox folder from the cache and
// applies its delta. A missing Inbox folder or an uninitialised cursor is a
// no-op that leaves the full reconcile to establish the baseline.
func (m *RemoteMailboxService) reconcileMicrosoftQuickPass(ctx context.Context, account, inbox string) (int, error) {
	t, c, e := m.microsoftAccess(ctx, account, inbox)
	if e != nil {
		return 0, e
	}
	f, e := m.Service.Store.RemoteFolderByRole(ctx, account, inbox, model.FolderRoleInbox)
	if e != nil {
		if errors.Is(e, store.ErrNotFound) {
			// The provider's Inbox folder has not been discovered yet; the full
			// reconcile establishes it and the delta baseline first.
			return 0, nil
		}
		return 0, e
	}
	arrived, _, _, e := m.syncMicrosoftFolderDelta(ctx, t, c, account, inbox, f)
	return arrived, e
}

// reconcileMicrosoftPass is the timed body of a Microsoft delta reconcile. It
// returns the index status, the count of newly-recorded arrivals, and any error.
func (m *RemoteMailboxService) reconcileMicrosoftPass(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, int, error) {
	t, c, e := m.microsoftAccess(ctx, account, inbox)
	if e != nil {
		return store.RemoteIndexStatus{}, 0, e
	}
	fs, e := m.microsoftFolders(ctx, account, inbox, t, c.Mailbox)
	if e != nil {
		return store.RemoteIndexStatus{}, 0, e
	}
	complete := true
	quotaHit := false
	arrived := 0
	for _, f := range fs {
		if quotaHit {
			break
		}
		a, folderComplete, qh, e := m.syncMicrosoftFolderDelta(ctx, t, c, account, inbox, f)
		arrived += a
		if e != nil {
			return store.RemoteIndexStatus{}, arrived, e
		}
		if qh {
			m.pauseMetadataSync(ctx, account, inbox)
			// Do not advance the delta cursor or prune: the page was not fully
			// indexed, so the next pass must replay it to stay consistent.
			quotaHit = true
			complete = false
			break
		}
		if !folderComplete {
			complete = false
		}
	}
	status := store.RemoteIndexPartial
	if complete && !quotaHit {
		status = store.RemoteIndexComplete
	}
	e = m.Service.Store.SetRemoteIndexStatus(ctx, account, inbox, status, "")
	return store.RemoteIndexStatus{Status: status, IndexedAt: time.Now()}, arrived, e
}

// syncMicrosoftFolderDelta applies one folder's delta page. It records INBOX
// arrivals, caches metadata, advances the folder's cursor and prunes on the final
// page. It returns the newly-recorded INBOX arrivals, whether the folder fully
// drained (no NextLink), whether a metadata quota stopped it, and any error. A
// folder the store does not own (origin != remote) is skipped.
//
// Concurrent syncs of the same inbox+folder are coalesced by an in-flight
// guard: a role-folder refresh (scheduled on a confirmed remote write) and a
// whole-mailbox reconcile can otherwise apply the same delta page at once and
// interleave their cursor writes. A call that finds the folder already being
// synced is a no-op; the live pass owns the folder and the caller's next pass
// re-derives the state.
func (m *RemoteMailboxService) syncMicrosoftFolderDelta(ctx context.Context, t string, c store.MicrosoftConnection, account, inbox string, f model.Folder) (arrived int, complete bool, quotaHit bool, err error) {
	if f.Origin != "remote" {
		return 0, true, false, nil
	}
	guard := inbox + ":" + f.ID
	m.refreshMu.Lock()
	if m.deltaSyncing[guard] {
		m.refreshMu.Unlock()
		return 0, false, false, nil
	}
	m.deltaSyncing[guard] = true
	m.refreshMu.Unlock()
	defer func() {
		m.refreshMu.Lock()
		delete(m.deltaSyncing, guard)
		m.refreshMu.Unlock()
	}()
	cursor, initialized, generation, e := m.Service.Store.MicrosoftDelta(ctx, inbox, f.Path)
	if e != nil {
		return 0, false, false, e
	}
	page, e := m.Microsoft.Delta(ctx, t, c.Mailbox, f.Path, cursor)
	if e != nil {
		var mb *model.MailboxError
		if errors.As(e, &mb) && mb.Kind == model.ErrKindConflict {
			if e = m.Service.Store.StartMicrosoftResync(ctx, inbox, f.Path); e != nil {
				return 0, false, false, e
			}
			return 0, false, false, nil
		}
		return 0, false, false, e
	}
	complete = true
	for _, g := range page.Messages {
		if g.Removed != nil {
			live, ge := m.Microsoft.Message(ctx, t, c.Mailbox, g.ID)
			if ge == nil {
				if _, ge = m.cacheMicrosoft(ctx, account, inbox, live); errors.Is(ge, store.ErrRemoteMetadataQuota) {
					return arrived, false, true, nil
				} else if ge != nil {
					return arrived, false, false, ge
				}
			} else {
				var mb *model.MailboxError
				if !errors.As(ge, &mb) || mb.Kind != model.ErrKindNotFound {
					return arrived, false, false, ge
				}
				if id, ie := m.Service.Store.MicrosoftLocalID(ctx, account, inbox, g.ID); ie == nil {
					if ie = m.Service.Store.DeleteRemoteMessage(ctx, account, inbox, id); ie != nil {
						return arrived, false, false, ie
					}
				}
			}
			continue
		}
		detected, de := m.Service.Store.MicrosoftDetected(ctx, inbox, g.ID)
		if de != nil {
			return arrived, false, false, de
		}
		if initialized && !detected && f.Role == model.FolderRoleInbox && !g.IsDraft {
			in := microsoftInput(g)
			date := parseStoreTime(g.ReceivedDateTime)
			_, inserted, ae := m.Service.Store.RecordRemoteArrival(ctx, account, inbox, store.RemoteArrivalInput{FolderPath: "microsoft:" + g.ID, RFCMessageID: in.RFCMessageID, FromName: in.FromName, FromAddress: in.FromAddress, Subject: in.Subject, InternalDate: &date, Source: m.microsoftArrivalSource(ctx, account, inbox)})
			if ae != nil {
				return arrived, false, false, ae
			}
			if inserted {
				arrived++
			}
		}
		if _, e = m.cacheMicrosoft(ctx, account, inbox, g); errors.Is(e, store.ErrRemoteMetadataQuota) {
			return arrived, false, true, nil
		} else if e != nil {
			return arrived, false, false, e
		}
		if e = m.Service.Store.MarkMicrosoftDetected(ctx, inbox, g.ID); e != nil {
			return arrived, false, false, e
		}
	}
	next := page.NextLink
	if next == "" {
		next = page.DeltaLink
	}
	if next == "" {
		return arrived, false, false, model.NewMailboxError(model.ErrKindUnavailable, "Microsoft delta cursor missing", true, nil)
	}
	if page.NextLink != "" {
		complete = false
	} else {
		initialized = true
	}
	if e = m.Service.Store.SaveMicrosoftDelta(ctx, inbox, f.Path, next, initialized, generation); e != nil {
		if errors.Is(e, store.ErrStaleCursor) {
			// A resync reset the folder while this page was in flight: the
			// cursor advanced here belongs to a superseded sweep, so the page
			// is dropped without advancing. The new sweep replays the folder
			// from the empty cursor and re-derives everything.
			return arrived, complete, false, nil
		}
		return arrived, complete, false, e
	}
	if page.NextLink == "" {
		if e = m.Service.Store.PruneMicrosoftResync(ctx, account, inbox, f.Path); e != nil {
			return arrived, complete, false, e
		}
	}
	return arrived, complete, false, nil
}
func (m *RemoteMailboxService) getMicrosoft(ctx context.Context, p model.Principal, inbox, id string) (RemoteMessageView, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	provider, e := m.Service.Store.MicrosoftMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	g, e := m.Microsoft.Message(ctx, t, c.Mailbox, provider)
	if e != nil {
		return RemoteMessageView{}, e
	}
	return m.cacheMicrosoft(ctx, p.AccountID, inbox, g)
}
func (m *RemoteMailboxService) microsoftRaw(ctx context.Context, account, inbox, id string) (string, int64, error) {
	provider, e := m.Service.Store.MicrosoftMessage(ctx, account, inbox, id)
	if e != nil {
		return "", 0, e
	}
	t, c, e := m.microsoftAccess(ctx, account, inbox)
	if e != nil {
		return "", 0, e
	}
	f, e := os.CreateTemp(m.Service.Config.DataDir, "microsoft-raw-*.eml")
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	if e = m.Microsoft.Raw(ctx, t, c.Mailbox, provider, &limitedWriter{w: f, limit: m.remoteBodyLimit()}); e != nil {
		os.Remove(f.Name())
		return "", 0, e
	}
	st, e := f.Stat()
	if e != nil {
		os.Remove(f.Name())
		return "", 0, e
	}
	return f.Name(), st.Size(), nil
}
func (m *RemoteMailboxService) modifyMicrosoft(ctx context.Context, p model.Principal, inbox, id string, changes map[string]any) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	provider, e := m.Service.Store.MicrosoftMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	if _, e = m.Microsoft.Patch(ctx, t, c.Mailbox, provider, changes); e != nil {
		return RemoteMessageView{}, e
	}
	return m.getMicrosoft(ctx, p, inbox, id)
}
func (m *RemoteMailboxService) moveMicrosoft(ctx context.Context, p model.Principal, inbox, id, dest string) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	// dest may be a folder id (the unified Labels API sends `folder:<id>`) or a
	// path. Resolve either so a label-backed move reaches the provider.
	f, e := m.Service.Store.GetFolderDestination(ctx, p.AccountID, inbox, dest)
	if e != nil {
		return RemoteMessageView{}, e
	}
	provider, e := m.Service.Store.MicrosoftMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	g, e := m.Microsoft.Move(ctx, t, c.Mailbox, provider, f.Path)
	if e != nil {
		return RemoteMessageView{}, e
	}
	return m.cacheMicrosoft(ctx, p.AccountID, inbox, g)
}
func (m *RemoteMailboxService) microsoftLabels(ctx context.Context, p model.Principal, inbox, id string, names []string, mode string) (RemoteMessageView, error) {
	v, e := m.getMicrosoft(ctx, p, inbox, id)
	if e != nil {
		return v, e
	}
	labels := v.Labels
	if mode == "set" {
		labels = names
	} else if mode == "add" {
		for _, n := range names {
			if !containsFoldApp(labels, n) {
				labels = append(labels, n)
			}
		}
	} else {
		var keep []string
		for _, n := range labels {
			if !containsFoldApp(names, n) {
				keep = append(keep, n)
			}
		}
		labels = keep
	}
	if labels == nil {
		labels = []string{}
	}
	return m.modifyMicrosoft(ctx, p, inbox, id, map[string]any{"categories": labels})
}

func (m *RemoteMailboxService) listMicrosoft(ctx context.Context, p model.Principal, inbox, folder string, limit int, before string, cached bool) (RemoteListResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteListResult{}, e
	}
	// A scoped read bootstraps the index on first use only, then serves the cache:
	// freshness is the worker's schedule (client presence drives the coalesced
	// quick pass, never a per-request full reconcile). While the index is still
	// incomplete a background refresh advances it, but a complete index adds no
	// work. This mirrors the IMAP read path. The bootstrap runs before folder
	// resolution so the provider's Inbox folder exists to resolve.
	status, serr := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, inbox)
	if serr != nil {
		return RemoteListResult{}, mapStoreError(serr)
	}
	switch {
	case !cached && (status.Status == store.RemoteIndexNeverStarted || status.IndexedAt.IsZero()):
		if _, e := m.ReconcileRemote(ctx, p.AccountID, inbox); e != nil {
			return RemoteListResult{}, e
		}
	case status.Status != store.RemoteIndexComplete:
		m.ScheduleRefresh(p.AccountID, inbox)
	}
	if folder == "" {
		f, e := m.Service.Store.GetSystemFolder(ctx, p.AccountID, inbox, model.FolderRoleInbox)
		if e != nil {
			return RemoteListResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessPartial}, nil
		}
		folder = f.Path
	}
	rows, e := m.Service.Store.ListRemoteMessagesFiltered(ctx, p.AccountID, inbox, store.RemoteMessageFilter{FolderPath: folder, Limit: limit, Before: before})
	if e != nil {
		return RemoteListResult{}, e
	}
	out := RemoteListResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessPartial}
	s, _ := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, inbox)
	if s.Status == store.RemoteIndexComplete {
		out.Completeness = model.CompletenessComplete
	}
	for _, r := range rows {
		v := m.remoteView(ctx, r)
		v.FolderPath = folder
		v.Labels, _ = m.Service.Store.RemoteMessageLabels(ctx, p.AccountID, inbox, r.ID)
		out.Items = append(out.Items, v)
	}
	if limit > 0 && len(rows) == limit {
		out.NextCursor = rows[len(rows)-1].ID
	}
	return out, nil
}
func (m *RemoteMailboxService) searchMicrosoft(ctx context.Context, p model.Principal, inbox string, q RemoteSearchQuery) (RemoteSearchResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteSearchResult{}, e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	var terms []string
	if q.Text != "" {
		terms = append(terms, q.Text)
	}
	for _, kv := range [][2]string{{"from", q.From}, {"to", q.To}, {"subject", q.Subject}} {
		if kv[1] != "" {
			terms = append(terms, kv[0]+":"+strconv.Quote(kv[1]))
		}
	}
	query := ""
	if len(terms) > 0 {
		query = strconv.Quote(strings.Join(terms, " AND "))
	}
	var resume struct {
		Page   string
		Offset int
	}
	if q.ProviderCursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.ProviderCursor)
		if e != nil {
			return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindInvalid, "Invalid Microsoft search cursor", false, nil)
		}
		if json.Unmarshal(b, &resume) != nil || resume.Offset < 0 {
			return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindInvalid, "Invalid Microsoft search cursor", false, nil)
		}
	}
	encode := func(page string, offset int) string {
		b, _ := json.Marshal(struct {
			Page   string
			Offset int
		}{page, offset})
		return base64.RawURLEncoding.EncodeToString(b)
	}
	page, e := m.Microsoft.Messages(ctx, t, c.Mailbox, q.FolderPath, query, resume.Page, q.Limit)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	out := RemoteSearchResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessComplete}
	if query != "" {
		out.Completeness = model.CompletenessPartial
	}
	if resume.Offset > len(page.Messages) {
		return out, model.NewMailboxError(model.ErrKindInvalid, "Invalid Microsoft search cursor", false, nil)
	}
	for i := resume.Offset; i < len(page.Messages); i++ {
		g := page.Messages[i]
		date := parseStoreTime(g.ReceivedDateTime)
		if q.Unread != nil && g.IsRead == *q.Unread || q.Flagged != nil && (g.Flag.FlagStatus == "flagged") != *q.Flagged || q.Label != "" && !containsFoldApp(g.Categories, q.Label) || !q.Since.IsZero() && date.Before(q.Since) || !q.Before.IsZero() && !date.Before(q.Before) {
			continue
		}
		v, e := m.cacheMicrosoft(ctx, p.AccountID, inbox, g)
		if e != nil {
			return out, e
		}
		if i+1 < len(page.Messages) {
			v.ProviderCursor = encode(resume.Page, i+1)
		} else if page.NextLink != "" {
			v.ProviderCursor = encode(page.NextLink, 0)
		}
		out.Items = append(out.Items, v)
		if q.Limit > 0 && len(out.Items) >= q.Limit {
			out.ProviderCursor = v.ProviderCursor
			return out, nil
		}
	}
	if page.NextLink != "" {
		out.ProviderCursor = encode(page.NextLink, 0)
	}
	return out, nil
}
func (m *RemoteMailboxService) microsoftFolderMutation(ctx context.Context, p model.Principal, inbox, path, name, mode string) (model.Folder, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return model.Folder{}, e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return model.Folder{}, e
	}
	var f microsoft.Folder
	if mode == "create" {
		if name == "" {
			name = path
			path = ""
		}
		if path != "" {
			parent, e := m.Service.Store.GetFolder(ctx, p.AccountID, inbox, path)
			if e != nil {
				return model.Folder{}, e
			}
			path = parent.Path
		}
		f, e = m.Microsoft.CreateFolder(ctx, t, c.Mailbox, path, name)
	} else {
		fs, e := m.Service.Store.ListFolders(ctx, p.AccountID, inbox)
		if e != nil {
			return model.Folder{}, e
		}
		var target *model.Folder
		for i := range fs {
			if fs[i].ID == path {
				target = &fs[i]
				break
			}
		}
		if target == nil {
			return model.Folder{}, store.ErrNotFound
		}
		if target.IsSystem || target.Role != model.FolderRoleFolder {
			return model.Folder{}, model.NewMailboxError(model.ErrKindForbidden, "Microsoft system folders are protected", false, nil)
		}
		if mode == "rename" {
			f, e = m.Microsoft.RenameFolder(ctx, t, c.Mailbox, target.Path, name)
		} else {
			live, le := m.Microsoft.Folder(ctx, t, c.Mailbox, target.Path)
			if le != nil {
				return model.Folder{}, le
			}
			if live.TotalItemCount > 0 || live.ChildFolderCount > 0 {
				return model.Folder{}, model.NewMailboxError(model.ErrKindConflict, "Folder is not empty", false, nil)
			}
			e = m.Microsoft.DeleteFolder(ctx, t, c.Mailbox, target.Path)
			if e == nil {
				e = m.Service.Store.DeleteRemoteFolderRow(ctx, p.AccountID, inbox, target.Path)
			}
			return model.Folder{}, e
		}
	}
	if e != nil {
		return model.Folder{}, e
	}
	if _, e = m.microsoftFolders(ctx, p.AccountID, inbox, t, c.Mailbox); e != nil {
		return model.Folder{}, e
	}
	return m.Service.Store.GetFolder(ctx, p.AccountID, inbox, f.ID)
}
func (m *RemoteMailboxService) purgeMicrosoft(ctx context.Context, p model.Principal, inbox, id string) error {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return e
	}
	v, e := m.getMicrosoft(ctx, p, inbox, id)
	if e != nil {
		return e
	}
	trash, e := m.Service.Store.GetSystemFolder(ctx, p.AccountID, inbox, model.FolderRoleTrash)
	if e != nil {
		return e
	}
	if v.FolderPath != trash.Path {
		return model.NewMailboxError(model.ErrKindConflict, "Only a trashed message can be permanently purged", false, nil)
	}
	provider, e := m.Service.Store.MicrosoftMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return e
	}
	t, c, e := m.microsoftAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return e
	}
	if e = m.Microsoft.Delete(ctx, t, c.Mailbox, provider); e != nil {
		return e
	}
	return m.Service.Store.DeleteRemoteMessage(ctx, p.AccountID, inbox, id)
}
