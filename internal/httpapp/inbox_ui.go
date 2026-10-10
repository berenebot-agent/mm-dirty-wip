package httpapp

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
)

const inboxPageSize = 50

// SendRequestRow joins a send request with its draft so the inbox
// send-requests table can show To/Subject/body/Date/Size like messages.
// SizeBytes is computed server-side like message sizes are stored.
type SendRequestRow struct {
	Request   model.DraftSendRequest
	Draft     model.Draft
	SizeBytes int64
}

// draftDisplaySize approximates a draft's size for the UI: subject, bodies,
// plus attachment bytes.
func draftDisplaySize(d model.Draft, attachments []model.DraftAttachment) int64 {
	var size int64
	size += int64(len(d.Subject) + len(d.Text) + len(d.HTML))
	for _, a := range attachments {
		size += a.Size
	}
	return size
}

// Mail action icons shared across the mailbox list, bulk bar and message view.
// Each is a 16-unit stroke glyph so it renders consistently via .icon-btn.
const (
	iconMarkRead    = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1.8 6.2 8 2l6.2 4.2v6.3a1 1 0 0 1-1 1H2.8a1 1 0 0 1-1-1z"/><path d="m1.8 6.2 4 3.1a1 1 0 0 0 1.2 0l4-3.1"/></svg>`
	iconMarkUnread  = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><rect x="1.8" y="3.4" width="12.4" height="9.2" rx="1.1"/><path d="m1.8 4.3 5.6 4.1a1 1 0 0 0 1.2 0l5.6-4.1"/></svg>`
	iconTrash       = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 4.5h10"/><path d="M6 4.5V3.3a1 1 0 0 1 1-1h2a1 1 0 0 1 1 1v1.2"/><path d="M4.3 4.5 5 12.6a1 1 0 0 0 1 .9h4a1 1 0 0 0 1-.9l.7-8.1"/><path d="M6.7 7v3.6M9.3 7v3.6"/></svg>`
	iconRestore     = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 8a5 5 0 1 0 1.5-3.5"/><path d="M3 3v3h3"/></svg>`
	iconDeleteFore  = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M3 4.5h10"/><path d="M6 4.5V3.3a1 1 0 0 1 1-1h2a1 1 0 0 1 1 1v1.2"/><path d="M4.3 4.5 5 12.6a1 1 0 0 0 1 .9h4a1 1 0 0 0 1-.9l.7-8.1"/><path d="M6 6.5l4 5M10 6.5l-4 5"/></svg>`
	iconNotSpam     = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M8 2.5 3.2 8.2h2.4L4.2 12.5l3.8-1.1 3.8 1.1-1.4-4.3h2.4L8 2.5Z"/></svg>`
	iconReply       = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M6.5 3.5 2.5 7.2l4 3.7"/><path d="M2.5 7.2h6.3a4.7 4.7 0 0 1 4.7 4.7v.6"/></svg>`
	iconReplyAll    = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M5.7 3.4 1.9 7.2l3.8 3.8"/><path d="M9.4 3.4 5.6 7.2l3.8 3.8"/><path d="M1.9 7.2h7a4.7 4.7 0 0 1 4.7 4.7v.6"/></svg>`
	iconForward     = `<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="m9.5 3.5 4 3.7-4 3.7"/><path d="M13.5 7.2H7.2a4.7 4.7 0 0 0-4.7 4.7v.6"/></svg>`
	iconSettingsSvg = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>`
)

// mailSidebar is the shared left-hand folder navigation for mailbox views.
// Compose sits at the very top; folders follow with their counts, then a
// Labels section listing the inbox's labels one level in. It relies on
// .Inbox, .Folder, .ActiveLabel, .Labels and the count fields being populated
// on pageData.
const mailSidebar = `<aside class="mailnav" data-sidebar data-inbox="{{.Inbox.ID}}"><a class="btn compose" href="/ui/inboxes/{{.Inbox.ID}}/compose">Compose</a><a class="folder{{if eq .Folder "inbox"}} active{{end}}" data-folder="inbox" href="/ui/inboxes/{{.Inbox.ID}}">Inbox{{if .UnreadCount}} <span class="count unread" data-count="unread">{{.UnreadCount}}</span>{{end}}</a><a class="folder{{if eq .Folder "drafts"}} active{{end}}" data-folder="drafts" href="/ui/inboxes/{{.Inbox.ID}}/drafts">Drafts{{if .DraftCount}} <span class="count" data-count="drafts">{{.DraftCount}}</span>{{end}}</a><a class="folder{{if eq .Folder "sent"}} active{{end}}" data-folder="sent" href="/ui/inboxes/{{.Inbox.ID}}/sent">Sent</a><a class="folder{{if eq .Folder "outbox"}} active{{end}}" data-folder="outbox" href="/ui/inboxes/{{.Inbox.ID}}/outbox">Outbox{{if .OutboxCount}} <span class="count" data-count="outbox">{{.OutboxCount}}</span>{{end}}</a>{{if .Folders}}{{range .Folders}}<a class="folder{{if eq $.ActiveFolderID .ID}} active{{end}}" data-folder="folder" href="/ui/inboxes/{{$.Inbox.ID}}/folder?folder={{.ID | querystring}}">{{.Name}}{{if .Count}} <span class="count">{{.Count}}</span>{{end}}</a>{{end}}{{end}}{{if .Labels}}<div class="navgroup">Labels</div>{{range .Labels}}<a class="folder label{{if eq $.ActiveLabel .}} active{{end}}" data-folder="label" data-label="{{.}}" href="/ui/inboxes/{{$.Inbox.ID}}/label?name={{querystring .}}"><span class="labelname">{{.}}</span>{{with index $.LabelUnread .}} <span class="count unread" data-count="label">{{.}}</span>{{end}}</a>{{end}}{{end}}<a class="folder{{if eq .Folder "trash"}} active{{end}}" data-folder="trash" href="/ui/inboxes/{{.Inbox.ID}}/trash">Trash{{if .TrashCount}} <span class="count" data-count="trash">{{.TrashCount}}</span>{{end}}</a><a class="folder{{if eq .Folder "spam"}} active{{end}}" data-folder="spam" href="/ui/inboxes/{{.Inbox.ID}}/spam">Spam{{if .SpamCount}} <span class="count" data-count="spam">{{.SpamCount}}</span>{{end}}</a><details class="navfolders"><summary>Manage folders</summary><form method="post" action="/ui/inboxes/{{.Inbox.ID}}/folders" class="folder-create"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input name="path" placeholder="New folder" aria-label="New folder path" required><button class="btn-sm">Create</button></form>{{range .Folders}}<span class="folder-manage" data-folder-id="{{.ID}}"><span class="labelname">{{.Name}}</span><form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/folders/{{.ID}}/rename"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input name="name" value="{{.Name}}" aria-label="Rename folder"><button class="btn-sm">Rename</button></form><form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/folders/{{.ID}}/delete" data-confirm="Delete this folder? It must be empty."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete folder" aria-label="Delete folder">` + iconTrash + `</button></form></span>{{end}}</details></aside>`

// selectBanner is the Gmail-style two-state "select all" bar. Its two spans are
// toggled by app.js: the first (after checking the header box on a paginated
// page) offers to widen the selection to every matching item; the second (after
// that) offers to clear it. The scope itself is driven by a hidden `scope` input
// on the bulk form, so no state is carried in the DOM here.
const selectBanner = `{{define "select-banner"}}<div class="select-banner" data-select-banner data-total="{{.TotalCount}}" hidden><span data-select-page hidden>All <b data-page-count></b> on this page are selected. <button type="button" class="cell-link" data-select-all-link>Select all <b data-total-count>{{.TotalCount}}</b> in {{folderLabel .Folder .ActiveLabel}}</button></span><span data-select-all-span hidden>All <b data-total-count>{{.TotalCount}}</b> in {{folderLabel .Folder .ActiveLabel}} are selected. <button type="button" class="cell-link" data-clear-selection>Clear selection</button></span></div>{{end}}`

const liveRequestsCard = `{{define "live-requests-card"}}{{if .SendRequests}}<section class="card" data-live-requests><div class="card-head"><h2>Draft send requests</h2></div><div class="mailheader"><span></span><span></span><span>To</span><span>Subject</span><span class="hcenter">Date</span><span class="hcenter">Size</span><span></span></div><div class="mailrows">{{range .SendRequests}}<div class="mailrow"><a class="mailrowlink" href="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.Draft.ID}}/edit"><span class="maildot"></span><span class="mailsender">{{join .Draft.To ", "}}</span><span class="mailsubject">{{if eq .Request.Status "pending"}}{{if eq .Request.NotificationStatus "failed"}}<span class="pill danger">Notification failed</span> {{else if eq .Request.NotificationStatus "queued"}}<span class="pill amber">Notification queued</span> {{else}}<span class="pill amber">Awaiting approval</span> {{end}}{{end}}{{if .Draft.Subject}}{{.Draft.Subject}}{{else}}(no subject){{end}}{{if .Draft.Text}} <span class="mailsnippet">— {{snippet .Draft.Text 80}}</span>{{end}}{{if .Request.Feedback}} <span class="muted small">— {{.Request.Feedback}}</span>{{end}}</span><span class="maildate">{{mailDate .Request.RequestedAt}}</span><span class="mailsize">{{filesize .SizeBytes}}</span></a><span class="mailaction">{{if eq .Request.Status "pending"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.Draft.ID}}/approve" data-confirm="Approve and send this draft?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn" title="Approve and send" aria-label="Approve and send">` + iconMarkRead + `</button></form>{{end}}</span></div>{{end}}</div></section>{{end}}{{end}}`
const liveListCard = `{{define "live-list-card"}}<section class="card" data-live-list{{if .Before}} data-cursor="{{.Before}}"{{end}}>{{if .Messages}}<div class="mailheader"><span class="mailcheck"><input type="checkbox" id="select-all" aria-label="Select all messages"></span><span></span><span>{{if eq .Folder "sent"}}To{{else}}From{{end}}</span><span>Subject</span><span class="hcenter">Date</span><span class="hcenter">Size</span><span></span></div><div class="mailrows">{{range .Messages}}<div class="mailrow{{if not .Read}} unread{{end}}" data-row-id="{{.ID}}"><span class="mailcheck"><input type="checkbox" name="ids" value="{{.ID}}" form="bulk-form" aria-label="Select message"></span><a class="mailrowlink" href="/ui/messages/{{.ID}}"><span class="maildot">{{if not .Read}}<span class="dot"></span>{{end}}</span><span class="mailsender">{{if eq $.Folder "sent"}}{{join .To ", "}}{{else}}{{if .From.Name}}{{.From.Name}}{{else}}{{.From.Address}}{{end}}{{end}}</span><span class="mailsubject">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .HasAttachments}} <span class="pill">attach</span>{{end}}{{range .Labels}} <span class="labelpill">{{.}}</span>{{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{mailDate .CreatedAt}}</span><span class="mailsize">{{filesize .SizeBytes}}</span></a><span class="mailaction">{{if eq $.Folder "spam"}}<form method="post" action="/ui/messages/{{.ID}}/spam"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="spam" value="0"><button class="secondary icon-btn" title="Not spam" aria-label="Not spam">` + iconNotSpam + `</button></form><form method="post" action="/ui/messages/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button></form>{{else if eq $.Folder "trash"}}<form method="post" action="/ui/messages/{{.ID}}/restore"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn" title="Restore" aria-label="Restore">` + iconRestore + `</button></form>{{else}}<form method="post" action="/ui/messages/{{.ID}}/read"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="read" value="{{if .Read}}0{{else}}1{{end}}"><button class="secondary icon-btn" title="{{if .Read}}Mark unread{{else}}Mark read{{end}}" aria-label="{{if .Read}}Mark unread{{else}}Mark read{{end}}">{{if .Read}}` + iconMarkUnread + `{{else}}` + iconMarkRead + `{{end}}</button></form><form method="post" action="/ui/messages/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button></form>{{end}}</span></div>{{end}}</div>{{if .HasMore}}<p><a href="{{.PagerURL}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No messages in this folder yet.</p>{{end}}</section>{{end}}`
const inboxBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr" data-copy="{{.Inbox.Address}}" role="button" tabindex="0" title="Click to copy">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar">{{if eq .Folder "trash"}}<form method="post" action="/ui/inboxes/{{.Inbox.ID}}/trash/empty" data-confirm="Permanently delete all messages in Trash? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Empty trash</button></form>{{end}}<form id="bulk-form" class="bulkbar" method="post" action="/ui/inboxes/{{.Inbox.ID}}/bulk"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="folder" value="{{.Folder}}"><input type="hidden" name="label" value="{{.ActiveLabel}}"><input type="hidden" name="scope" value="page">{{if .Principal.Admin}}<a class="btn secondary icon-btn" href="/?inbox={{.Inbox.ID}}" title="Inbox settings" aria-label="Inbox settings">` + iconSettingsSvg + `</a>{{end}}{{if eq .Folder "trash"}}<button name="action" value="restore" class="secondary icon-btn" title="Restore" aria-label="Restore">` + iconRestore + `</button><button name="action" value="purge" class="secondary icon-btn danger" data-confirm-all="Delete all selected messages permanently? This cannot be undone." title="Delete forever" aria-label="Delete forever">` + iconDeleteFore + `</button>{{else}}<button name="action" value="read" class="secondary icon-btn" title="Mark read" aria-label="Mark read">` + iconMarkRead + `</button><button name="action" value="unread" class="secondary icon-btn" title="Mark unread" aria-label="Mark unread">` + iconMarkUnread + `</button><button name="action" value="delete" class="secondary icon-btn danger" data-confirm-all="Move all selected messages to trash?" title="Move to trash" aria-label="Move to trash">` + iconTrash + `</button>{{end}}</form></div>
<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{if .StandaloneMode}}{{if not .StandaloneConfigured}}<div class="banner warn">This standalone inbox has no remote connector configured yet. <a href="{{.RemoteConnectorURL}}">Set up IMAP/SMTP</a>.</div>{{else if .StandalonePlain}}<div class="banner warn">This inbox connects to its remote server over plaintext (no transport security). <a href="{{.RemoteConnectorURL}}">Review connection settings</a>.</div>{{end}}{{else}}{{if not .OutboundReady}}<div class="banner warn">{{if .SendingPausedExternal}}Sending paused — configure the sending connector for the selected sender ({{.SendingPausedAddress}}). Mail will queue. <a href="{{.SendingPausedURL}}">Configure</a>.{{else}}Sending is paused until a provider is configured for this domain. Mail will queue. <a href="{{.DomainSendingSettingsURL}}">Add one</a>.{{end}}</div>{{end}}
{{if not .InboundReady}}<div class="banner warn">Not receiving — no receive path is configured for this domain. <a href="{{.DomainReceivingSettingsURL}}">Add one</a>.</div>{{end}}{{end}}
{{template "live-requests-card" .}}
{{template "select-banner" .}}
{{template "live-list-card" .}}</div></div>`

const composeBody = `<div class="toolbar"><a href="{{.ComposeCancel}}">← Cancel</a></div><section class="card"><h1>{{.ComposeTitle}}</h1>{{if .ComposeError}}<div class="error">{{.ComposeError}}</div>{{end}}<form method="post" action="{{.ComposeAction}}" enctype="multipart/form-data"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="_flash" value="{{.ComposeFlash}}"><input type="hidden" name="draft_id" value="{{.ComposeDraftID}}"><input type="hidden" name="return_to" value="{{.ComposeCancel}}">{{if gt (len .ComposeFromOptions) 1}}<label>From</label><select name="sender">{{$from := .ComposeFrom}}{{range .ComposeFromOptions}}<option value="{{.Address}}"{{if eq .Address $from}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else if .ComposeFromOptions}}<label>From</label><input value="{{(index .ComposeFromOptions 0).Label}}" disabled>{{end}}<label>To</label><input name="to" value="{{.ComposeTo}}" placeholder="someone@example.com" required><div class="row"><div><label>Cc</label><input name="cc" value="{{.ComposeCC}}"></div><div><label>Bcc</label><input name="bcc" value="{{.ComposeBCC}}"></div></div><label>Subject</label><input name="subject" value="{{.ComposeSubject}}"><label>Message</label><textarea name="text" rows="14">{{.ComposeText}}</textarea><label>Attachments</label><div class="attach-drop" id="attach-drop"><p class="attach-hint">Drag &amp; drop files here, or</p><label class="btn secondary attach-browse" for="attachments">Choose files</label><input class="attach-input" type="file" id="attachments" name="attachments" multiple><ul class="attach-list" id="attach-list"></ul></div><div class="attach-overlay" id="attach-overlay" hidden aria-hidden="true"><div class="attach-overlay-inner">Drop files to attach</div></div>{{if .ComposeNote}}<p class="muted">{{.ComposeNote}}</p>{{end}}<div class="dialog-actions"><a class="btn secondary" href="{{.ComposeCancel}}">Cancel</a><button type="submit" name="action" value="draft" class="secondary">Save Draft</button><button type="submit" name="action" value="send">Send</button></div></form></section>`

const draftReviewBody = `<div class="toolbar"><a href="/ui/inboxes/{{.Inbox.ID}}/drafts">← Back to drafts</a></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
<section class="card"><h1>Review draft</h1>
{{if .ReviewDraft.SendRequest.ApproverEmail}}{{if eq .ReviewDraft.SendRequest.NotificationStatus "failed"}}<p><span class="pill danger">Notification failed</span> The approval email to {{.ReviewDraft.SendRequest.ApproverEmail}} could not be delivered.</p>{{else if eq .ReviewDraft.SendRequest.NotificationStatus "queued"}}<p><span class="pill amber">Notification queued</span> The approval email to {{.ReviewDraft.SendRequest.ApproverEmail}} has not been handed to the outbound path yet.</p>{{else}}<p><span class="pill amber">Awaiting approval</span> Requested from {{.ReviewDraft.SendRequest.ApproverEmail}}{{if .ReviewDraft.SendRequest.TokenExpiresAt}} · expires {{mailDate .ReviewDraft.SendRequest.TokenExpiresAt}}{{end}}</p>{{end}}{{else}}<p><span class="pill amber">Awaiting approval</span> Requested by {{.ReviewDraft.SendRequest.RequestedBy}} · {{mailDate .ReviewDraft.SendRequest.RequestedAt}}</p>{{end}}
<dl class="draftmeta"><dt>From</dt><dd>{{if .ReviewDraft.FromAddress}}{{.ReviewDraft.FromAddress}}{{else}}{{.Inbox.Address}}{{end}}</dd><dt>To</dt><dd>{{join .ReviewDraft.To ", "}}</dd>{{if .ReviewDraft.CC}}<dt>Cc</dt><dd>{{join .ReviewDraft.CC ", "}}</dd>{{end}}{{if .ReviewDraft.BCC}}<dt>Bcc</dt><dd>{{join .ReviewDraft.BCC ", "}}</dd>{{end}}<dt>Subject</dt><dd>{{if .ReviewDraft.Subject}}{{.ReviewDraft.Subject}}{{else}}(no subject){{end}}</dd><dt>Attachments</dt><dd>{{if .ComposeNote}}{{.ComposeNote}}{{else}}None{{end}}</dd></dl>
<label>Text body</label>
<pre class="draftbody">{{if .ReviewDraft.Text}}{{.ReviewDraft.Text}}{{else}}(empty){{end}}</pre>
{{if .ReviewDraft.HTML}}<label>HTML alternative (source)</label>
<pre class="draftbody">{{.ReviewDraft.HTML}}</pre>{{end}}
<div class="dialog-actions"><form method="post" action="/ui/inboxes/{{.Inbox.ID}}/drafts/{{.ReviewDraft.ID}}/reject"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input name="feedback" placeholder="Feedback to agent (optional)"><button class="secondary danger">Reject</button></form><form method="post" action="/ui/inboxes/{{.Inbox.ID}}/drafts/{{.ReviewDraft.ID}}/cancel-send-request"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary">Cancel approval &amp; edit</button></form><form method="post" action="/ui/inboxes/{{.Inbox.ID}}/drafts/{{.ReviewDraft.ID}}/approve" data-confirm="Approve and send this draft?"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button>Approve &amp; send</button></form></div>
</section>`

const draftsBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr" data-copy="{{.Inbox.Address}}" role="button" tabindex="0" title="Click to copy">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar"><form id="drafts-bulk-form" class="bulkbar" method="post" action="/ui/inboxes/{{.Inbox.ID}}/drafts/bulk"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="scope" value="page">{{if .Principal.Admin}}<a class="btn secondary icon-btn" href="/?inbox={{.Inbox.ID}}" title="Inbox settings" aria-label="Inbox settings">` + iconSettingsSvg + `</a>{{end}}<button name="action" value="delete" class="secondary icon-btn danger" data-confirm-all="Delete all drafts?" title="Delete drafts" aria-label="Delete drafts">` + iconTrash + `</button></form></div>
<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{template "select-banner" .}}
<section class="card" data-live-list>{{if .Drafts}}<div class="mailheader drafts"><span class="mailcheck"><input type="checkbox" id="select-all" aria-label="Select all drafts"></span><span></span><span>To</span><span>Subject</span><span class="hcenter">Updated</span><span></span></div><div class="mailrows">{{range .Drafts}}<div class="mailrow drafts" data-row-id="{{.ID}}"><span class="mailcheck"><input type="checkbox" name="ids" value="{{.ID}}" form="drafts-bulk-form" aria-label="Select draft"></span><a class="mailrowlink" href="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/edit"><span class="maildot"></span><span class="mailsender">{{join .To ", "}}</span><span class="mailsubject">{{if .SendRequest}}{{if eq .SendRequest.Status "pending"}}<span class="draft-status pending"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 8h10"/><path d="m8.5 3.5 4.5 4.5-4.5 4.5"/></svg>Pending Send</span> {{else if eq .SendRequest.Status "rejected"}}<span class="draft-status rejected">Rejected</span> {{else if eq .SendRequest.Status "approved"}}<span class="draft-status sent">Sent</span> {{end}}{{end}}{{with index $.HandoffByDraft .ID}}{{template "handoff-badge" .}}{{end}}{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{mailDate .UpdatedAt}}</span></a><span class="mailaction">{{if .SendRequest}}{{if eq .SendRequest.Status "pending"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/approve" data-confirm="Approve and send this draft?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="btn-sm">Send</button></form>{{end}}{{end}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" data-confirm="Delete this draft?" title="Delete" aria-label="Delete">` + iconTrash + `</button></form></span></div>{{end}}</div>{{if .HasMore}}<p><a href="{{.PagerURL}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No drafts yet.</p>{{end}}</section>{{template "handoff-history" .}}</div></div>`

const outboxBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr" data-copy="{{.Inbox.Address}}" role="button" tabindex="0" title="Click to copy">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar">{{if .Principal.Admin}}<a class="btn secondary icon-btn" href="/?inbox={{.Inbox.ID}}" title="Inbox settings" aria-label="Inbox settings">` + iconSettingsSvg + `</a>{{end}}</div>
<div class="mail-layout">` + mailSidebar + `<div class="mailcontent">
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
<section class="card" data-live-list>{{if .Messages}}<div class="mailheader"><span></span><span>To</span><span>Subject</span><span class="hcenter">Status</span><span></span></div><div class="mailrows">{{range .Messages}}<div class="mailrow outbox"><a class="mailrowlink" href="/ui/messages/{{.ID}}"><span class="maildot"></span><span class="mailsender">{{join .To ", "}}</span><span class="mailsubject">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{if eq .Status "failed"}}<span class="pill danger">Failed</span>{{else if .Sending}}<span class="pill amber">Sending…</span>{{else}}<span class="pill">Pending</span>{{end}}</span></a><span class="mailaction">{{if eq .Status "failed"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/outbox/{{.ID}}/retry"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Retry</button></form>{{end}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/outbox/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" data-confirm="Remove this message from the outbox?" title="Delete" aria-label="Delete">` + iconTrash + `</button></form></span></div>{{end}}</div>{{if .HasMore}}<p><a href="{{.PagerURL}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No messages in the outbox.</p>{{end}}</section></div></div>`

// composeFlash carries a failed send (including uploaded attachment bytes)
// from the POST to the GET form, so refreshing never re-submits and the user's
// work is preserved.
type composeFlash struct {
	Title, Action, Cancel, Err string
	Input                      app.SendInput
}

func composeFlashSize(f composeFlash) int {
	n := len(f.Title) + len(f.Action) + len(f.Cancel) + len(f.Err) + len(f.Input.Subject) + len(f.Input.Text) + len(f.Input.HTML) + len(f.Input.FromAddress)
	for _, group := range [][]string{f.Input.To, f.Input.CC, f.Input.BCC} {
		for _, addr := range group {
			n += len(addr)
		}
	}
	for _, a := range f.Input.Attachments {
		n += len(a.Filename) + len(a.ContentType) + len(a.Content)
	}
	return n
}

func (s *Server) peekComposeFlash(r *http.Request) (composeFlash, bool) {
	if v, ok := s.flashes.peek(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(composeFlash); ok {
			return f, true
		}
	}
	return composeFlash{}, false
}

func (s *Server) renderComposeFlash(w http.ResponseWriter, r *http.Request, p model.Principal, tok string, f composeFlash) {
	note := ""
	if len(f.Input.Attachments) > 0 {
		names := make([]string, 0, len(f.Input.Attachments))
		for _, a := range f.Input.Attachments {
			names = append(names, a.Filename)
		}
		note = "Attachments kept: " + strings.Join(names, ", ") + ". They will be sent with this message."
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	data := pageData{
		Title:          f.Title,
		Principal:      p,
		CSRF:           csrf(r),
		Account:        acc,
		ComposeTitle:   f.Title,
		ComposeError:   f.Err,
		ComposeAction:  actionWithCSRF(f.Action, csrf(r)),
		ComposeCancel:  f.Cancel,
		ComposeTo:      strings.Join(f.Input.To, ", "),
		ComposeCC:      strings.Join(f.Input.CC, ", "),
		ComposeBCC:     strings.Join(f.Input.BCC, ", "),
		ComposeSubject: f.Input.Subject,
		ComposeText:    f.Input.Text,
		ComposeNote:    note,
		ComposeFlash:   tok,
	}
	if f.Input.InboxID == "" && strings.HasPrefix(f.Action, "/ui/messages/") {
		parts := strings.Split(f.Action, "/")
		if len(parts) >= 4 {
			if m, err := s.Service.Store.GetMessage(r.Context(), p, parts[3]); err == nil {
				f.Input.InboxID = m.InboxID
			}
		}
	}
	if f.Input.InboxID != "" {
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, f.Input.InboxID); err == nil {
			data.ComposeFromOptions, data.ComposeFrom = composeFromOptions(box, f.Input.FromAddress)
		}
	}
	s.render(w, r, composeBody, data)
}

// composeFromOptions returns the selectable From addresses for an inbox
// (primary first, then managed aliases) and the one to preselect: the requested
// address when present, otherwise the inbox default, otherwise the primary. A
// label shows "Name <address>" when the inbox or alias has a display name.
func composeFromOptions(box model.Inbox, requested string) ([]fromOption, string) {
	label := func(addr, name string) string {
		if name != "" {
			return name + " <" + addr + ">"
		}
		return addr
	}
	options := []fromOption{{Address: box.Address, Label: label(box.Address, box.DisplayName)}}
	for _, addr := range box.Aliases {
		options = append(options, fromOption{Address: addr, Label: label(addr, box.AliasNames[addr])})
	}
	selected := strings.TrimSpace(requested)
	if selected == "" {
		selected = box.DefaultSender
	}
	if selected != "" {
		for _, opt := range options {
			if strings.EqualFold(opt.Address, selected) {
				return options, opt.Address
			}
		}
	}
	return options, box.Address
}

func (s *Server) uiInbox(w http.ResponseWriter, r *http.Request) {
	s.renderMailbox(w, r, "inbox")
}

func (s *Server) uiSent(w http.ResponseWriter, r *http.Request) {
	s.renderMailbox(w, r, "sent")
}

func (s *Server) uiSpam(w http.ResponseWriter, r *http.Request) {
	s.renderMailbox(w, r, "spam")
}

func (s *Server) uiTrash(w http.ResponseWriter, r *http.Request) {
	s.renderMailbox(w, r, "trash")
}

// uiLabel renders the inbox filtered to messages carrying a single label. The
// label is passed as a query parameter (?name=) to avoid path-encoding issues
// with labels that contain slashes.
func (s *Server) uiLabel(w http.ResponseWriter, r *http.Request) {
	s.renderMailboxLabel(w, r, r.URL.Query().Get("name"))
}

func (s *Server) uiDrafts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	drafts, err := s.Service.Store.ListDraftsPaged(r.Context(), p, box.ID, before, inboxPageSize+1)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	hasMore := len(drafts) > inboxPageSize
	if hasMore {
		drafts = drafts[:inboxPageSize]
	}
	cursor := ""
	if len(drafts) > 0 {
		cursor = drafts[len(drafts)-1].ID
	}
	pagerURL := ""
	if cursor != "" {
		pagerURL = "/ui/inboxes/" + box.ID + "/drafts?before=" + url.QueryEscape(cursor)
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, box.ID)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, box.ID)
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	spamCount, _ := s.Service.Store.CountSpam(r.Context(), p, box.ID)
	trashCount, _ := s.Service.Store.CountTrash(r.Context(), p, box.ID)
	inboxLabels, _ := s.Service.Store.ListInboxLabels(r.Context(), p, box.ID)
	labelUnread, _ := s.Service.Store.InboxLabelUnreadCounts(r.Context(), p, box.ID)
	handoffs, _ := s.Service.Store.ListAssistantHandlingForInbox(r.Context(), p, box.ID, 50)
	handoffByDraft := make(map[string]model.AssistantHandlingRequest, len(handoffs))
	for _, h := range handoffs {
		if _, ok := handoffByDraft[h.DraftID]; !ok {
			handoffByDraft[h.DraftID] = h
		}
	}
	s.renderMail(w, r, draftsBody, pageData{
		Title:          box.Address + " · Drafts",
		Page:           "inbox",
		Principal:      p,
		CSRF:           csrf(r),
		Account:        acc,
		Inbox:          &box,
		Drafts:         drafts,
		Handoffs:       handoffs,
		HandoffByDraft: handoffByDraft,
		Folder:         "drafts",
		HasMore:        hasMore,
		Before:         cursor,
		PagerURL:       pagerURL,
		TotalCount:     draftCount,
		Labels:         inboxLabels,
		LabelUnread:    labelUnread,
		UnreadCount:    unread[box.ID],
		SpamCount:      spamCount,
		TrashCount:     trashCount,
		DraftCount:     draftCount,
		OutboxCount:    outboxCount,
		Folders:        s.buildFolderSidebar(r.Context(), p.AccountID, box.ID),
		Notice:         r.URL.Query().Get("notice"),
	})
}

// uiDraftCancelHandoff withdraws an outstanding (pending or ambiguous) RemoteDraft
// handoff and returns the draft to editable. It requires Assistant/Owner. An
// ambiguous handoff can be resolved here so its frozen draft is not trapped.
func (s *Server) uiDraftCancelHandoff(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if _, _, err := s.Service.Store.CancelHandoff(r.Context(), p, r.PathValue("draftId")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice="+url.QueryEscape("Handoff cancelled"), 303)
}

// uiDraftRetryHandoff re-requests a RemoteDraft handoff for a draft whose previous
// handoff reached a terminal state (failed/cancelled). It routes through the
// inbox's effective authoring mode, so it only produces a handoff when the inbox
// is in remote_draft mode.
func (s *Server) uiDraftRetryHandoff(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if _, err := s.Service.RequestSend(r.Context(), p, r.PathValue("draftId"), false); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice="+url.QueryEscape("Handoff re-requested"), 303)
}

func (s *Server) uiOutbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	msgs, err := s.Service.Store.ListOutboxBefore(r.Context(), p, box.ID, before, inboxPageSize+1)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	hasMore := len(msgs) > inboxPageSize
	if hasMore {
		msgs = msgs[:inboxPageSize]
	}
	pagerURL := ""
	if hasMore && len(msgs) > 0 {
		pagerURL = "/ui/inboxes/" + box.ID + "/outbox?before=" + url.QueryEscape(msgs[len(msgs)-1].ID)
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, box.ID)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, box.ID)
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	spamCount, _ := s.Service.Store.CountSpam(r.Context(), p, box.ID)
	trashCount, _ := s.Service.Store.CountTrash(r.Context(), p, box.ID)
	inboxLabels, _ := s.Service.Store.ListInboxLabels(r.Context(), p, box.ID)
	labelUnread, _ := s.Service.Store.InboxLabelUnreadCounts(r.Context(), p, box.ID)
	s.renderMail(w, r, outboxBody, pageData{
		Title:       box.Address + " · Outbox",
		Page:        "inbox",
		Principal:   p,
		CSRF:        csrf(r),
		Account:     acc,
		Inbox:       &box,
		Messages:    msgs,
		Folder:      "outbox",
		HasMore:     hasMore,
		PagerURL:    pagerURL,
		Labels:      inboxLabels,
		LabelUnread: labelUnread,
		UnreadCount: unread[box.ID],
		SpamCount:   spamCount,
		TrashCount:  trashCount,
		DraftCount:  draftCount,
		OutboxCount: outboxCount,
		Folders:     s.buildFolderSidebar(r.Context(), p.AccountID, box.ID),
		Notice:      r.URL.Query().Get("notice"),
	})
}

func (s *Server) uiDraftEdit(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	d, err := s.Service.Store.GetDraft(r.Context(), p, r.PathValue("draftId"))
	if err != nil {
		http.Error(w, "draft not found", 404)
		return
	}
	if d.InboxID != box.ID {
		http.Error(w, "draft not found", 404)
		return
	}
	atts, _ := s.Service.Store.ListDraftAttachments(r.Context(), p, d.ID)
	note := ""
	if len(atts) > 0 {
		names := make([]string, 0, len(atts))
		for _, a := range atts {
			names = append(names, a.Filename)
		}
		note = "Attachments: " + strings.Join(names, ", ")
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	if d.Status == model.DraftStatusPendingApproval && d.SendRequest != nil {
		s.render(w, r, draftReviewBody, pageData{
			Title:       "Review Draft",
			Principal:   p,
			CSRF:        csrf(r),
			Account:     acc,
			Inbox:       &box,
			ReviewDraft: &d,
			ComposeNote: note,
			Notice:      r.URL.Query().Get("notice"),
		})
		return
	}
	if d.Status == model.DraftStatusRejected && d.SendRequest != nil && d.SendRequest.Feedback != "" {
		if note != "" {
			note += " · "
		}
		note += "Rejected: " + d.SendRequest.Feedback
	}
	fromOptions, from := composeFromOptions(box, d.FromAddress)
	s.render(w, r, composeBody, pageData{
		Title:              "Edit Draft",
		Principal:          p,
		CSRF:               csrf(r),
		Account:            acc,
		ComposeTitle:       "Edit Draft",
		ComposeAction:      actionWithCSRF("/ui/inboxes/"+box.ID+"/drafts/"+d.ID+"/save", csrf(r)),
		ComposeCancel:      "/ui/inboxes/" + box.ID + "/drafts",
		ComposeTo:          strings.Join(d.To, ", "),
		ComposeCC:          strings.Join(d.CC, ", "),
		ComposeBCC:         strings.Join(d.BCC, ", "),
		ComposeSubject:     d.Subject,
		ComposeText:        d.Text,
		ComposeNote:        note,
		ComposeDraftID:     d.ID,
		ComposeFrom:        from,
		ComposeFromOptions: fromOptions,
	})
}

func (s *Server) uiDraftSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	draftID := r.PathValue("draftId")
	// Parse the multipart form first so the action field (and all other fields)
	// are available. The CSRF middleware only runs ParseForm (url-encoded), so
	// without this the action is always empty for a multipart POST.
	in, err := s.parseMessageForm(w, r)
	if err != nil {
		s.Log.Error("draft save: parse form", "draft_id", draftID, "error", err)
		s.uiError(w, err, 400)
		return
	}
	action := r.Form.Get("action")
	if action == "send" {
		// Send the draft via the send-draft path, then redirect to Sent.
		if draftID == "" {
			http.Error(w, "no draft to send", 400)
			return
		}
		// Preserve the draft's reply linkage while using the edited form fields.
		d, derr := s.Service.Store.GetDraft(r.Context(), p, draftID)
		if derr != nil {
			http.Error(w, derr.Error(), 400)
			return
		}
		in.InboxID = box.ID
		if in.ReplyToMessageID == "" {
			in.ReplyToMessageID = d.ReplyToMessageID
		}
		if _, err := s.Service.SendDraft(r.Context(), p, draftID, in, ""); err != nil {
			s.Log.Error("draft send: enqueue failed", "draft_id", draftID, "inbox_id", box.ID, "error", err)
			s.uiError(w, err, 400)
			return
		}
		http.Redirect(w, r, returnTo(r, box.ID, "drafts"), 303)
		return
	}
	// Save draft: create or update.
	d := model.Draft{InboxID: box.ID, FromAddress: in.FromAddress, To: in.To, CC: in.CC, BCC: in.BCC, Subject: in.Subject, Text: in.Text, HTML: in.HTML}
	if draftID != "" {
		d.ID = draftID
		d, err = s.Service.Store.UpdateDraft(r.Context(), p, d)
	} else {
		d, err = s.Service.Store.CreateDraft(r.Context(), p, d)
	}
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	// Persist any uploaded attachments to disk.
	atts, err := s.formAttachments(r)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if _, err = s.persistDraftAttachments(r.Context(), p, d.ID, atts); err != nil {
		s.uiError(w, err, 500)
		return
	}
	if action == "request-send" {
		if _, err = s.Service.RequestSend(r.Context(), p, d.ID, false); err != nil {
			s.uiError(w, err, 400)
			return
		}
		http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Send+requested", 303)
		return
	}
	http.Redirect(w, r, returnTo(r, box.ID, "drafts"), 303)
}

func (s *Server) uiDraftDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	draftID := r.PathValue("draftId")
	paths, err := s.Service.Store.DeleteDraftCascade(r.Context(), p, draftID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Draft+deleted", 303)
}

func (s *Server) uiDraftRequestSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if _, err = s.Service.RequestSend(r.Context(), p, r.PathValue("draftId"), false); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Send+requested", 303)
}

func (s *Server) uiDraftApprove(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	feedback := strings.TrimSpace(r.Form.Get("feedback"))
	if _, err = s.Service.ApproveDraft(r.Context(), p, r.PathValue("draftId"), feedback, model.DecisionMethodUI, ""); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"?notice=Draft+sent", 303)
}

func (s *Server) uiDraftReject(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	feedback := strings.TrimSpace(r.Form.Get("feedback"))
	if len(feedback) > maxFeedbackBytes {
		http.Error(w, "feedback is too long", 400)
		return
	}
	if _, err = s.Service.RejectDraft(r.Context(), p, r.PathValue("draftId"), feedback, model.DecisionMethodUI); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice=Draft+rejected", 303)
}

func (s *Server) uiDraftCancelSendRequest(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if _, err = s.Service.CancelSendRequest(r.Context(), p, r.PathValue("draftId")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts/"+r.PathValue("draftId")+"/edit?notice=Request+cancelled", 303)
}

func (s *Server) uiOutboxRetry(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if err = s.Service.Store.RequeueFailed(r.Context(), p, r.PathValue("msgId")); err != nil {
		s.uiError(w, err, 400)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/outbox?notice=Message+queued+for+retry", 303)
}

func (s *Server) uiOutboxDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	_, ev, err := s.Service.Store.DeleteOutboxMessage(r.Context(), p, r.PathValue("msgId"))
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/outbox?notice=Message+removed", 303)
}

func (s *Server) draftAttachmentPath() string {
	id := idgen.New("dat")
	return filepath.Join(s.Service.Config.DataDir, "drafts", id[4:6], id[6:8], id+".bin")
}

func (s *Server) renderMailbox(w http.ResponseWriter, r *http.Request, folder string) {
	s.renderMailboxFiltered(w, r, folder, "")
}

// renderMailboxLabel renders the inbox filtered to a single label.
func (s *Server) renderMailboxLabel(w http.ResponseWriter, r *http.Request, label string) {
	s.renderMailboxFiltered(w, r, "inbox", label)
}

func (s *Server) renderMailboxFiltered(w http.ResponseWriter, r *http.Request, folder, label string) {
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if label != "" {
		folder = "label"
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	msgs, hasMore, cursor, pagerURL, err := s.buildMessageList(r, p, id, folder, label, before)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	// The exact folder/label total backs the "select all N" banner; it is
	// independent of the list page size.
	totalCount := 0
	if f, _, ok := mailboxFilter(id, folder, label); ok {
		totalCount, _ = s.Service.Store.CountMessages(r.Context(), p, f)
	}
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	spamCount, _ := s.Service.Store.CountSpam(r.Context(), p, id)
	trashCount, _ := s.Service.Store.CountTrash(r.Context(), p, id)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, id)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, id)
	inboxLabels, _ := s.Service.Store.ListInboxLabels(r.Context(), p, id)
	labelUnread, _ := s.Service.Store.InboxLabelUnreadCounts(r.Context(), p, id)
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	folderSidebar := s.buildFolderSidebar(r.Context(), p.AccountID, id)
	// Sending readiness follows the inbox domain's sending config.
	outboundReady := true
	pausedExternal := false
	pausedAddress := ""
	pausedURL := ""
	_, outErr := s.Service.Store.ResolveDomainSendingConfig(r.Context(), p.AccountID, box.DomainID)
	outboundReady = outErr == nil
	recvDomain, inErr := s.Service.Store.GetDomain(r.Context(), p.AccountID, box.DomainID)
	var sendRequests []SendRequestRow
	if folder == "inbox" {
		sendRequests, _ = s.buildSendRequests(r, p, id)
	}
	data := pageData{
		Title:                      box.Address,
		Page:                       "inbox",
		Principal:                  p,
		CSRF:                       csrf(r),
		Account:                    acc,
		Inbox:                      &box,
		Messages:                   msgs,
		SendRequests:               sendRequests,
		HasMore:                    hasMore,
		Before:                     cursor,
		Folder:                     folder,
		Folders:                    folderSidebar,
		PagerURL:                   pagerURL,
		TotalCount:                 totalCount,
		Labels:                     inboxLabels,
		LabelUnread:                labelUnread,
		ActiveLabel:                label,
		UnreadCount:                unread[id],
		SpamCount:                  spamCount,
		TrashCount:                 trashCount,
		DraftCount:                 draftCount,
		OutboxCount:                outboxCount,
		OutboundReady:              outboundReady,
		InboundReady:               inErr == nil && recvDomain.ReceivingProvider != "",
		SendingPausedExternal:      pausedExternal,
		SendingPausedAddress:       pausedAddress,
		SendingPausedURL:           pausedURL,
		DomainSendingSettingsURL:   "/?domain=" + url.PathEscape(box.DomainID) + "&kind=sending",
		DomainReceivingSettingsURL: "/?domain=" + url.PathEscape(box.DomainID) + "&kind=receiving",
		Notice:                     r.URL.Query().Get("notice"),
	}
	s.applyStandaloneMailbox(r, p, box, &data)
	s.renderMail(w, r, inboxBody, data)
}

// applyStandaloneMailbox decorates a mailbox page's data with the standalone
// (remote) connection state, so the mailbox page can show a setup/plain warning
// without the handler branching on the inbox kind per template.
func (s *Server) applyStandaloneMailbox(r *http.Request, p model.Principal, box model.Inbox, data *pageData) {
	if box.Kind != model.InboxKindStandalone {
		return
	}
	data.StandaloneMode = true
	data.StandaloneConfigured = box.RemoteConfigured && box.Remote != nil
	data.StandalonePlain = box.Remote != nil && box.Remote.Security == model.RemoteSecurityPlain
	data.RemoteConnectorURL = "/ui/inboxes/" + box.ID + "/remote"
	if view, verr := s.remoteConfigView(r.Context(), p, box.ID); verr == nil {
		data.RemoteConfig = view
	}
}

func (s *Server) uiBulk(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	action := r.Form.Get("action")
	switch action {
	case "read", "unread", "delete", "restore", "purge", "move":
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	// A standalone inbox's messages are cached remote metadata; the bulk action
	// routes to the live server (flags/move) or the local label store, and the
	// "all N" set is re-derived from the folder's remote index — never from a
	// client-supplied id list that could name another inbox.
	if box.Kind == model.InboxKindStandalone && box.RemoteConfigured && box.Remote != nil {
		s.uiBulkRemote(w, r, p, box, action)
		return
	}
	ids := r.Form["ids"]
	if r.Form.Get("scope") == "all" {
		// "Select all N": re-derive the folder/label set server-side rather than
		// trusting a client-supplied id list, so the operation covers exactly
		// what the rendered banner promised. An unknown folder is refused.
		folder := r.Form.Get("folder")
		label := r.Form.Get("label")
		f, _, ok := mailboxFilter(box.ID, folder, label)
		if !ok {
			http.Error(w, "unknown folder", 400)
			return
		}
		// Every bulk action here (including purge) requires Assistant; only
		// sending needs Owner, and none of these actions sends.
		if !p.CanAssist(box.ID) && !p.Admin {
			http.Error(w, "forbidden", 403)
			return
		}
		ids, err = s.Service.Store.AllMessageIDs(r.Context(), p, f)
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	count := 0
	for _, id := range ids {
		if s.bulkMessageAction(r, p, box.ID, id, action) {
			count++
		}
	}
	s.redirectBulkResult(w, r, box, action, count)
}

// uiBulkRemote applies a bulk action to a standalone inbox's remote messages.
// The explicit-id list is verified to belong to this inbox's remote index; the
// "all N" set is re-derived from the folder's cached remote index. A per-message
// failure is counted out rather than aborting the batch.
func (s *Server) uiBulkRemote(w http.ResponseWriter, r *http.Request, p model.Principal, box model.Inbox, action string) {
	// Every bulk action here (including purge) requires Assistant; only sending
	// needs Owner, and none of these actions sends.
	if !p.CanAssist(box.ID) && !p.Admin {
		http.Error(w, "forbidden", 403)
		return
	}
	folder := r.Form.Get("folder")
	rolePath := s.remoteFolderPathForView(r, p, box, folder)
	var ids []string
	if r.Form.Get("scope") == "all" {
		res, err := s.remoteMailbox().ListRemoteMessages(r.Context(), p, box.ID, rolePath, remoteBulkMax, "")
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		for _, v := range res.Items {
			ids = append(ids, v.ID)
		}
	} else {
		for _, id := range r.Form["ids"] {
			// Verify the id is in this inbox's remote index before acting, so a
			// crafted id from another inbox is never touched.
			if _, gerr := s.Service.Store.GetRemoteMessage(r.Context(), p.AccountID, box.ID, id); gerr == nil {
				ids = append(ids, id)
			}
		}
	}
	count := 0
	for _, id := range ids {
		if s.bulkRemoteMessageAction(r, p, box, id, action, r.Form.Get("dest")) {
			count++
		}
	}
	s.redirectBulkResult(w, r, box, action, count)
}

// remoteBulkMax bounds an "all N" remote bulk enumeration.
const remoteBulkMax = 5000

// remoteFolderPathForView maps a mailbox view folder to its remote folder path.
func (s *Server) remoteFolderPathForView(r *http.Request, p model.Principal, box model.Inbox, folder string) string {
	root := strings.TrimSpace(box.Namespace)
	if root == "" {
		root = model.NamespaceINBOX
	}
	switch folder {
	case "sent":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleSent); ok {
			return f.Path
		}
	case "spam":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleSpam); ok {
			return f.Path
		}
	case "trash":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleTrash); ok {
			return f.Path
		}
	case "inbox":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleInbox); ok {
			return f.Path
		}
	}
	return root
}

// bulkRemoteMessageAction applies one bulk action to one remote message.
func (s *Server) bulkRemoteMessageAction(r *http.Request, p model.Principal, box model.Inbox, id, action, dest string) bool {
	ctx := r.Context()
	switch action {
	case "read", "unread":
		read := action == "read"
		if _, err := s.remoteMailbox().SetRemoteRead(ctx, p, box.ID, id, read); err == nil {
			return true
		}
	case "delete":
		if err := s.trashRemoteMessage(ctx, p, mailboxBackend{srv: s, inbox: box, remote: s.remoteMailbox(), routed: true, p: p}, id); err == nil {
			return true
		}
	case "restore":
		if err := s.moveRemoteToRole(ctx, p, mailboxBackend{srv: s, inbox: box, remote: s.remoteMailbox(), routed: true, p: p}, id, model.FolderRoleInbox); err == nil {
			return true
		}
	case "move":
		if strings.TrimSpace(dest) == "" {
			return false
		}
		if _, err := s.remoteMailbox().MoveRemoteMessage(ctx, p, box.ID, id, dest); err == nil {
			return true
		}
	case "purge":
		// Permanent erasure is a remote expunge; Assistant or Owner may do it
		// (the only role difference is sending), enforced by the app service.
		if err := s.remoteMailbox().PurgeRemoteMessage(ctx, p, box.ID, id); err == nil {
			return true
		}
	}
	return false
}

// redirectBulkResult redirects to the mailbox folder after a bulk action.
func (s *Server) redirectBulkResult(w http.ResponseWriter, r *http.Request, box model.Inbox, action string, count int) {
	notice := fmt.Sprintf("%d message", count)
	if count != 1 {
		notice += "s"
	}
	switch action {
	case "read":
		notice += " marked read"
	case "unread":
		notice += " marked unread"
	case "delete":
		notice += " moved to trash"
	case "restore":
		notice += " restored"
	case "purge":
		notice += " deleted permanently"
	case "move":
		notice += " moved"
	}
	base := "/ui/inboxes/" + box.ID
	switch r.Form.Get("folder") {
	case "sent":
		base += "/sent"
	case "trash":
		base += "/trash"
	case "spam":
		base += "/spam"
	}
	http.Redirect(w, r, base+"?notice="+url.QueryEscape(notice), 303)
}

// bulkMessageAction applies one bulk action to one message, scoped to the
// owning inbox. It returns true when an action was performed, so the caller can
// count the result. It is a no-op (false) for a message that does not exist or
// belongs to a different inbox, mirroring the explicit-id list path.
func (s *Server) bulkMessageAction(r *http.Request, p model.Principal, inboxID, id, action string) bool {
	m, err := s.Service.Store.GetMessage(r.Context(), p, id)
	if err != nil || m.InboxID != inboxID {
		return false
	}
	switch action {
	case "read", "unread":
		read := action == "read"
		if ev, err := s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); err == nil {
			s.publishStateEvent(ev)
			return true
		}
	case "delete":
		if _, ev, err := s.Service.Store.TrashMessage(r.Context(), p, m.ID); err == nil {
			if ev != nil {
				s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
				s.Service.Hub.Publish(*ev)
			}
			return true
		}
	case "restore":
		if _, ev, err := s.Service.Store.RestoreMessage(r.Context(), p, m.ID); err == nil {
			if ev != nil {
				s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
				s.Service.Hub.Publish(*ev)
			}
			return true
		}
	case "purge":
		path, _, ev, err := s.Service.Store.PurgeMessage(r.Context(), p, m.ID)
		if err != nil {
			return false
		}
		if path != "" {
			s.removeDataFile(path)
		}
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(ev)
		return true
	case "move":
		// The form submits the destination folder path; resolve it to this
		// inbox's folder id before moving so a cross-inbox path can never move a
		// message into another mailbox.
		dest := strings.TrimSpace(r.Form.Get("dest"))
		if dest == "" {
			return false
		}
		folder, ferr := s.Service.Store.GetFolderByPath(r.Context(), p.AccountID, inboxID, dest)
		if ferr != nil {
			return false
		}
		if _, ev, err := s.Service.Store.MoveMessageToFolder(r.Context(), p, m.ID, folder.ID); err == nil {
			s.publishStateEvent(ev)
			return true
		}
	}
	return false
}

// uiDraftsBulk deletes the selected drafts (explicit ids or every draft in the
// inbox with scope=all). Drafts have no read/flag state, so delete is the only
// bulk action. Each draft is verified to belong to the inbox and the caller to
// hold Assistant on it before its attachment bytes are unlinked.
func (s *Server) uiDraftsBulk(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	ids := r.Form["ids"]
	if r.Form.Get("scope") == "all" {
		ids, err = s.Service.Store.AllDraftIDs(r.Context(), p, box.ID)
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
	}
	count := 0
	for _, id := range ids {
		d, err := s.Service.Store.GetDraft(r.Context(), p, id)
		if err != nil || d.InboxID != box.ID {
			continue
		}
		paths, err := s.Service.Store.DeleteDraftCascade(r.Context(), p, id)
		if err != nil {
			continue
		}
		for _, path := range paths {
			s.removeDataFile(path)
		}
		count++
	}
	notice := fmt.Sprintf("%d draft", count)
	if count != 1 {
		notice += "s"
	}
	notice += " deleted"
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/drafts?notice="+url.QueryEscape(notice), 303)
}

func (s *Server) uiCompose(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	if f, ok := s.peekComposeFlash(r); ok {
		s.renderComposeFlash(w, r, p, r.URL.Query().Get("_flash"), f)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	fromOptions, from := composeFromOptions(box, "")
	s.render(w, r, composeBody, pageData{
		Title:              "Compose",
		Principal:          p,
		CSRF:               csrf(r),
		Account:            acc,
		ComposeTitle:       "New message",
		ComposeAction:      actionWithCSRF("/ui/inboxes/"+box.ID+"/send", csrf(r)),
		ComposeCancel:      "/ui/inboxes/" + box.ID,
		ComposeFrom:        from,
		ComposeFromOptions: fromOptions,
	})
}

func (s *Server) uiComposeSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	formURL := "/ui/inboxes/" + box.ID + "/compose"
	in, err := s.parseMessageForm(w, r)
	if err != nil {
		s.renderComposeError(w, r, "New message", formURL, "/ui/inboxes/"+box.ID+"/send", "/ui/inboxes/"+box.ID, in, err)
		return
	}
	in.InboxID = box.ID
	// Save Draft button on the compose form.
	if r.Form.Get("action") == "draft" {
		d := model.Draft{InboxID: box.ID, FromAddress: in.FromAddress, To: in.To, CC: in.CC, BCC: in.BCC, Subject: in.Subject, Text: in.Text, HTML: in.HTML}
		if id := r.Form.Get("draft_id"); id != "" {
			d.ID = id
			d, err = s.Service.Store.UpdateDraft(r.Context(), p, d)
		} else {
			d, err = s.Service.Store.CreateDraft(r.Context(), p, d)
		}
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		// Persist uploaded attachments.
		atts, aerr := s.formAttachments(r)
		if aerr != nil {
			http.Error(w, aerr.Error(), 400)
			return
		}
		if _, err = s.persistDraftAttachments(r.Context(), p, d.ID, atts); err != nil {
			s.uiError(w, err, 500)
			return
		}
		http.Redirect(w, r, returnTo(r, box.ID, "drafts"), 303)
		return
	}
	s.submitMessage(w, r, p, in, "New message", formURL, "/ui/inboxes/"+box.ID+"/send", "/ui/inboxes/"+box.ID)
}

func (s *Server) uiReplyForm(w http.ResponseWriter, r *http.Request) {
	s.composeMessage(w, r, "reply")
}

func (s *Server) uiReplyAllForm(w http.ResponseWriter, r *http.Request) {
	s.composeMessage(w, r, "reply-all")
}

func (s *Server) uiForwardForm(w http.ResponseWriter, r *http.Request) {
	s.composeMessage(w, r, "forward")
}

func (s *Server) composeMessage(w http.ResponseWriter, r *http.Request, kind string) {
	p := principal(r)
	if f, ok := s.peekComposeFlash(r); ok {
		s.renderComposeFlash(w, r, p, r.URL.Query().Get("_flash"), f)
		return
	}
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox message resolves through the remote reader so the
		// compose/reply form can be built from its cached metadata.
		if s.composeRemoteMessage(w, r, p, kind, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	data := pageData{Principal: p, CSRF: csrf(r), Account: acc, ComposeCancel: "/ui/messages/" + m.ID}
	var box model.Inbox
	if b, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, m.InboxID); err == nil {
		box = b
		data.ComposeFromOptions, data.ComposeFrom = composeFromOptions(box, "")
	}
	switch kind {
	case "reply":
		to := strings.Join(m.To, ", ")
		if m.Direction == "inbound" {
			to = m.From.Address
		}
		data.Title = "Reply"
		data.ComposeTitle = "Reply"
		data.ComposeTo = to
		data.ComposeSubject = app.ReplySubject(m.Subject)
		data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/reply", csrf(r))
	case "reply-all":
		to, cc := replyAllRecipients(m, box)
		data.Title = "Reply all"
		data.ComposeTitle = "Reply all"
		data.ComposeTo = to
		data.ComposeCC = cc
		data.ComposeSubject = app.ReplySubject(m.Subject)
		data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/reply-all", csrf(r))
	case "forward":
		data.Title = "Forward"
		data.ComposeTitle = "Forward"
		data.ComposeSubject = app.ForwardSubject(m.Subject)
		data.ComposeNote = "The original message and its attachments are included automatically."
		data.ComposeAction = actionWithCSRF("/ui/messages/"+m.ID+"/forward", csrf(r))
	}
	s.render(w, r, composeBody, data)
}

func (s *Server) uiReplySend(w http.ResponseWriter, r *http.Request) {
	s.sendMessage(w, r, "reply")
}

func (s *Server) uiReplyAllSend(w http.ResponseWriter, r *http.Request) {
	s.sendMessage(w, r, "reply-all")
}

func (s *Server) uiForwardSend(w http.ResponseWriter, r *http.Request) {
	s.sendMessage(w, r, "forward")
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, kind string) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "message not found", 404)
		return
	}
	formURL := "/ui/messages/" + m.ID + "/" + kind
	in, err := s.parseMessageForm(w, r)
	if err != nil {
		title := replyTitle(kind)
		s.renderComposeError(w, r, title, formURL, "/ui/messages/"+m.ID+"/"+kind, "/ui/messages/"+m.ID, in, err)
		return
	}
	in.InboxID = m.InboxID
	title := replyTitle(kind)
	action := "/ui/messages/" + m.ID + "/reply"
	if kind == "forward" {
		action = "/ui/messages/" + m.ID + "/forward"
		in.ForwardOfMessageID = m.ID
	} else if kind == "reply-all" {
		action = "/ui/messages/" + m.ID + "/reply-all"
		in.ReplyToMessageID = m.ID
	} else {
		in.ReplyToMessageID = m.ID
	}
	s.submitMessage(w, r, p, in, title, formURL, action, "/ui/messages/"+m.ID)
}

// replyTitle returns the compose-page title for a reply, reply-all or forward.
func replyTitle(kind string) string {
	switch kind {
	case "forward":
		return "Forward"
	case "reply-all":
		return "Reply all"
	default:
		return "Reply"
	}
}

// replyAllRecipients computes the To and Cc addresses for a reply-all: the
// original sender goes in To (the original To list for an outbound message),
// and the remaining original To/Cc recipients go in Cc. Addresses belonging to
// the mailbox itself (its primary address and aliases) are removed so the
// reply does not copy the sender's own mailbox. Order is preserved and
// duplicates (case-insensitive) are dropped.
func replyAllRecipients(m model.Message, box model.Inbox) (string, string) {
	self := map[string]bool{strings.ToLower(box.Address): true}
	for _, a := range box.Aliases {
		self[strings.ToLower(a)] = true
	}
	seen := map[string]bool{}
	var to []string
	var cc []string
	add := func(list *[]string, addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		key := strings.ToLower(addr)
		if self[key] || seen[key] {
			return
		}
		seen[key] = true
		*list = append(*list, addr)
	}
	if m.Direction == "inbound" {
		add(&to, m.From.Address)
		for _, a := range m.To {
			add(&cc, a)
		}
	} else {
		for _, a := range m.To {
			add(&to, a)
		}
	}
	for _, a := range m.CC {
		add(&cc, a)
	}
	return strings.Join(to, ", "), strings.Join(cc, ", ")
}

func (s *Server) submitMessage(w http.ResponseWriter, r *http.Request, p model.Principal, in app.SendInput, title, formURL, action, cancel string) {
	_, err := s.Service.Send(r.Context(), p, in, "")
	if err != nil {
		s.renderComposeError(w, r, title, formURL, action, cancel, in, err)
		return
	}
	if tok := r.Form.Get("_flash"); tok != "" {
		s.flashes.take(tok)
	}
	http.Redirect(w, r, returnTo(r, in.InboxID, "sent"), 303)
}

// renderComposeError redirects back to the compose form (Post/Redirect/Get)
// with the failed input held in a flash, so refresh cannot re-send.
func (s *Server) renderComposeError(w http.ResponseWriter, r *http.Request, title, formURL, action, cancel string, in app.SendInput, err error) {
	f := composeFlash{Title: title, Action: action, Cancel: cancel, Err: safeErrorMessage(err, "Could not send the message. Please try again."), Input: in}
	dest := formURL
	if tok := s.flashes.put(f, composeFlashSize(f)); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) parseMessageForm(w http.ResponseWriter, r *http.Request) (app.SendInput, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.Service.Config.MaxMessageBytes+1<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		return app.SendInput{}, fmt.Errorf("could not read the form")
	}
	atts, err := s.formAttachments(r)
	if err != nil {
		return app.SendInput{}, err
	}
	if v, ok := s.flashes.peek(r.Form.Get("_flash")); ok {
		if f, ok := v.(composeFlash); ok {
			atts = append(atts, f.Input.Attachments...)
		}
	}
	return app.SendInput{
		FromAddress: strings.TrimSpace(r.Form.Get("sender")),
		To:          formAddresses(r, "to"),
		CC:          formAddresses(r, "cc"),
		BCC:         formAddresses(r, "bcc"),
		Subject:     strings.TrimSpace(r.Form.Get("subject")),
		Text:        r.Form.Get("text"),
		Attachments: atts,
	}, nil
}

func (s *Server) formAttachments(r *http.Request) ([]app.SendAttachment, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	files := r.MultipartForm.File["attachments"]
	out := make([]app.SendAttachment, 0, len(files))
	var total int64
	for _, fh := range files {
		if fh.Size > s.Service.Config.MaxMessageBytes {
			return nil, fmt.Errorf("attachment %q exceeds the maximum message size", fh.Filename)
		}
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, s.Service.Config.MaxMessageBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > s.Service.Config.MaxMessageBytes {
			return nil, fmt.Errorf("attachments exceed the maximum message size")
		}
		if len(data) == 0 {
			continue
		}
		out = append(out, app.SendAttachment{Filename: fh.Filename, ContentType: fh.Header.Get("Content-Type"), Content: data})
	}
	return out, nil
}

func (s *Server) uiMessageLabels(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox message resolves its labels through the remote
		// metadata cache (labels are local metadata on both kinds).
		if s.remoteMessageSetLabels(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	label := strings.TrimSpace(r.Form.Get("label"))
	current := append([]string{}, m.Labels...)
	switch r.Form.Get("action") {
	case "add":
		if label == "" {
			http.Redirect(w, r, "/ui/messages/"+m.ID, 303)
			return
		}
		current = append(current, label)
	case "remove":
		kept := current[:0]
		for _, l := range current {
			if !strings.EqualFold(l, label) {
				kept = append(kept, l)
			}
		}
		current = kept
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	ev, err := s.Service.Store.ReplaceMessageLabels(r.Context(), p, m.ID, current)
	if err != nil {
		http.Redirect(w, r, "/ui/messages/"+m.ID+"?notice="+url.QueryEscape("Invalid label: "+err.Error()), 303)
		return
	}
	s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
	s.Service.Hub.Publish(ev)
	http.Redirect(w, r, "/ui/messages/"+m.ID, 303)
}

func (s *Server) uiMessageRead(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox message id resolves through the remote reader.
		if s.remoteMessageSetRead(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	read := r.Form.Get("read") == "1"
	if ev, err := s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); err != nil {
		s.uiError(w, err, 400)
		return
	} else {
		s.publishStateEvent(ev)
	}
	target := "/ui/inboxes/" + m.InboxID
	if m.Direction == "outbound" {
		target += "/sent"
	}
	http.Redirect(w, r, target, 303)
}

func (s *Server) uiMessageSpam(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox's message id resolves through the remote reader
		// (spam is its remote Spam-role folder).
		if s.remoteMessageSpam(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	spam := r.Form.Get("spam") == "1"
	if _, ev, err := s.Service.Store.SetMessageSpam(r.Context(), p, m.ID, spam); err != nil {
		s.uiError(w, err, 400)
		return
	} else if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	target := "/ui/inboxes/" + m.InboxID
	if spam {
		target += "/spam"
	}
	http.Redirect(w, r, target, 303)
}

func (s *Server) uiMessageDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	// GetMessage enforces the per-mailbox Read role and TrashMessage the
	// Assistant/Owner role, so a non-admin mailbox user can act on their own
	// mail without the account-wide Admin flag. Delete moves the message to
	// Trash; it is not erased until purged.
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox's message id resolves through the remote reader and
		// moves to the inbox's remote Trash-role folder.
		if s.remoteMessageTrash(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	_, ev, err := s.Service.Store.TrashMessage(r.Context(), p, m.ID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	target := "/ui/inboxes/" + m.InboxID
	if m.Direction == "outbound" {
		target += "/sent"
	}
	target += "?notice=" + url.QueryEscape("1 message moved to trash")
	http.Redirect(w, r, target, 303)
}

func (s *Server) uiMessageRestore(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox's message id resolves through the remote reader and
		// moves from its remote Trash folder back to the remote Inbox.
		if s.remoteMessageRestore(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	_, ev, err := s.Service.Store.RestoreMessage(r.Context(), p, m.ID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	http.Redirect(w, r, "/ui/inboxes/"+m.InboxID+"/trash", 303)
}

func (s *Server) uiMessagePurge(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox's message id resolves through the remote reader and
		// is erased with a UID-targeted remote expunge.
		if s.remoteMessagePurge(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	path, _, ev, err := s.Service.Store.PurgeMessage(r.Context(), p, m.ID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	if path != "" {
		s.removeDataFile(path)
	}
	s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
	s.Service.Hub.Publish(ev)
	http.Redirect(w, r, "/ui/inboxes/"+m.InboxID+"/trash?notice=Message+deleted+permanently", 303)
}

func (s *Server) uiInboxTrashEmpty(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	paths, events, err := s.Service.Store.EmptyTrash(r.Context(), p, box.ID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	for _, ev := range events {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(ev)
	}
	notice := fmt.Sprintf("Trash emptied (%d message", len(events))
	if len(events) != 1 {
		notice += "s"
	}
	notice += ")"
	http.Redirect(w, r, "/ui/inboxes/"+box.ID+"/trash?notice="+url.QueryEscape(notice), 303)
}

func (s *Server) uiMessageHTML(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	// GetMessage enforces the per-mailbox Read role, so any user with access to
	// the message can view its sanitized HTML, not only account Admins.
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		// A standalone inbox message renders its sanitized HTML from the live
		// remote body, through the same sanitizer the local reader uses.
		if s.remoteMessageHTML(w, r, p, r.PathValue("id")) {
			return
		}
		http.Error(w, "message not found", 404)
		return
	}
	// Remote images load only when the viewer explicitly opts in (?remote=1);
	// the default blocks them so opening mail cannot phone the sender. CID
	// inlines (same-origin) and data: images always load.
	remote := r.URL.Query().Get("remote") == "1"
	atts, _ := s.Service.Store.ListAttachments(r.Context(), p, m.ID)
	body := rewriteCIDs(m.HTML, atts)
	body = htmlsanitize.Sanitize(body)
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
}

func (s *Server) uiAttachment(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, false)
}

func (s *Server) uiAttachmentInline(w http.ResponseWriter, r *http.Request) {
	s.serveAttachment(w, r, true)
}

func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request, inline bool) {
	p := principal(r)
	// GetAttachment resolves the owning message under the per-mailbox Read
	// role, so access is scoped to the mailbox rather than the Admin flag.
	a, m, err := s.Service.Store.GetAttachment(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "attachment not found", 404)
		return
	}
	disposition := "attachment"
	contentType := "application/octet-stream"
	if inline {
		if ct := normalizeContentType(a.ContentType); isInlineImage(ct) {
			disposition = "inline"
			contentType = ct
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		}
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	path, perr := s.dataPath(m.RawPath)
	if perr != nil {
		s.Log.Error("attachment path rejected", "error", perr)
		return
	}
	if err := mailparse.ExtractAttachment(path, a.PartIndex, w); err != nil {
		s.Log.Error("attachment extraction", "error", err)
	}
}

// inlineImageTypes are the raster image types safe to render inline. Scriptable
// or vector types (notably image/svg+xml) are deliberately excluded and are only
// ever served as downloads.
var inlineImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"image/avif": true,
}

func normalizeContentType(ct string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
}

func isInlineImage(ct string) bool { return inlineImageTypes[ct] }

func actionWithCSRF(path, token string) string {
	if token == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_csrf=" + token
}

func formAddresses(r *http.Request, name string) []string {
	raw := r.Form.Get(name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(ch rune) bool {
		return ch == ',' || ch == ';' || ch == '\n' || ch == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// returnTo resolves the "previous screen" a compose form should redirect to
// after save/send. It prefers the form's return_to field (the page the user
// came from) and falls back to the inbox folder. Only same-origin relative
// paths are accepted to avoid open-redirect.
func returnTo(r *http.Request, inboxID, fallbackFolder string) string {
	dest := strings.TrimSpace(r.Form.Get("return_to"))
	// Reject scheme-relative forms ("//host") and any backslash, which browsers
	// may normalize to a slash and turn into an off-site redirect.
	if dest == "" || !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.Contains(dest, "\\") {
		dest = "/ui/inboxes/" + inboxID
		if fallbackFolder != "" {
			dest += "/" + fallbackFolder
		}
	}
	return dest
}

func rewriteCIDs(body string, atts []model.Attachment) string {
	for _, a := range atts {
		cid := strings.Trim(strings.TrimSpace(a.ContentID), "<>")
		if cid == "" || !isInlineImage(normalizeContentType(a.ContentType)) {
			continue
		}
		url := "/ui/attachments/" + a.ID + "/inline"
		body = strings.ReplaceAll(body, "cid:<"+cid+">", url)
		body = strings.ReplaceAll(body, "cid:"+cid, url)
	}
	return body
}

// handoffBadge renders a draft's latest RemoteDraft handoff state on the drafts
// list so a handoff draft is never shown as a plain (or approval) draft. It shows
// the publication state and, independently, an unavailable/failed notification.
const handoffBadge = `{{define "handoff-badge"}}{{if eq .Publication "published"}}<span class="draft-status sent">Handoff published</span> {{else if eq .Publication "ambiguous"}}<span class="draft-status rejected">Handoff ambiguous</span> {{else if eq .Publication "failed"}}{{if eq .LastError "cancelled"}}<span class="draft-status rejected">Handoff cancelled</span> {{else}}<span class="draft-status rejected">Handoff failed</span> {{end}}{{else}}<span class="draft-status pending">Handoff pending</span> {{end}}{{end}}`

// handoffHistory is the RemoteDraft handoff history panel. It is independent of
// the local draft: a published handoff's local draft is cleaned up, but the
// terminal record (with its publication and notification state) is retained. A
// pending handoff can be cancelled; a terminal (failed/cancelled) one can be
// re-requested. It never offers approval controls for a remote job.
const handoffHistory = `{{define "handoff-history"}}{{if .Handoffs}}<section class="card" data-live-list><div class="card-head"><h2>Draft handoffs</h2></div><p class="muted small">One-way handoffs of drafts to this inbox's connected remote Drafts folder. Publication and notification are tracked separately; the record is kept after the local draft is cleaned up.</p><div class="mailheader"><span></span><span></span><span>Subject</span><span>Publication</span><span>Notification</span><span>Requested</span><span></span></div><div class="mailrows">{{range .Handoffs}}<div class="mailrow"><span class="mailcheck"></span><span class="maildot"></span><span class="mailsubject">{{if eq .Publication "published"}}<span class="draft-status sent">Published</span> {{else if eq .Publication "ambiguous"}}<span class="draft-status rejected">Ambiguous</span> {{else if eq .Publication "failed"}}{{if eq .LastError "cancelled"}}<span class="draft-status rejected">Cancelled</span> {{else}}<span class="draft-status rejected">Failed</span> {{end}}{{else}}<span class="draft-status pending">Pending</span> {{end}}{{if .MessageID}}<span class="muted small">{{.MessageID}}</span>{{end}}{{if .RemoteFolder}} <span class="muted small">→ {{.RemoteFolder}}</span>{{end}}{{if .LastError}} <span class="muted small">— {{.LastError}}</span>{{end}}</span><span>{{if eq .NotificationStatus "sent"}}<span class="pill">sent</span>{{else if eq .NotificationStatus "queued"}}<span class="pill amber">queued</span>{{else if eq .NotificationStatus "failed"}}<span class="pill danger">notification failed</span>{{else}}<span class="muted small">notification unavailable</span>{{end}}</span><span class="maildate">{{mailDate .RequestedAt}}</span><span class="mailaction">{{if eq .Publication "pending"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.DraftID}}/cancel-handoff" data-confirm="Withdraw this handoff? The remote draft is not removed."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Cancel</button></form>{{else if eq .Publication "ambiguous"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.DraftID}}/cancel-handoff" data-confirm="Resolve this ambiguous handoff and return the draft to editable? A remote copy, if any, is not removed."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Resolve</button></form>{{else if eq .Publication "failed"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.DraftID}}/retry-handoff" data-confirm="Re-request the handoff for this draft?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Retry</button></form>{{end}}</span></div>{{end}}</div></section>{{end}}{{end}}`
