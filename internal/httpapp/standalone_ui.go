package httpapp

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// This file is the human-UI half of the common standalone mailbox surface: the
// folder create/rename/delete routes (session-authenticated, CSRF-protected) and
// the standalone setup form. It shares the mailboxBackend dispatch with the API,
// so a folder mutation routes to the live remote server for a standalone inbox
// and to the local store for a domain inbox. Every write is a POST with a CSRF
// token, matching the rest of the UI.

// uiInboxFolderCreate creates a folder in an inbox.
func (s *Server) uiInboxFolderCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	path := strings.TrimSpace(r.Form.Get("path"))
	name := strings.TrimSpace(r.Form.Get("name"))
	if path == "" {
		http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice="+url.QueryEscape("Folder path is required"), 303)
		return
	}
	if mb.routed {
		if _, cerr := mb.remote.CreateRemoteFolder(r.Context(), p, mb.inbox.ID, path, name); cerr != nil {
			s.uiError(w, cerr, 400)
			return
		}
	} else if _, cerr := s.Service.Store.CreateFolder(r.Context(), p, mb.inbox.ID, store.FolderCreate{Path: path, Name: name}); cerr != nil {
		s.uiError(w, cerr, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice="+url.QueryEscape("Folder created"), 303)
}

// uiInboxFolderRename renames a folder.
func (s *Server) uiInboxFolderRename(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" {
		http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice="+url.QueryEscape("Folder name is required"), 303)
		return
	}
	if mb.routed {
		if _, rerr := mb.remote.RenameRemoteFolder(r.Context(), p, mb.inbox.ID, r.PathValue("folderId"), name); rerr != nil {
			s.uiError(w, rerr, 400)
			return
		}
	} else if _, rerr := s.Service.Store.RenameFolder(r.Context(), p, mb.inbox.ID, r.PathValue("folderId"), name); rerr != nil {
		s.uiError(w, rerr, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice="+url.QueryEscape("Folder renamed"), 303)
}

// uiInboxFolderDelete deletes a folder.
func (s *Server) uiInboxFolderDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if mb.routed {
		if derr := mb.remote.DeleteRemoteFolder(r.Context(), p, mb.inbox.ID, r.PathValue("folderId")); derr != nil {
			s.uiError(w, derr, 400)
			return
		}
	} else if derr := s.Service.Store.DeleteFolder(r.Context(), p, mb.inbox.ID, r.PathValue("folderId")); derr != nil {
		s.uiError(w, derr, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+mb.inbox.ID+"?notice="+url.QueryEscape("Folder deleted"), 303)
}

// uiCreateStandalone creates a standalone inbox from the dashboard's standalone
// setup form. It requires an account Admin, matching the domain-inbox create
// rule. The remote binding is optional at creation: an operator may create the
// inbox now and configure the connector later from the inbox settings.
func (s *Server) uiCreateStandalone(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	address := strings.TrimSpace(r.Form.Get("address"))
	display := strings.TrimSpace(r.Form.Get("display"))
	namespace := strings.TrimSpace(r.Form.Get("namespace"))
	host := strings.TrimSpace(r.Form.Get("host"))
	username := strings.TrimSpace(r.Form.Get("username"))
	security := strings.TrimSpace(r.Form.Get("security"))
	port, _ := parseIntForm(r.Form.Get("port"))
	var remote *model.RemoteConnection
	if host != "" {
		remote = &model.RemoteConnection{Host: host, Port: port, Username: username, Security: security}
		smtpHost := strings.TrimSpace(r.Form.Get("smtp_host"))
		if smtpHost != "" {
			smtpPort, _ := parseIntForm(r.Form.Get("smtp_port"))
			remote.SMTP = &model.RemoteSMTP{Host: smtpHost, Port: smtpPort, Username: strings.TrimSpace(r.Form.Get("smtp_username")), Security: strings.TrimSpace(r.Form.Get("smtp_security"))}
		}
	}
	box, err := s.Service.Store.CreateStandaloneInbox(r.Context(), p.AccountID, store.StandaloneCreate{
		DisplayName: display, Address: address, Namespace: namespace, Remote: remote,
	})
	if err != nil {
		http.Redirect(w, r, "/?notice="+url.QueryEscape(err.Error()), 303)
		return
	}
	// If secrets were supplied, store them encrypted through the remote service so
	// the inbox is immediately usable and testable. A stored plaintext SMTP/IMAP
	// choice is persisted as a non-secret field, never downgraded.
	if pw := r.Form.Get("imap_password"); strings.TrimSpace(pw) != "" || strings.TrimSpace(r.Form.Get("smtp_password")) != "" {
		if _, cerr := s.remoteMailbox().ConfigureStandaloneRemote(r.Context(), p, box.ID, store.StandaloneRemoteUpdate{IMAPPassword: pw, SMTPPassword: r.Form.Get("smtp_password")}); cerr != nil {
			s.Log.Warn("standalone inbox created but credentials were not stored", "inbox_id", box.ID, "error", cerr)
		}
	}
	// An IMAP binding supplied at creation must authenticate before the inbox is
	// kept: otherwise a bad or empty connector would be left as an active inbox.
	// On failure the just-created inbox is removed so nothing is mislabeled.
	if host != "" {
		if _, terr := s.remoteMailbox().TestStandaloneRemote(r.Context(), p, box.ID, store.StandaloneRemoteUpdate{}); terr != nil {
			if paths, perr := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, box.ID); perr == nil {
				for _, path := range paths {
					s.removeDataFile(path)
				}
			}
			http.Redirect(w, r, "/?error="+url.QueryEscape("IMAP connection failed: "+terr.Error()), 303)
			return
		}
	}
	http.Redirect(w, r, "/?inbox="+box.ID+"&notice="+url.QueryEscape("Standalone inbox created"), 303)
}

// uiInboxRemoteSave saves a standalone inbox's remote connector from the inbox
// settings dialog. It requires Owner.
func (s *Server) uiInboxRemoteSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	if !p.CanOwn(inboxID) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	port, _ := parseIntForm(r.Form.Get("remote_port"))
	smtpPort, _ := parseIntForm(r.Form.Get("smtp_port"))
	in := store.StandaloneRemoteUpdate{
		Host:         r.Form.Get("remote_host"),
		Port:         port,
		Username:     r.Form.Get("remote_username"),
		Security:     r.Form.Get("remote_security"),
		SMTPHost:     r.Form.Get("smtp_host"),
		SMTPPort:     smtpPort,
		SMTPUsername: r.Form.Get("smtp_username"),
		SMTPSecurity: r.Form.Get("smtp_security"),
		ClearSMTP:    r.Form.Get("smtp_enabled") != "1",
		Namespace:    r.Form.Get("namespace"),
		IMAPPassword: r.Form.Get("imap_password"),
		SMTPPassword: r.Form.Get("smtp_password"),
	}
	if !hasFormRemoteNonSecret(in) && strings.TrimSpace(in.IMAPPassword) == "" && strings.TrimSpace(in.SMTPPassword) == "" {
		http.Redirect(w, r, "/?inbox="+inboxID+"&notice="+url.QueryEscape("No changes to save"), 303)
		return
	}
	if _, err := s.remoteMailbox().ConfigureStandaloneRemote(r.Context(), p, inboxID, in); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/?inbox="+inboxID+"&notice="+url.QueryEscape("Remote connector saved"), 303)
}

// hasFormRemoteNonSecret reports whether a submitted remote form carries any
// non-secret field, so a secrets-only save does not require the host.
func hasFormRemoteNonSecret(in store.StandaloneRemoteUpdate) bool {
	return strings.TrimSpace(in.Host) != "" || in.Port != 0 || strings.TrimSpace(in.Username) != "" ||
		strings.TrimSpace(in.Security) != "" || strings.TrimSpace(in.Namespace) != "" ||
		strings.TrimSpace(in.SMTPHost) != "" || in.SMTPPort != 0 || strings.TrimSpace(in.SMTPUsername) != ""
}

// parseIntForm parses an optional integer form field, returning 0 when blank or
// unparseable (a zero port means "use the conventional default").
func parseIntForm(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
		if n > 65535 {
			return 0, false
		}
	}
	return n, true
}

// standaloneDialogs is appended to the dashboard body: the standalone-inbox
// setup dialog. The remote binding is optional at creation; the connector can be
// configured later from the inbox settings. Gmail/Microsoft OAuth is a documented
// roadmap item, so those options are shown disabled with a note rather than
// silently hidden.
const standaloneDialogs = `
<dialog id="standalone-dialog" class="inbox-settings"><div class="inbox-settings-head"><h2>Add standalone inbox</h2><button type="button" class="secondary icon-btn standalone-close" aria-label="Close"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></div>
<form method="post" action="/ui/inboxes/standalone" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}">
<div class="inbox-settings-body"><div class="inbox-settings-panels">
<section class="dialog-panel" role="tabpanel">
<h3 class="section-head">Identity</h3>
<label>Email address</label><input name="address" placeholder="agent@example.com" required>
<label>Display name</label><input name="display" placeholder="Agent">
<h3 class="section-head">Remote server (IMAP)</h3>
<p class="muted small">Connect this inbox to an existing mailbox over IMAP. Gmail and Microsoft sign-in is coming later; configure their IMAP/SMTP with an app password for now. Leave the host blank to create the inbox now and configure the connector later.</p>
<div class="standalone-providers"><button type="button" class="secondary" disabled title="Coming later">Gmail <span class="muted small">(coming later)</span></button><button type="button" class="secondary" disabled title="Coming later">Microsoft <span class="muted small">(coming later)</span></button></div>
<label>IMAP host</label><input name="host" placeholder="imap.example.com">
<label>IMAP port</label><input name="port" type="number" min="1" max="65535" placeholder="993">
<label>IMAP username</label><input name="username" placeholder="agent@example.com">
<label>IMAP password or app password</label><input name="imap_password" type="password" autocomplete="new-password" placeholder="app password">
<label>Security</label><select name="security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select>
<p class="muted small">TLS is the default and the connection never downgrades. Plain is an explicit choice for a self-hosted server; the deployment may refuse it.</p>
<label>Sync root folder</label><input name="namespace" placeholder="INBOX">
<p class="muted small">The root folder this inbox syncs. The default personal root is INBOX plus its siblings. A missing special folder (Sent, Drafts, Trash, Spam) is mapped on first sync, or you can select or create one from the inbox settings.</p>
<h3 class="section-head">Outbound SMTP (optional)</h3>
<p class="muted small">Optional. Without an SMTP server this inbox can receive and hand off drafts but cannot send from MailMoose. Leave the host blank to configure it later.</p>
<label>SMTP host</label><input name="smtp_host" placeholder="smtp.example.com">
<label>SMTP port</label><input name="smtp_port" type="number" min="1" max="65535" placeholder="465">
<label>SMTP username</label><input name="smtp_username" placeholder="agent@example.com">
<label>SMTP security</label><select name="smtp_security"><option value="tls" selected>TLS (implicit, default)</option><option value="starttls">STARTTLS</option><option value="plain">Plain (no transport security)</option></select>
<label>SMTP password or app password</label><input name="smtp_password" type="password" autocomplete="new-password" placeholder="app password">
</section>
</div></div>
<footer class="inbox-settings-footer"><span class="spacer"></span><button type="button" class="secondary standalone-cancel">Cancel</button><button type="submit">Add standalone inbox</button></footer>
</form></dialog>`

// folderSidebarItem is one custom (non-system) folder in the mailbox sidebar. It
// carries the stable id the rename/delete forms post and a display count; system
// folders keep their dedicated sidebar entries.
type folderSidebarItem struct {
	ID    string
	Path  string
	Name  string
	Role  string
	Count int64
}

// buildFolderSidebar returns the inbox's custom folders (role "folder") in
// hierarchical order, for the common folder sidebar. System folders are excluded
// because they already have their own sidebar entries.
func (s *Server) buildFolderSidebar(ctx context.Context, accountID, inboxID string) []folderSidebarItem {
	folders, err := s.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return nil
	}
	var out []folderSidebarItem
	for _, f := range folders {
		if f.Role != model.FolderRoleFolder {
			continue
		}
		out = append(out, folderSidebarItem{ID: f.ID, Path: f.Path, Name: f.Name, Role: f.Role, Count: f.MessageCount})
	}
	return out
}

// folderMailboxView renders the messages in one custom folder of an inbox (both
// kinds). A domain inbox lists the local messages whose mailbox_id is the folder;
// a standalone inbox lists the cached remote messages in the folder's path. It
// reuses the common message list page so the same UI, bulk actions and live
// updates apply to both.
func (s *Server) folderMailboxView(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	folderID := strings.TrimSpace(r.URL.Query().Get("folder"))
	if folderID == "" {
		http.Error(w, "folder is required", 400)
		return
	}
	folders, err := s.Service.Store.ListFolders(r.Context(), p.AccountID, id)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	var target *model.Folder
	for i := range folders {
		if folders[i].ID == folderID {
			target = &folders[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "folder not found", 404)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	spamCount, _ := s.Service.Store.CountSpam(r.Context(), p, id)
	trashCount, _ := s.Service.Store.CountTrash(r.Context(), p, id)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, id)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, id)
	sidebar := s.buildFolderSidebar(r.Context(), p.AccountID, id)

	before := strings.TrimSpace(r.URL.Query().Get("before"))
	msgs, err := s.folderMessages(r, p, box, target, before)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	hasMore := len(msgs) > inboxPageSize
	if hasMore {
		msgs = msgs[:inboxPageSize]
	}
	cursor := ""
	if len(msgs) > 0 {
		cursor = msgs[len(msgs)-1].ID
	}
	pagerURL := ""
	if cursor != "" {
		pagerURL = "/ui/inboxes/" + box.ID + "/folder?folder=" + url.QueryEscape(target.ID) + "&before=" + url.QueryEscape(cursor)
	}
	totalCount, _ := s.folderMessageCount(r, p, box, target)

	data := pageData{
		Title:          box.Address + " · " + target.Name,
		Page:           "inbox",
		Principal:      p,
		CSRF:           csrf(r),
		Account:        acc,
		Inbox:          &box,
		Messages:       msgs,
		Folder:         "folder",
		ActiveFolderID: target.ID,
		Folders:        sidebar,
		MoveTargets:    s.buildMoveTargets(r.Context(), p.AccountID, box.ID, []string{target.ID}),
		UnreadCount:    unread[id],
		SpamCount:      spamCount,
		TrashCount:     trashCount,
		DraftCount:     draftCount,
		OutboxCount:    outboxCount,
		HasMore:        hasMore,
		Before:         cursor,
		PagerURL:       pagerURL,
		TotalCount:     totalCount,
		Notice:         r.URL.Query().Get("notice"),
	}
	if box.Kind == model.InboxKindStandalone {
		data.StandaloneMode = true
		data.StandaloneConfigured = box.RemoteConfigured && box.Remote != nil
		data.StandalonePlain = box.Remote != nil && box.Remote.Security == model.RemoteSecurityPlain
		data.RemoteConnectorURL = "/ui/inboxes/" + box.ID + "/remote"
		if view, verr := s.remoteConfigView(r.Context(), p, box.ID); verr == nil {
			data.RemoteConfig = view
		}
	}
	s.renderMail(w, r, folderBody, data)
}

// folderMessages lists up to inboxPageSize+1 messages in a folder. A domain
// inbox reads the local store scoped to the folder; a standalone inbox lists the
// cached remote messages in the folder's path and projects them onto the common
// message shape.
func (s *Server) folderMessages(r *http.Request, p model.Principal, box model.Inbox, folder *model.Folder, before string) ([]model.Message, error) {
	if box.Kind == model.InboxKindStandalone {
		res, err := s.remoteMailbox().ListRemoteMessages(r.Context(), p, box.ID, folder.Path, inboxPageSize+1, before)
		if err != nil {
			return nil, err
		}
		out := make([]model.Message, 0, len(res.Items))
		for _, v := range res.Items {
			out = append(out, remoteMessageToModel(v, folder))
		}
		return out, nil
	}
	return s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{
		InboxID:      box.ID,
		MailboxID:    folder.ID,
		FolderScoped: true,
		Before:       before,
		Limit:        inboxPageSize + 1,
	})
}

// folderMessageCount returns the exact number of messages in a folder, for the
// "select all N" banner.
func (s *Server) folderMessageCount(r *http.Request, p model.Principal, box model.Inbox, folder *model.Folder) (int, error) {
	if box.Kind == model.InboxKindStandalone {
		msgs, err := s.Service.Store.ListRemoteMessagesFiltered(r.Context(), p.AccountID, box.ID, store.RemoteMessageFilter{FolderPath: folder.Path})
		if err != nil {
			return 0, err
		}
		return len(msgs), nil
	}
	return s.Service.Store.CountMessages(r.Context(), p, store.MessageFilter{
		InboxID:      box.ID,
		MailboxID:    folder.ID,
		FolderScoped: true,
	})
}

// remoteMessageToModel projects a cached remote message onto the common message
// shape so the local message list template renders it unchanged.
func remoteMessageToModel(v app.RemoteMessageView, folder *model.Folder) model.Message {
	m := model.Message{
		ID:             v.ID,
		InboxID:        v.InboxID,
		ThreadID:       v.ThreadKey,
		From:           v.From,
		To:             v.To,
		CC:             v.CC,
		Subject:        v.Subject,
		SizeBytes:      v.SizeBytes,
		Read:           v.Read,
		Labels:         v.Labels,
		MailboxID:      folder.ID,
		FolderPath:     folder.Path,
		HasAttachments: v.HasAttach,
	}
	// A remote message's body lives only on the server; the list shows the cached
	// snippet, so Text stays empty and the UI renders the snippet.
	if v.ReceivedAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *v.ReceivedAt); err == nil {
			m.CreatedAt = t
			m.ReceivedAt = &t
		}
	}
	if m.CreatedAt.IsZero() && !v.IndexedAt.IsZero() {
		m.CreatedAt = v.IndexedAt
	}
	return m
}

// folderBody is the common custom-folder mailbox page. It reuses the shared
// message list template and the mailbox sidebar (with custom folder management),
// so a domain inbox and a standalone inbox render identically.
const folderBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr" data-copy="{{.Inbox.Address}}" role="button" tabindex="0" title="Click to copy">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar"><form id="bulk-form" class="bulkbar" method="post" action="/ui/inboxes/{{.Inbox.ID}}/bulk"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="folder" value="folder"><input type="hidden" name="label" value=""><input type="hidden" name="scope" value="page">{{if .Principal.Admin}}<a class="btn secondary icon-btn" href="/?inbox={{.Inbox.ID}}" title="Inbox settings" aria-label="Inbox settings">` + iconSettingsSvg + `</a>{{end}}<button name="action" value="read" class="secondary icon-btn" title="Mark read" aria-label="Mark read">` + iconMarkRead + `</button><button name="action" value="unread" class="secondary icon-btn" title="Mark unread" aria-label="Mark unread">` + iconMarkUnread + `</button><button name="action" value="delete" class="secondary icon-btn danger" data-confirm-all="Move all selected messages to trash?" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button>{{if .MoveTargets}}<select name="dest" aria-label="Move to folder" class="move-select"><option value="">Move to…</option>{{range .MoveTargets}}<option value="{{.Path}}">{{.Name}}</option>{{end}}</select><button name="action" value="move" class="secondary" title="Move to folder">Move</button>{{end}}</form></div>
<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{template "select-banner" .}}
{{template "live-list-card" .}}</div></div>`

// remoteMessagePage renders one standalone (remote) message in the common
// message reader. The remote body is fetched live, parsed, and its HTML passed
// through the same sanitizer the local reader uses; the transient raw file is
// removed afterwards. The message is marked seen on the server only when the
// viewer opens it (never on a header list). It returns false when id is not a
// cached remote message of a standalone inbox, so the caller can fall back to the
// local path.
func (s *Server) remoteMessagePage(w http.ResponseWriter, r *http.Request, p model.Principal, id string) bool {
	// Find the owning standalone inbox by scanning the account's remote index for
	// the metadata id. A remote id is opaque and not inbox-prefixed, so the inbox
	// must be resolved first; only a standalone inbox holds remote rows.
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) && !p.Admin {
			continue
		}
		view, gerr := s.remoteMailbox().GetRemoteMessage(r.Context(), p, box.ID, id)
		if gerr != nil {
			continue
		}
		s.renderRemoteMessage(w, r, p, box, view)
		return true
	}
	return false
}

// renderRemoteMessage renders one resolved remote message.
func (s *Server) renderRemoteMessage(w http.ResponseWriter, r *http.Request, p model.Principal, box model.Inbox, view app.RemoteMessageView) {
	// Fetch the body live and parse it for display. There is no offline
	// fallback: a fetch failure is surfaced (the common GET answers 503) rather
	// than rendering the cached header as if it were the message body.
	mb := mailboxBackend{srv: s, inbox: box, remote: s.remoteMailbox(), routed: true, p: p}
	text, html, ferr := s.hydrateRemoteBody(r.Context(), p, mb, view.ID)
	if ferr != nil {
		s.uiError(w, ferr, 503)
		return
	}
	// Mark seen on the server now that the viewer has opened it.
	if !view.Read {
		if updated, uerr := s.remoteMailbox().SetRemoteRead(r.Context(), p, box.ID, view.ID, true); uerr == nil {
			view = updated
		}
	}
	m := remoteMessageToModel(view, &model.Folder{ID: "", Path: view.FolderPath})
	m.Text = text
	m.HTML = html
	m.Direction = "inbound"
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	folderSidebar := s.buildFolderSidebar(r.Context(), p.AccountID, box.ID)
	title := view.Subject
	if title == "" {
		title = "(no subject)"
	}
	data := pageData{
		Title:                  title,
		Page:                   "inbox",
		Principal:              p,
		CSRF:                   csrf(r),
		Account:                acc,
		Message:                &m,
		MessageHasRemoteImages: htmlsanitize.HasRemoteImages(m.HTML),
		Inbox:                  &box,
		Folder:                 "inbox",
		Folders:                folderSidebar,
		UnreadCount:            unread[box.ID],
		Notice:                 r.URL.Query().Get("notice"),
	}
	s.applyStandaloneMailbox(r, p, box, &data)
	s.render(w, r, remoteMessageBody, data)
}

// remoteMessageBody is the remote message reader. It mirrors the local reader but
// points the reply/compose links at the standalone inbox and renders the sanitized
// body from the live fetch. It reuses the shared sidebar.
const remoteMessageBody = `<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{if not .StandaloneConfigured}}<div class="banner warn">This standalone inbox has no remote connector configured yet. <a href="{{.RemoteConnectorURL}}">Set up IMAP/SMTP</a>.</div>{{else if .StandalonePlain}}<div class="banner warn">This inbox connects to its remote server over plaintext. <a href="{{.RemoteConnectorURL}}">Review connection settings</a>.</div>{{end}}
<section class="card mail-reader"><div class="msghead"><h1>{{if .Message.Subject}}{{.Message.Subject}}{{else}}(no subject){{end}}</h1><div class="actions"><form method="post" action="/ui/messages/{{.Message.ID}}/read"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="read" value="0"><button class="secondary icon-btn" title="Mark unread" aria-label="Mark unread">` + iconMarkUnread + `</button></form></div></div>
<div class="msgmeta"><p class="muted"><b>From:</b> {{if .Message.From.Name}}{{.Message.From.Name}} &lt;{{.Message.From.Address}}&gt;{{else}}{{.Message.From.Address}}{{end}}<br><b>To:</b> {{join .Message.To ", "}}{{if .Message.CC}}<br><b>Cc:</b> {{join .Message.CC ", "}}{{end}}{{if .Inbox}} · <b>Mailbox:</b> {{.Inbox.Address}}{{end}}</p></div>
{{if .Message.Labels}}<div class="labelbar"><b>Labels:</b>{{range .Message.Labels}}<form class="labelpill" method="post" action="/ui/messages/{{$.Message.ID}}/labels"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="remove"><input type="hidden" name="label" value="{{.}}"><span>{{.}}</span><button class="labelx" title="Remove label" aria-label="Remove label">×</button></form>{{end}}</div>{{end}}
<hr>{{if .Message.HTML}}<iframe class="mailframe" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" srcdoc="{{.Message.HTML}}" data-mailframe></iframe>{{else}}<div class="msgbody">{{linkify .Message.Text}}</div>{{end}}
</section></div></div>`

// remoteMessageSetRead toggles the read flag of a standalone inbox's cached remote
// message and mirrors it to the live server. It returns false when id is not a
// cached remote message, so the caller can answer 404 for a genuine miss.
func (s *Server) remoteMessageSetRead(w http.ResponseWriter, r *http.Request, p model.Principal, id string) bool {
	read := r.Form.Get("read") == "1"
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) && !p.Admin {
			continue
		}
		if _, gerr := s.Service.Store.GetRemoteMessage(r.Context(), p.AccountID, box.ID, id); gerr != nil {
			continue
		}
		if _, uerr := s.remoteMailbox().SetRemoteRead(r.Context(), p, box.ID, id, read); uerr != nil {
			s.uiError(w, uerr, 400)
			return true
		}
		http.Redirect(w, r, "/ui/messages/"+id, 303)
		return true
	}
	return false
}

// remoteMessageSetLabels adds or removes one label on a standalone inbox's cached
// remote message. Labels are local free-text metadata on both inbox kinds, so no
// provider call is made. It returns false when id is not a cached remote message.
func (s *Server) remoteMessageSetLabels(w http.ResponseWriter, r *http.Request, p model.Principal, id string) bool {
	label := strings.TrimSpace(r.Form.Get("label"))
	action := r.Form.Get("action")
	if action != addLabelAction && action != removeLabelAction {
		return false
	}
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanAssist(box.ID) && !p.Admin {
			continue
		}
		current, lerr := s.Service.Store.RemoteMessageLabels(r.Context(), p.AccountID, box.ID, id)
		if lerr != nil {
			continue
		}
		var next []string
		switch action {
		case addLabelAction:
			next = append(append([]string{}, current...), label)
		case removeLabelAction:
			for _, l := range current {
				if !strings.EqualFold(l, label) {
					next = append(next, l)
				}
			}
		}
		if _, uerr := s.Service.Store.SetRemoteMessageLabels(r.Context(), p.AccountID, box.ID, id, next); uerr != nil {
			http.Redirect(w, r, "/ui/messages/"+id+"?notice="+url.QueryEscape("Invalid label: "+uerr.Error()), 303)
			return true
		}
		http.Redirect(w, r, "/ui/messages/"+id, 303)
		return true
	}
	return false
}

const (
	addLabelAction    = "add"
	removeLabelAction = "remove"
)

// remoteMessageHTML serves the sanitized HTML body of a standalone inbox's remote
// message, fetched live. It applies the same remote-image policy as the local
// reader (?remote=1 opts in) and never loads external images by default. It
// returns false when id is not a cached remote message.
func (s *Server) remoteMessageHTML(w http.ResponseWriter, r *http.Request, p model.Principal, id string) bool {
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) && !p.Admin {
			continue
		}
		if _, gerr := s.Service.Store.GetRemoteMessage(r.Context(), p.AccountID, box.ID, id); gerr != nil {
			continue
		}
		path, _, ferr := s.remoteMailbox().FetchRemoteRaw(r.Context(), p, box.ID, id)
		if ferr != nil {
			http.Error(w, "message unavailable", 503)
			return true
		}
		defer s.remoteMailbox().CleanupRemoteRaw(path)
		parsed, perr := mailparse.ParseFile(path)
		if perr != nil {
			http.Error(w, "message unavailable", 503)
			return true
		}
		remote := r.URL.Query().Get("remote") == "1"
		body := htmlsanitize.Sanitize(parsed.HTML)
		if !remote {
			body = htmlsanitize.StripRemoteImages(body)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		imgSrc := "img-src 'self' data:"
		if remote {
			imgSrc = "img-src 'self' https: http: data:"
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+imgSrc+"; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = io.WriteString(w, body)
		return true
	}
	return false
}

// remoteMessageAttachment serves one MIME part of a standalone inbox's remote
// message as a secure download: Content-Disposition: attachment and nosniff, the
// same guarantees the local attachment route gives. The part is addressed
// structurally by its MIME part path (?part=1.2). It returns false when id is not
// a cached remote message.
func (s *Server) remoteMessageAttachment(w http.ResponseWriter, r *http.Request, p model.Principal, id string) bool {
	part := parsePartPath(r.URL.Query().Get("part"))
	if len(part) == 0 {
		return false
	}
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) && !p.Admin {
			continue
		}
		if _, gerr := s.Service.Store.GetRemoteMessage(r.Context(), p.AccountID, box.ID, id); gerr != nil {
			continue
		}
		att, ferr := s.remoteMailbox().FetchRemoteAttachment(r.Context(), p, box.ID, id, part, r.URL.Query().Get("filename"), r.URL.Query().Get("content_type"))
		if ferr != nil {
			http.Error(w, "attachment unavailable", 503)
			return true
		}
		defer s.remoteMailbox().CleanupRemoteRaw(att.Path)
		name := att.Filename
		if name == "" {
			name = "attachment"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		f, oerr := os.Open(att.Path)
		if oerr != nil {
			http.Error(w, "attachment unavailable", 500)
			return true
		}
		defer f.Close()
		_, _ = io.Copy(w, f)
		return true
	}
	return false
}

// buildMoveTargets returns the selectable folders a bulk move can target: every
// selectable folder of the inbox (custom and role-bearing but not the implicit
// system Inbox or the current folder). It is common to both inbox kinds.
func (s *Server) buildMoveTargets(ctx context.Context, accountID, inboxID string, exclude []string) []folderSidebarItem {
	folders, err := s.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return nil
	}
	skip := map[string]bool{}
	for _, id := range exclude {
		skip[id] = true
	}
	var out []folderSidebarItem
	for _, f := range folders {
		if !f.Selectable || skip[f.ID] || f.Role == model.FolderRoleInbox {
			continue
		}
		out = append(out, folderSidebarItem{ID: f.ID, Path: f.Path, Name: f.Name, Role: f.Role, Count: f.MessageCount})
	}
	return out
}

// uiInboxAuthoringSave saves an inbox's assistant authoring settings (mode and
// notify override) from the Approvals tab. It requires Owner. The mode is
// snapshotted onto each request at creation, so a change never affects an
// in-flight request.
func (s *Server) uiInboxAuthoringSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	if !p.CanOwn(inboxID) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	mode := strings.TrimSpace(r.Form.Get("mode"))
	if mode != "" && !model.ValidAuthoringMode(mode) {
		http.Redirect(w, r, "/?inbox="+inboxID+"&error="+url.QueryEscape("Unknown authoring mode"), 303)
		return
	}
	if err := s.Service.Store.SetInboxAuthoringMode(r.Context(), p, inboxID, mode); err != nil {
		s.uiError(w, err, 400)
		return
	}
	if r.Form.Has("notify_address") {
		if err := s.Service.Store.SetInboxNotifyAddress(r.Context(), p, inboxID, strings.TrimSpace(r.Form.Get("notify_address"))); err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	http.Redirect(w, r, "/?inbox="+inboxID+"&notice="+url.QueryEscape("Approval settings saved"), 303)
}

// authoringSettingsByInbox builds the per-inbox authoring payload embedded on the
// dashboard's edit-inbox button so the Approvals tab loads and saves it. Only the
// Account Admin reaches the dashboard, so every inbox is included. It is
// secret-free.
func (s *Server) authoringSettingsByInbox(ctx context.Context, boxes []model.Inbox) map[string]string {
	out := make(map[string]string, len(boxes))
	for _, box := range boxes {
		settings, err := s.Service.Store.GetInboxAuthoringSettingsInternal(ctx, box.AccountID, box.ID)
		if err != nil {
			continue
		}
		payload := map[string]any{
			"mode":              settings.Mode,
			"default_mode":      model.DefaultAuthoringMode(box.Kind),
			"notify_address":    settings.NotifyAddress,
			"notify_overridden": settings.NotifyOverridden,
			// Approver is enabled by the EFFECTIVE mode, not the kind.
			"approver_enabled": settings.Mode == model.AuthoringMailMooseApproval,
			"standalone":       box.Kind == model.InboxKindStandalone,
		}
		b, merr := json.Marshal(payload)
		if merr != nil {
			continue
		}
		out[box.ID] = string(b)
	}
	return out
}

// composeRemoteMessage builds the compose/reply form for a standalone inbox's
// remote message from its cached metadata. The send itself is the app service's
// reply flow (integrated by the app owner); this renders the form so the operator
// can compose. It returns false when id is not a cached remote message.
func (s *Server) composeRemoteMessage(w http.ResponseWriter, r *http.Request, p model.Principal, kind, id string) bool {
	boxes, err := s.Service.Store.ListStandaloneInboxes(r.Context(), p.AccountID)
	if err != nil {
		return false
	}
	for _, box := range boxes {
		if !p.CanRead(box.ID) && !p.Admin {
			continue
		}
		v, gerr := s.remoteMailbox().GetRemoteMessage(r.Context(), p, box.ID, id)
		if gerr != nil {
			continue
		}
		m := remoteMessageToModel(v, &model.Folder{Path: v.FolderPath})
		m.Direction = "inbound"
		acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
		data := pageData{Principal: p, CSRF: csrf(r), Account: acc, ComposeCancel: "/ui/messages/" + m.ID}
		data.ComposeFromOptions, data.ComposeFrom = composeFromOptions(box, "")
		switch kind {
		case "reply":
			data.Title = "Reply"
			data.ComposeTitle = "Reply"
			data.ComposeTo = m.From.Address
			data.ComposeSubject = app.ReplySubject(m.Subject)
			data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/reply", csrf(r))
		case "reply-all":
			data.Title = "Reply all"
			data.ComposeTitle = "Reply all"
			data.ComposeTo = m.From.Address
			data.ComposeCC = strings.Join(m.CC, ", ")
			data.ComposeSubject = app.ReplySubject(m.Subject)
			data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/reply-all", csrf(r))
		case "forward":
			data.Title = "Forward"
			data.ComposeTitle = "Forward"
			data.ComposeSubject = app.ForwardSubject(m.Subject)
			data.ComposeNote = "The original message is fetched live from the remote server when forwarded."
			data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/forward", csrf(r))
		}
		s.render(w, r, composeBody, data)
		return true
	}
	return false
}

// uiInboxRemoteSettings renders the standalone inbox connection-settings page:
// the IMAP/SMTP bindings (secret-free; blank secrets keep the stored value) and
// the special-folder role mappings, showing detected folders and offering an
// explicit select-or-create for each missing role.
func (s *Server) uiInboxRemoteSettings(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if box.Kind != model.InboxKindStandalone {
		http.Error(w, "not a standalone inbox", 404)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	view, verr := s.remoteConfigView(r.Context(), p, id)
	if verr != nil {
		s.uiError(w, verr, 400)
		return
	}
	data := pageData{
		Title:                box.Address + " · Connection",
		Page:                 "inbox",
		Principal:            p,
		CSRF:                 csrf(r),
		Account:              acc,
		Inbox:                &box,
		RemoteConfig:         view,
		RemoteConnectorURL:   "/ui/inboxes/" + id + "/remote",
		StandaloneMode:       true,
		StandaloneConfigured: box.RemoteConfigured && box.Remote != nil,
		StandalonePlain:      box.Remote != nil && box.Remote.Security == model.RemoteSecurityPlain,
		Notice:               r.URL.Query().Get("notice"),
	}
	data.Folders = s.buildFolderSidebar(r.Context(), p.AccountID, id)
	if folders, lerr := s.Service.Store.ListFolders(r.Context(), p.AccountID, id); lerr == nil {
		items := make([]folderListResponse, 0, len(folders))
		for _, f := range folders {
			items = append(items, folderResponse(f))
		}
		data.RemoteFolders = items
	}
	data.RoleMappings = s.buildRoleMappings(r.Context(), p.AccountID, id)
	s.render(w, r, remoteSettingsBody, data)
}

// uiInboxRemoteRoleCreate maps a special folder role either by creating its
// conventional folder on the live server or by selecting an existing folder
// (folder_id). It returns to the connection-settings page.
func (s *Server) uiInboxRemoteRoleCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if !p.CanOwn(id) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	role := strings.TrimSpace(r.PathValue("role"))
	if _, ok := roleFolderNames[role]; !ok {
		http.Redirect(w, r, "/ui/inboxes/"+id+"/remote?notice="+url.QueryEscape("Unknown folder role"), 303)
		return
	}
	// Selecting an existing folder persists an explicit role_locked mapping and
	// requires no provider call.
	if folderID := strings.TrimSpace(r.Form.Get("folder_id")); folderID != "" {
		if _, merr := s.remoteMailbox().SetRemoteFolderRole(r.Context(), p, id, folderID, role); merr != nil {
			s.uiError(w, merr, 400)
			return
		}
		http.Redirect(w, r, "/ui/inboxes/"+id+"/remote?notice="+url.QueryEscape("Folder mapped to "+role), 303)
		return
	}
	path := strings.TrimSpace(r.Form.Get("path"))
	if path == "" {
		http.Redirect(w, r, "/ui/inboxes/"+id+"/remote?notice="+url.QueryEscape("Select a folder or a name to create"), 303)
		return
	}
	folder, cerr := s.remoteMailbox().CreateRemoteFolder(r.Context(), p, id, path, path)
	if cerr != nil {
		s.uiError(w, cerr, 400)
		return
	}
	// Lock the created folder's role explicitly so a later reconcile cannot reset
	// it by re-inferring from the name.
	if _, merr := s.remoteMailbox().SetRemoteFolderRole(r.Context(), p, id, folder.ID, role); merr != nil {
		s.uiError(w, merr, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+id+"/remote?notice="+url.QueryEscape("Folder created and mapped"), 303)
}

// uiInboxRemoteSettingsSave saves the connection fields (session+CSRF). A blank
// secret keeps the stored value; a plaintext choice is persisted as the non-secret
// security field and never silently downgraded.
func (s *Server) uiInboxRemoteSettingsSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if !p.CanOwn(id) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.uiError(w, err, 400)
		return
	}
	port, _ := parseIntForm(r.Form.Get("port"))
	smtpPort, _ := parseIntForm(r.Form.Get("smtp_port"))
	in := store.StandaloneRemoteUpdate{
		Host: r.Form.Get("host"), Port: port, Username: r.Form.Get("username"), Security: r.Form.Get("security"),
		SMTPHost: r.Form.Get("smtp_host"), SMTPPort: smtpPort, SMTPUsername: r.Form.Get("smtp_username"), SMTPSecurity: r.Form.Get("smtp_security"),
		ClearSMTP: r.Form.Get("smtp_host") == "", Namespace: r.Form.Get("namespace"),
		IMAPPassword: r.Form.Get("imap_password"), SMTPPassword: r.Form.Get("smtp_password"),
	}
	// A connector must authenticate before it is saved as active.
	if verr := s.verifyRemoteUpdate(r.Context(), p, id, in); verr != nil {
		s.uiError(w, verr, 400)
		return
	}
	box, err := s.remoteMailbox().ConfigureStandaloneRemote(r.Context(), p, id, in)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	// The Sent-copy switch is saved with the same form when present.
	if r.Form.Has("sent_copy_enabled") {
		enabled := r.Form.Get("sent_copy_enabled") == "1"
		folder := box.RemoteSentCopyFolder
		if strings.TrimSpace(r.Form.Get("sent_copy_folder")) != "" {
			folder = r.Form.Get("sent_copy_folder")
		}
		if err := s.Service.Store.SetRemoteSentCopy(r.Context(), p, id, enabled, folder); err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	http.Redirect(w, r, "/ui/inboxes/"+id+"/remote?notice="+url.QueryEscape("Connection settings saved"), 303)
}

// remoteSettingsBody is the standalone connection-settings page. It shows the
// secret-free bindings, warns about plaintext, and lists each special folder role
// with its detected folder or an explicit create control. Nothing is invented: a
// missing role shows a prompt, not a fabricate
const remoteSettingsBody = `<div class="toolbar"><a href="/ui/inboxes/{{.Inbox.ID}}">← Back to inbox</a></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
<section class="card"><h1>Connection settings</h1>
{{if .StandalonePlain}}<div class="banner warn">This inbox connects over plaintext (no transport security). TLS is the default and is never downgraded automatically; plain is an explicit choice for a self-hosted server.</div>{{end}}
<form method="post" action="/ui/inboxes/{{.Inbox.ID}}/remote/settings" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}">
<h3 class="section-head">Inbound IMAP</h3>
<label>Host</label><input name="host" value="{{.RemoteConfig.Host}}" placeholder="imap.example.com">
<label>Port</label><input name="port" type="number" min="1" max="65535" value="{{if .RemoteConfig.Port}}{{.RemoteConfig.Port}}{{end}}" placeholder="993">
<label>Username</label><input name="username" value="{{.RemoteConfig.Username}}" placeholder="agent@example.com">
<label>Password or app password{{if .RemoteConfig.IMAPPasswordSet}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input name="imap_password" type="password" autocomplete="new-password" placeholder="{{if .RemoteConfig.IMAPPasswordSet}}unchanged{{else}}app password{{end}}">
<label>Security</label><select name="security"><option value="tls"{{if eq .RemoteConfig.Security "tls"}} selected{{end}}>TLS (implicit, default)</option><option value="starttls"{{if eq .RemoteConfig.Security "starttls"}} selected{{end}}>STARTTLS</option><option value="plain"{{if eq .RemoteConfig.Security "plain"}} selected{{end}}>Plain (no transport security)</option></select>
<label>Sync root folder</label><input name="namespace" value="{{.RemoteConfig.Namespace}}" placeholder="INBOX">
<h3 class="section-head">Outbound SMTP (optional)</h3>
<p class="muted small">Without SMTP this inbox can receive and hand off drafts but cannot send. Leave the host blank to disable outbound.</p>
<label>SMTP host</label><input name="smtp_host" value="{{.RemoteConfig.SMTPHost}}" placeholder="smtp.example.com">
<label>SMTP port</label><input name="smtp_port" type="number" min="1" max="65535" value="{{if .RemoteConfig.SMTPPort}}{{.RemoteConfig.SMTPPort}}{{end}}" placeholder="465">
<label>SMTP username</label><input name="smtp_username" value="{{.RemoteConfig.SMTPUsername}}" placeholder="agent@example.com">
<label>SMTP password{{if .RemoteConfig.SMTPPasswordSet}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input name="smtp_password" type="password" autocomplete="new-password" placeholder="{{if .RemoteConfig.SMTPPasswordSet}}unchanged{{else}}app password{{end}}">
<label>SMTP security</label><select name="smtp_security"><option value="tls"{{if eq .RemoteConfig.SMTPSecurity "tls"}} selected{{end}}>TLS (implicit, default)</option><option value="starttls"{{if eq .RemoteConfig.SMTPSecurity "starttls"}} selected{{end}}>STARTTLS</option><option value="plain"{{if eq .RemoteConfig.SMTPSecurity "plain"}} selected{{end}}>Plain (no transport security)</option></select>
<h3 class="section-head">Sent copy</h3>
<p class="muted small">When enabled, a message sent from this inbox is copied into the remote Sent folder. The destination is resolved from the Sent role unless a folder is set here.</p>
<label style="display:flex;align-items:flex-start;gap:8px"><input type="checkbox" name="sent_copy_enabled" value="1"{{if .RemoteConfig.SentCopyEnabled}} checked{{end}} style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>Copy sent messages to the remote Sent folder</span></label>
<label>Sent copy folder (optional override)</label><input name="sent_copy_folder" value="{{.RemoteConfig.SentCopyFolder}}" placeholder="Defaults to the Sent role folder">
<div class="dialog-actions"><button type="submit">Save connection</button></div>
</form>
</section>
<section class="card"><h2>Special folders</h2>
<p class="muted small">Each well-known role is mapped to a folder on the remote server. A role is mapped only when a folder is selected for it; MailMoose never invents a mapping. Select an existing folder or create the conventional one.</p>
<table class="dense"><thead><tr><th>Role</th><th>Mapped folder</th><th>Map an existing folder</th><th></th></tr></thead><tbody>
{{range .RoleMappings}}<tr><td>{{.Role}}</td><td>{{if .Mapped}}{{.FolderName}} <span class="pill">mapped</span>{{else}}<span class="pill amber">missing</span>{{end}}</td>
<td><form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/remote/roles/{{.Role}}"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><select name="folder_id"><option value="">Select a folder…</option>{{range .Candidates}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select><button class="secondary btn-sm">Map</button></form></td>
<td><form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/remote/roles/{{.Role}}"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="path" value="{{.DefaultName}}"><button class="secondary btn-sm">Create {{.DefaultName}}</button></form></td></tr>{{end}}
</tbody></table>
</section>`

// roleMappingView is one special folder role in the connection-settings mapping
// table: whether it is currently mapped, which folder carries it, and the
// existing folders that could be selected for it.
type roleMappingView struct {
	Role        string
	Mapped      bool
	FolderID    string
	FolderName  string
	DefaultName string
	Candidates  []folderListResponse
}

// buildRoleMappings builds the special-folder mapping table for a standalone
// inbox: a role is mapped only when a remote-origin (or explicitly locked) folder
// carries it. Candidates are the inbox's selectable, non-protected folders the
// operator may map.
func (s *Server) buildRoleMappings(ctx context.Context, accountID, inboxID string) []roleMappingView {
	folders, err := s.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return nil
	}
	var candidates []folderListResponse
	mappedByRole := map[string]model.Folder{}
	for _, f := range folders {
		remoteMapping := f.Origin == "remote" || f.RoleLocked
		if remoteMapping && f.Role != model.FolderRoleFolder && f.Role != model.FolderRoleLabel {
			if _, exists := mappedByRole[f.Role]; !exists && strings.TrimSpace(f.Path) != "" {
				mappedByRole[f.Role] = f
			}
		}
		if f.Selectable && !f.IsSystem {
			candidates = append(candidates, folderResponse(f))
		}
	}
	out := make([]roleMappingView, 0, len(specialFolderRoles))
	for _, role := range specialFolderRoles {
		v := roleMappingView{Role: role, DefaultName: roleConventionalName(role), Candidates: candidates}
		if f, ok := mappedByRole[role]; ok {
			v.Mapped = true
			v.FolderID = f.ID
			v.FolderName = f.Name
		}
		out = append(out, v)
	}
	return out
}
