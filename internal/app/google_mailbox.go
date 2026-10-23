package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"io"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func (m *RemoteMailboxService) IsGoogle(ctx context.Context, account, inbox string) bool {
	return m.Service.Store.IsGoogle(ctx, account, inbox)
}
func GoogleAAD(account, inbox string) string { return "google:" + account + ":" + inbox }

// GoogleAccess serializes refresh with reconnect to avoid replacing a newer grant.
func (m *RemoteMailboxService) GoogleAccess(ctx context.Context, account, inbox string) (string, error) {
	m.googleMu.Lock()
	defer m.googleMu.Unlock()
	c, e := m.Service.Store.GoogleConnection(ctx, account, inbox)
	if e != nil {
		return "", e
	}
	b, e := m.Service.DecryptSecretAAD(GoogleAAD(account, inbox), c.EncryptedToken)
	if e != nil {
		return "", model.NewMailboxError(model.ErrKindAuth, "Google credentials unavailable", false, nil)
	}
	var token gmail.Token
	if json.Unmarshal(b, &token) != nil {
		return "", model.NewMailboxError(model.ErrKindAuth, "Google credentials unavailable", false, nil)
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return token.AccessToken, nil
	}
	s, e := m.Service.DecryptSecretAAD(GoogleAAD(account, inbox), c.EncryptedSecret)
	if e != nil {
		return "", e
	}
	next, e := m.Google.Refresh(ctx, c.ClientID, string(s), token.RefreshToken)
	if e != nil {
		return "", e
	}
	if next.RefreshToken == "" {
		next.RefreshToken = token.RefreshToken
	}
	if next.Scope == "" {
		next.Scope = token.Scope
	}
	if next.AccessToken == "" || next.ExpiresIn <= 0 {
		return "", model.NewMailboxError(model.ErrKindAuth, "Google returned incomplete credentials", false, nil)
	}
	b, _ = json.Marshal(next)
	c.EncryptedToken, e = m.Service.EncryptSecretAAD(GoogleAAD(account, inbox), b)
	if e != nil {
		return "", e
	}
	c.ExpiresAt = time.Now().Add(time.Duration(next.ExpiresIn) * time.Second)
	if e = m.Service.Store.SaveGoogleConnection(ctx, c); e != nil {
		return "", e
	}
	return next.AccessToken, nil
}
func (m *RemoteMailboxService) SaveGoogleGrant(ctx context.Context, account, inbox, id, secret string, t gmail.Token) error {
	m.googleMu.Lock()
	defer m.googleMu.Unlock()
	if t.RefreshToken == "" {
		return model.NewMailboxError(model.ErrKindAuth, "Google did not grant offline access; connect again with consent", false, nil)
	}
	if t.AccessToken == "" || t.ExpiresIn <= 0 || !containsFoldApp(strings.Fields(t.Scope), gmail.Scope) {
		return model.NewMailboxError(model.ErrKindAuth, "Google mailbox permission was not granted", false, nil)
	}
	aad := GoogleAAD(account, inbox)
	sec, e := m.Service.EncryptSecretAAD(aad, []byte(secret))
	if e != nil {
		return e
	}
	b, _ := json.Marshal(t)
	tok, e := m.Service.EncryptSecretAAD(aad, b)
	if e != nil {
		return e
	}
	return m.Service.Store.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: account, InboxID: inbox, ClientID: id, EncryptedSecret: sec, EncryptedToken: tok, ExpiresAt: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)})
}
func googleInput(g gmail.Message) store.RemoteMessageInput {
	h := map[string]string{}
	if g.Payload != nil {
		for _, v := range g.Payload.Headers {
			h[strings.ToLower(v.Name)] = v.Value
		}
	}
	f := model.Address{}
	if a, e := mail.ParseAddress(h["from"]); e == nil {
		f = model.Address{Name: a.Name, Address: a.Address}
	}
	addresses := func(raw string) []string {
		var out []string
		if a, e := mail.ParseAddressList(raw); e == nil {
			for _, v := range a {
				out = append(out, v.Address)
			}
		}
		return out
	}
	date := time.UnixMilli(g.InternalDate).UTC().Format(time.RFC3339Nano)
	has := false
	var walk func(gmail.Part)
	walk = func(p gmail.Part) {
		if p.Filename != "" {
			has = true
		}
		for _, v := range p.Parts {
			walk(v)
		}
	}
	if g.Payload != nil {
		walk(*g.Payload)
	}
	return store.RemoteMessageInput{RFCMessageID: h["message-id"], InReplyTo: h["in-reply-to"], References: strings.Fields(h["references"]), FromName: f.Name, FromAddress: f.Address, To: addresses(h["to"]), CC: addresses(h["cc"]), Subject: h["subject"], Snippet: g.Snippet, SizeBytes: g.SizeEstimate, Read: !containsFoldApp(g.LabelIDs, "UNREAD"), Flagged: containsFoldApp(g.LabelIDs, "STARRED"), Draft: containsFoldApp(g.LabelIDs, "DRAFT"), HasAttach: has, ReceivedAt: &date, InternalDate: &date}
}
func (m *RemoteMailboxService) cacheGoogle(ctx context.Context, account, inbox string, g gmail.Message) (RemoteMessageView, error) {
	r, e := m.Service.Store.UpsertGoogleMessage(ctx, account, inbox, g.ID, g.ThreadID, g.LabelIDs, googleInput(g))
	if e != nil {
		return RemoteMessageView{}, e
	}
	labels, e := m.Service.Store.ListFolders(ctx, account, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	var names []string
	for _, l := range labels {
		if l.Role == model.FolderRoleFolder && containsFoldApp(g.LabelIDs, l.Path) {
			names = append(names, l.Name)
		}
	}
	if _, e = m.Service.Store.SetRemoteMessageLabels(ctx, account, inbox, r.ID, names); e != nil {
		return RemoteMessageView{}, e
	}
	r.Labels = names
	return m.googleView(ctx, r), nil
}
func (m *RemoteMailboxService) googleView(ctx context.Context, r store.RemoteMessage) RemoteMessageView {
	v := m.remoteView(ctx, r)
	if v.Labels == nil {
		v.Labels, _ = m.Service.Store.RemoteMessageLabels(ctx, r.AccountID, r.InboxID, r.ID)
	}
	_, _, labels, e := m.Service.Store.GoogleMessage(ctx, r.AccountID, r.InboxID, r.ID)
	if e == nil {
		// A message can carry several system labels; its folder is the first
		// one it carries in the precedence order Trash, Spam, Drafts, Inbox,
		// Sent. A message with none of them has no folder: there is no derived
		// Archive view.
		v.FolderPath = ""
		for _, l := range []string{"SENT", "INBOX", "DRAFT", "SPAM", "TRASH"} {
			if containsFoldApp(labels, l) {
				v.FolderPath = l
			}
		}
	}
	return v
}
func (m *RemoteMailboxService) googleFolders(ctx context.Context, account, inbox, t string) error {
	client, t, e := m.GoogleMailboxClient(ctx, account, inbox)
	if e != nil {
		return e
	}
	labels, e := client.Labels(ctx, t)
	if e != nil {
		return e
	}
	var out []store.RemoteFolderSync
	roles := map[string]string{"INBOX": model.FolderRoleInbox, "SENT": model.FolderRoleSent, "DRAFT": model.FolderRoleDrafts, "TRASH": model.FolderRoleTrash, "SPAM": model.FolderRoleSpam}
	for _, l := range labels {
		role := roles[l.ID]
		if role == "" {
			if l.Type != "user" {
				continue
			}
			role = model.FolderRoleFolder
		}
		out = append(out, store.RemoteFolderSync{Path: l.ID, Name: l.Name, Role: role, Selectable: true, UIDValidity: 1, IndexedAt: time.Now()})
	}
	// Gmail has no Archive label. The record is kept as a non-selectable
	// internal row so an existing index is never pruned by a projection
	// change; it is never listed, never a move destination and never a folder
	// scope. The archive operation itself removes the Inbox label.
	out = append(out, store.RemoteFolderSync{Path: googleArchiveDestination, Name: "Archive", Role: model.FolderRoleArchive, Selectable: false, UIDValidity: 1, IndexedAt: time.Now()})
	_, e = m.Service.Store.ReconcileRemoteFolders(ctx, account, inbox, out)
	if e == nil {
		e = m.Service.Store.RefreshGoogleLabelNames(ctx, account, inbox)
	}
	return e
}

// reconcileGoogle runs one Gmail full reconcile and emits the operator-facing
// completion line. New arrivals are reported by the history detection pass
// (detectGoogle), so this deep index pass always reports new=0; its value is the
// index completeness and duration.
func (m *RemoteMailboxService) reconcileGoogle(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, error) {
	start := time.Now()
	status, err := m.reconcileGooglePass(ctx, account, inbox)
	result := remoteSyncResult{started: start, status: syncStatus(status.Status), err: err}
	if err != nil {
		result.status = "error"
	}
	m.logRemoteSync(ctx, account, inbox, result)
	return status, err
}

func (m *RemoteMailboxService) reconcileGooglePass(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, error) {
	if e := m.Service.Store.BeginGoogleRefresh(ctx, account, inbox); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, account, inbox)
	if e != nil {
		return store.RemoteIndexStatus{}, e
	}
	if e = m.googleFolders(ctx, account, inbox, t); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	c, e := m.Service.Store.GmailSync(ctx, account, inbox)
	if e != nil {
		return store.RemoteIndexStatus{}, e
	}
	if c.HistoryID == "" {
		p, e := client.Profile(ctx, t)
		if e != nil {
			return store.RemoteIndexStatus{}, e
		}
		c.HistoryID = p.HistoryID
		// Record the provider's own message count as the metadata denominator.
		// For One it is an estimate (it counts every label, while we index a
		// folder subset), so the UI clamps and frames it as approximate.
		if p.MessagesTotal > 0 {
			_ = m.Service.Store.SaveMetadataSyncTotal(ctx, account, inbox, int64(p.MessagesTotal))
		}
	}
	// Catch up history fully before committing its anchor. Bounded passes retain
	// the old anchor on interruption, making replay safe and idempotent.
	quotaHit := false
	page := ""
	if m.Service.Store.IsOne(ctx, account, inbox) {
		next, complete, err := m.drainGoogleHistory(ctx, account, inbox, "sync", c.HistoryID, client, t, func(ctx context.Context, item gmailHistoryItem) error {
			if item.Deleted {
				if id, err := m.Service.Store.GoogleLocalID(ctx, account, inbox, item.ID); err == nil {
					return m.Service.Store.DeleteRemoteMessage(ctx, account, inbox, id)
				} else if errors.Is(err, store.ErrNotFound) {
					return nil
				} else {
					return err
				}
			}
			g, err := client.Get(ctx, t, item.ID, "metadata")
			if err != nil {
				if vanished, e := m.googleMessageVanished(ctx, account, inbox, item.ID, err); vanished {
					return e
				}
				return err
			}
			_, err = m.cacheGoogle(ctx, account, inbox, g)
			return err
		})
		if err != nil {
			var expired *gmailHistoryExpired
			if errors.As(err, &expired) {
				profile, err := client.Profile(ctx, t)
				if err != nil {
					return store.RemoteIndexStatus{}, err
				}
				c.HistoryID, c.BackfillPage, c.BackfillDone = profile.HistoryID, "", false
				if err = m.Service.Store.StartGoogleResync(ctx, account, inbox, c.HistoryID); err != nil {
					return store.RemoteIndexStatus{}, err
				}
			} else {
				return store.RemoteIndexStatus{}, err
			}
		} else if !complete {
			return store.RemoteIndexStatus{Status: store.RemoteIndexPartial}, nil
		} else {
			c.HistoryID = next
		}
	} else {
		for n := 0; n < 20 && !quotaHit; n++ {
			h, e := client.History(ctx, t, c.HistoryID, page)
			if e != nil {
				var mb *model.MailboxError
				if errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
					p, pe := client.Profile(ctx, t)
					if pe != nil {
						return store.RemoteIndexStatus{}, pe
					}
					c.HistoryID = p.HistoryID
					c.BackfillDone = false
					c.BackfillPage = ""
					if e = m.Service.Store.StartGoogleResync(ctx, account, inbox, c.HistoryID); e != nil {
						return store.RemoteIndexStatus{}, e
					}
					break
				}
				return store.RemoteIndexStatus{}, e
			}
			changed := map[string]bool{}
			deleted := map[string]bool{}
			for _, v := range h.History {
				for _, a := range v.MessagesDeleted {
					deleted[a.Message.ID] = true
				}
				for _, set := range [][]gmail.HistoryMessage{v.MessagesAdded, v.LabelsAdded, v.LabelsRemoved} {
					for _, a := range set {
						changed[a.Message.ID] = true
					}
				}
			}
			for id := range deleted {
				if local, e := m.Service.Store.GoogleLocalID(ctx, account, inbox, id); e == nil {
					if e = m.Service.Store.DeleteRemoteMessage(ctx, account, inbox, local); e != nil {
						return store.RemoteIndexStatus{}, e
					}
				}
			}
			for id := range changed {
				if deleted[id] {
					continue
				}
				g, e := client.Get(ctx, t, id, "metadata")
				if e != nil {
					// Gmail deletes a message's history entry asynchronously, so a
					// history window can still name a message that is already gone.
					// Treat it as vanished, not as a failed pass.
					if vanished, ve := m.googleMessageVanished(ctx, account, inbox, id, e); vanished {
						if ve != nil {
							return store.RemoteIndexStatus{}, ve
						}
						continue
					}
					return store.RemoteIndexStatus{}, e
				}
				if _, e = m.cacheGoogle(ctx, account, inbox, g); errors.Is(e, store.ErrRemoteMetadataQuota) {
					quotaHit = true
					break
				} else if e != nil {
					return store.RemoteIndexStatus{}, e
				}
			}
			page = h.NextPageToken
			if page == "" {
				c.HistoryID = h.HistoryID
				break
			}
			if n == 19 && !quotaHit {
				return store.RemoteIndexStatus{}, model.NewMailboxError(model.ErrKindUnavailable, "Google history catch-up is still in progress", true, nil)
			}
		}
	}
	if !c.BackfillDone && !quotaHit {
		limit := 100
		one := m.Service.Store.IsOne(ctx, account, inbox)
		if one {
			limit = 10
		}
		// An optional history horizon keeps a bounded backfill inside One's
		// account-wide request quota. An empty horizon indexes everything.
		after, _ := m.Service.Store.GmailBackfillAfter(ctx, account, inbox)
		query := ""
		if after != "" {
			query = "after:" + after
		}
		var p gmail.MessagePage
		if one && c.BackfillSnapshot != "" {
			e = json.Unmarshal([]byte(c.BackfillSnapshot), &p)
		} else {
			p, e = client.ListIncludingSpamTrash(ctx, t, query, "", c.BackfillPage, limit)
			if e == nil && one {
				b, err := json.Marshal(p)
				if err != nil {
					return store.RemoteIndexStatus{}, err
				}
				e = m.Service.Store.SaveGmailBackfillSnapshot(ctx, account, inbox, string(b))
				c.BackfillOffset = 0
			}
		}
		if e != nil {
			return store.RemoteIndexStatus{}, e
		}
		for i, v := range p.Messages {
			if one && i < c.BackfillOffset {
				continue
			}
			g, e := client.Get(ctx, t, v.ID, "metadata")
			if e != nil {
				var mb *model.MailboxError
				if one && errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
					if e = m.Service.Store.SaveGmailBackfillOffset(ctx, account, inbox, i+1); e != nil {
						return store.RemoteIndexStatus{}, e
					}
					continue
				}
				return store.RemoteIndexStatus{}, e
			}
			if _, e = m.cacheGoogle(ctx, account, inbox, g); errors.Is(e, store.ErrRemoteMetadataQuota) {
				quotaHit = true
				break
			} else if e != nil {
				return store.RemoteIndexStatus{}, e
			}
			if one {
				if e = m.Service.Store.SaveGmailBackfillOffset(ctx, account, inbox, i+1); e != nil {
					return store.RemoteIndexStatus{}, e
				}
			}
		}
		if !quotaHit {
			c.BackfillPage = p.NextPageToken
			c.BackfillDone = c.BackfillPage == ""
		}
	}
	if e = m.Service.Store.SaveGoogleProgress(ctx, account, inbox, c.HistoryID, c.BackfillPage, c.BackfillDone); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	// A cap-truncated pass must not prune (its enumeration is incomplete) nor
	// claim completeness; the index stays partial until room frees up.
	if c.BackfillDone && !quotaHit {
		if e = m.Service.Store.PruneGoogleResync(ctx, account, inbox); e != nil {
			return store.RemoteIndexStatus{}, e
		}
	}
	status := store.RemoteIndexPartial
	if quotaHit {
		m.pauseMetadataSync(ctx, account, inbox)
	}
	if c.BackfillDone && !quotaHit {
		status = store.RemoteIndexComplete
	}
	e = m.Service.Store.SetRemoteIndexStatus(ctx, account, inbox, status, "")
	return store.RemoteIndexStatus{Status: status, IndexedAt: time.Now()}, e
}

// googleMessageVanished reports whether a messages.get failure is Gmail's
// not_found for a message that left the mailbox between the history/list snapshot
// and the fetch. That is a benign race, not a failed pass: a history window can
// still name a message Gmail has already deleted. When it is one, any stale cached
// row is dropped and the caller skips the message.
func (m *RemoteMailboxService) googleMessageVanished(ctx context.Context, account, inbox, providerID string, err error) (bool, error) {
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindNotFound {
		return false, nil
	}
	id, e := m.Service.Store.GoogleLocalID(ctx, account, inbox, providerID)
	if e != nil {
		return true, nil
	}
	e = m.Service.Store.DeleteRemoteMessage(ctx, account, inbox, id)
	if errors.Is(e, store.ErrNotFound) {
		return true, nil
	}
	return true, e
}

// googleArchiveDestination is the internal handle of the Gmail archive
// operation: it removes the Inbox label. It is not a folder and never appears
// in a folder list or a folder filter.
const googleArchiveDestination = "ARCHIVE"

// googleFolderScope resolves a folder scope to the Gmail system folder it
// names. A client may address a folder by its stable id or by the provider's
// label id; a user label is an additive label, not a folder, and selects
// nothing.
func (m *RemoteMailboxService) googleFolderScope(ctx context.Context, accountID, inboxID, scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return ""
	}
	for _, label := range store.GoogleFolderPrecedence {
		if label == scope {
			return scope
		}
	}
	if f, e := m.Service.Store.GetFolderByID(ctx, accountID, inboxID, scope); e == nil {
		return f.Path
	}
	if f, e := m.Service.Store.GetFolder(ctx, accountID, inboxID, scope); e == nil {
		return f.Path
	}
	// An unknown scope selects nothing rather than widening the query.
	return "none:"
}

func (m *RemoteMailboxService) listGoogle(ctx context.Context, p model.Principal, inbox, folder string, limit int, before string, cached bool) (RemoteListResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteListResult{}, e
	}
	// An empty scope is the whole mailbox, including the mail that carries no
	// system label. A folder scope is a system folder view; a user label is an
	// additive label and selects no folder. Either way Trash and Spam are
	// excluded by default, so every inbox kind's plain read means the same thing.
	folder = m.googleFolderScope(ctx, p.AccountID, inbox, folder)
	if cached {
		// UI navigation serves the cache. Advance an initial or partial index
		// in the background, but do not start a deep reconcile on every folder
		// open once indexing is complete; freshness then belongs to the worker's
		// periodic cadence, matching Microsoft and IMAP.
		status, e := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, inbox)
		if e != nil {
			return RemoteListResult{}, mapStoreError(e)
		}
		if status.Status != store.RemoteIndexComplete {
			m.ScheduleRefresh(p.AccountID, inbox)
		}
	} else {
		if _, e := m.ReconcileRemote(ctx, p.AccountID, inbox); e != nil {
			return RemoteListResult{}, e
		}
	}
	r, e := m.Service.Store.ListRemoteMessagesFiltered(ctx, p.AccountID, inbox, store.RemoteMessageFilter{FolderPath: folder, ExcludeSpam: folder == "", ExcludeTrash: folder == "", Limit: limit, Before: before})
	if e != nil {
		return RemoteListResult{}, e
	}
	out := RemoteListResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessPartial}
	s, _ := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, inbox)
	if s.Status == store.RemoteIndexComplete {
		out.Completeness = model.CompletenessComplete
	}
	for _, v := range r {
		names, _ := m.Service.Store.RemoteMessageLabels(ctx, p.AccountID, inbox, v.ID)
		v.Labels = names
		out.Items = append(out.Items, m.googleView(ctx, v))
	}
	if limit > 0 && len(r) == limit {
		out.NextCursor = r[len(r)-1].ID
	}
	return out, nil
}
func (m *RemoteMailboxService) getGoogle(ctx context.Context, p model.Principal, inbox, id string) (RemoteMessageView, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	provider, _, _, e := m.Service.Store.GoogleMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	g, e := client.Get(ctx, t, provider, "metadata")
	if e != nil {
		return RemoteMessageView{}, e
	}
	return m.cacheGoogle(ctx, p.AccountID, inbox, g)
}
func (m *RemoteMailboxService) googleRaw(ctx context.Context, account, inbox, id string) (string, int64, error) {
	provider, _, _, e := m.Service.Store.GoogleMessage(ctx, account, inbox, id)
	if e != nil {
		return "", 0, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, account, inbox)
	if e != nil {
		return "", 0, e
	}
	f, e := os.CreateTemp(m.Service.Config.DataDir, "google-raw-*.eml")
	if e != nil {
		return "", 0, e
	}
	path := f.Name()
	defer f.Close()
	w := &limitedWriter{w: f, limit: m.remoteBodyLimit()}
	if e = client.Raw(ctx, t, provider, w); e != nil {
		os.Remove(path)
		return "", 0, e
	}
	info, e := f.Stat()
	if e != nil {
		os.Remove(path)
		return "", 0, e
	}
	return path, info.Size(), nil
}
func (m *RemoteMailboxService) modifyGoogle(ctx context.Context, p model.Principal, inbox, id string, add, remove []string) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	provider, _, _, e := m.Service.Store.GoogleMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	if _, e = client.Modify(ctx, t, provider, add, remove); e != nil {
		return RemoteMessageView{}, e
	}
	return m.getGoogle(ctx, p, inbox, id)
}

func (m *RemoteMailboxService) moveGoogle(ctx context.Context, p model.Principal, inbox, id, dest string) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	// The archive is an operation, not a folder: it removes the Inbox label.
	if dest == googleArchiveDestination {
		return m.modifyGoogle(ctx, p, inbox, id, nil, []string{"INBOX"})
	}
	f, e := m.Service.Store.GetFolder(ctx, p.AccountID, inbox, dest)
	if e != nil {
		return RemoteMessageView{}, e
	}
	switch f.Role {
	case model.FolderRoleInbox, model.FolderRoleTrash, model.FolderRoleSpam:
	case model.FolderRoleSent, model.FolderRoleDrafts:
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindUnsupported, "Gmail Sent and Drafts are managed by Google", false, nil)
	default:
		// A user label is an additive label, not a location.
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindUnsupported, "a Gmail user label is a label, not a move destination", false, nil)
	}
	remove := []string{"INBOX"}
	if dest == "INBOX" {
		remove = []string{"TRASH", "SPAM"}
	}
	if dest == "TRASH" {
		remove = []string{"INBOX", "SPAM"}
	}
	if dest == "SPAM" {
		remove = []string{"INBOX", "TRASH"}
	}
	return m.modifyGoogle(ctx, p, inbox, id, []string{dest}, remove)
}
func (m *RemoteMailboxService) googleLabels(ctx context.Context, p model.Principal, inbox, id string, names []string, mode string) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	labels, e := client.Labels(ctx, t)
	if e != nil {
		return RemoteMessageView{}, e
	}
	_, _, current, e := m.Service.Store.GoogleMessage(ctx, p.AccountID, inbox, id)
	if e != nil {
		return RemoteMessageView{}, e
	}
	var add, remove []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := ""
		for _, l := range labels {
			if l.Type == "user" && strings.EqualFold(l.Name, name) {
				found = l.ID
				break
			}
		}
		if found == "" && mode != "remove" {
			l, e := client.CreateLabel(ctx, t, name)
			if e != nil {
				return RemoteMessageView{}, e
			}
			found = l.ID
		}
		if found != "" {
			if mode == "remove" {
				remove = append(remove, found)
			} else {
				add = append(add, found)
			}
		}
	}
	if mode == "set" {
		for _, l := range labels {
			if l.Type == "user" && containsFoldApp(current, l.ID) && !containsFoldApp(add, l.ID) {
				remove = append(remove, l.ID)
			}
		}
	}
	if e = m.googleFolders(ctx, p.AccountID, inbox, t); e != nil {
		return RemoteMessageView{}, e
	}
	return m.modifyGoogle(ctx, p, inbox, id, add, remove)
}
func (m *RemoteMailboxService) googleFolderMutation(ctx context.Context, p model.Principal, inbox, path, name, mode string) (model.Folder, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return model.Folder{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inbox)
	if e != nil {
		return model.Folder{}, e
	}
	var l gmail.Label
	if mode == "create" {
		if name == "" {
			name = path
		}
		l, e = client.CreateLabel(ctx, t, name)
	} else {
		fs, e := m.Service.Store.ListFolders(ctx, p.AccountID, inbox)
		if e != nil {
			return model.Folder{}, e
		}
		var f *model.Folder
		for i := range fs {
			if fs[i].ID == path {
				f = &fs[i]
				break
			}
		}
		if f == nil {
			return model.Folder{}, store.ErrNotFound
		}
		if f.Role != model.FolderRoleFolder {
			return model.Folder{}, model.NewMailboxError(model.ErrKindForbidden, "Gmail system labels are protected", false, nil)
		}
		if mode == "rename" {
			l, e = client.RenameLabel(ctx, t, f.Path, name)
		} else {
			e = client.DeleteLabel(ctx, t, f.Path)
			l.ID = f.Path
		}
	}
	if e != nil {
		return model.Folder{}, e
	}
	if e = m.googleFolders(ctx, p.AccountID, inbox, t); e != nil {
		return model.Folder{}, e
	}
	if mode == "delete" {
		return model.Folder{}, nil
	}
	return m.Service.Store.GetFolder(ctx, p.AccountID, inbox, l.ID)
}
func (m *RemoteMailboxService) googleAttachment(ctx context.Context, account, inbox, id string, part []int, filename, ct string) (RemoteAttachment, error) {
	path, _, e := m.fetchRemoteRawFor(ctx, account, inbox, id, nil)
	if e != nil {
		return RemoteAttachment{}, e
	}
	defer m.CleanupRemoteRaw(path)
	f, e := os.CreateTemp(m.Service.Config.DataDir, "google-att-*")
	if e != nil {
		return RemoteAttachment{}, e
	}
	defer f.Close()
	var parts []string
	for _, v := range part {
		parts = append(parts, strconv.Itoa(v))
	}
	want := strings.Join(parts, ".")
	found := false
	e = mailparse.ExtractAllAttachments(path, func(a mailparse.Attachment, r io.Reader) error {
		if a.PartPath != want {
			return nil
		}
		found = true
		_, err := io.Copy(&limitedWriter{w: f, limit: m.remoteBodyLimit()}, r)
		return err
	})
	if e == nil && !found {
		e = store.ErrNotFound
	}
	if e != nil {
		os.Remove(f.Name())
		return RemoteAttachment{}, e
	}
	st, e := f.Stat()
	if e != nil {
		os.Remove(f.Name())
		return RemoteAttachment{}, e
	}
	return RemoteAttachment{Path: f.Name(), Filename: filename, ContentType: ct, Size: st.Size()}, nil
}
