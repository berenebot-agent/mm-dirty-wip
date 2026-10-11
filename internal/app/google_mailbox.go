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
	v := remoteView(r)
	if v.Labels == nil {
		v.Labels, _ = m.Service.Store.RemoteMessageLabels(ctx, r.AccountID, r.InboxID, r.ID)
	}
	_, _, labels, e := m.Service.Store.GoogleMessage(ctx, r.AccountID, r.InboxID, r.ID)
	if e == nil {
		v.FolderPath = "ARCHIVE"
		for _, l := range []string{"INBOX", "SENT", "DRAFT", "SPAM", "TRASH"} {
			if containsFoldApp(labels, l) {
				v.FolderPath = l
			}
		}
	}
	return v
}
func (m *RemoteMailboxService) googleFolders(ctx context.Context, account, inbox, t string) error {
	labels, e := m.Google.Labels(ctx, t)
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
	out = append(out, store.RemoteFolderSync{Path: "ARCHIVE", Name: "Archive", Role: model.FolderRoleArchive, Selectable: true, UIDValidity: 1, IndexedAt: time.Now()})
	_, e = m.Service.Store.ReconcileRemoteFolders(ctx, account, inbox, out)
	if e == nil {
		e = m.Service.Store.RefreshGoogleLabelNames(ctx, account, inbox)
	}
	return e
}
func (m *RemoteMailboxService) reconcileGoogle(ctx context.Context, account, inbox string) (store.RemoteIndexStatus, error) {
	if e := m.Service.Store.BeginGoogleRefresh(ctx, account, inbox); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	t, e := m.GoogleAccess(ctx, account, inbox)
	if e != nil {
		return store.RemoteIndexStatus{}, e
	}
	if e = m.googleFolders(ctx, account, inbox, t); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	c, e := m.Service.Store.GoogleConnection(ctx, account, inbox)
	if e != nil {
		return store.RemoteIndexStatus{}, e
	}
	if c.HistoryID == "" {
		p, e := m.Google.Profile(ctx, t)
		if e != nil {
			return store.RemoteIndexStatus{}, e
		}
		c.HistoryID = p.HistoryID
	}
	// Catch up history fully before committing its anchor. Bounded passes retain
	// the old anchor on interruption, making replay safe and idempotent.
	page := ""
	for n := 0; n < 20; n++ {
		h, e := m.Google.History(ctx, t, c.HistoryID, page)
		if e != nil {
			var mb *model.MailboxError
			if errors.As(e, &mb) && mb.Kind == model.ErrKindNotFound {
				p, pe := m.Google.Profile(ctx, t)
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
			g, e := m.Google.Get(ctx, t, id, "metadata")
			if e != nil {
				return store.RemoteIndexStatus{}, e
			}
			if _, e = m.cacheGoogle(ctx, account, inbox, g); e != nil {
				return store.RemoteIndexStatus{}, e
			}
		}
		page = h.NextPageToken
		if page == "" {
			c.HistoryID = h.HistoryID
			break
		}
		if n == 19 {
			return store.RemoteIndexStatus{}, model.NewMailboxError(model.ErrKindUnavailable, "Google history catch-up is still in progress", true, nil)
		}
	}
	if !c.BackfillDone {
		p, e := m.Google.List(ctx, t, "", "", c.BackfillPage, 100)
		if e != nil {
			return store.RemoteIndexStatus{}, e
		}
		for _, v := range p.Messages {
			g, e := m.Google.Get(ctx, t, v.ID, "metadata")
			if e != nil {
				return store.RemoteIndexStatus{}, e
			}
			if _, e = m.cacheGoogle(ctx, account, inbox, g); e != nil {
				return store.RemoteIndexStatus{}, e
			}
		}
		c.BackfillPage = p.NextPageToken
		c.BackfillDone = c.BackfillPage == ""
	}
	if e = m.Service.Store.SaveGoogleProgress(ctx, account, inbox, c.HistoryID, c.BackfillPage, c.BackfillDone); e != nil {
		return store.RemoteIndexStatus{}, e
	}
	if c.BackfillDone {
		if e = m.Service.Store.PruneGoogleResync(ctx, account, inbox); e != nil {
			return store.RemoteIndexStatus{}, e
		}
	}
	status := store.RemoteIndexPartial
	if c.BackfillDone {
		status = store.RemoteIndexComplete
	}
	e = m.Service.Store.SetRemoteIndexStatus(ctx, account, inbox, status, "")
	return store.RemoteIndexStatus{Status: status, IndexedAt: time.Now()}, e
}
func (m *RemoteMailboxService) listGoogle(ctx context.Context, p model.Principal, inbox, folder string, limit int, before string, cached bool) (RemoteListResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteListResult{}, e
	}
	if folder == "" {
		folder = "INBOX"
	}
	if cached {
		m.ScheduleRefresh(p.AccountID, inbox)
	} else {
		if _, e := m.ReconcileRemote(ctx, p.AccountID, inbox); e != nil {
			return RemoteListResult{}, e
		}
	}
	r, e := m.Service.Store.ListRemoteMessagesFiltered(ctx, p.AccountID, inbox, store.RemoteMessageFilter{FolderPath: folder, Limit: limit, Before: before})
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
	t, e := m.GoogleAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	g, e := m.Google.Get(ctx, t, provider, "metadata")
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
	t, e := m.GoogleAccess(ctx, account, inbox)
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
	if e = m.Google.Raw(ctx, t, provider, w); e != nil {
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
	t, e := m.GoogleAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	if _, e = m.Google.Modify(ctx, t, provider, add, remove); e != nil {
		return RemoteMessageView{}, e
	}
	return m.getGoogle(ctx, p, inbox, id)
}

func (m *RemoteMailboxService) moveGoogle(ctx context.Context, p model.Principal, inbox, id, dest string) (RemoteMessageView, error) {
	if _, e := m.authorizeAssist(ctx, p, inbox); e != nil {
		return RemoteMessageView{}, e
	}
	f, e := m.Service.Store.GetFolder(ctx, p.AccountID, inbox, dest)
	if e != nil {
		return RemoteMessageView{}, e
	}
	if dest == "ARCHIVE" {
		return m.modifyGoogle(ctx, p, inbox, id, nil, []string{"INBOX"})
	}
	if f.Role == model.FolderRoleSent || f.Role == model.FolderRoleDrafts {
		return RemoteMessageView{}, model.NewMailboxError(model.ErrKindUnsupported, "Gmail Sent and Drafts are managed by Google", false, nil)
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
	t, e := m.GoogleAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteMessageView{}, e
	}
	labels, e := m.Google.Labels(ctx, t)
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
			l, e := m.Google.CreateLabel(ctx, t, name)
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
	t, e := m.GoogleAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return model.Folder{}, e
	}
	var l gmail.Label
	if mode == "create" {
		if name == "" {
			name = path
		}
		l, e = m.Google.CreateLabel(ctx, t, name)
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
			l, e = m.Google.RenameLabel(ctx, t, f.Path, name)
		} else {
			e = m.Google.DeleteLabel(ctx, t, f.Path)
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
	path, _, e := m.googleRaw(ctx, account, inbox, id)
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
