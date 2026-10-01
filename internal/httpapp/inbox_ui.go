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
	"github.com/dellarb/mailmoose/internal/store"
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

const inboxBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr">{{.Inbox.Address}}</span></h1>{{if .UnreadCount}}<span class="pill unread-pill">{{.UnreadCount}} unread</span>{{end}}</div>
<div class="inboxbar"><a class="btn" href="/ui/inboxes/{{.Inbox.ID}}/compose">Compose</a><a class="btn secondary{{if eq .Folder "inbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}">Inbox</a><a class="btn secondary{{if eq .Folder "spam"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/spam">Spam{{if .SpamCount}} ({{.SpamCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "trash"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/trash">Trash{{if .TrashCount}} ({{.TrashCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "drafts"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/drafts">Drafts{{if .DraftCount}} ({{.DraftCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "outbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/outbox">Outbox{{if .OutboxCount}} ({{.OutboxCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "sent"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/sent">Sent</a><a class="btn secondary{{if eq .Folder "trash"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/trash">Trash{{if .TrashCount}} ({{.TrashCount}}){{end}}</a>{{if eq .Folder "trash"}}<form method="post" action="/ui/inboxes/{{.Inbox.ID}}/trash/empty" class="bulkbar" data-confirm="Permanently delete all messages in Trash? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Empty trash</button></form>{{end}}<form id="bulk-form" class="bulkbar" method="post" action="/ui/inboxes/{{.Inbox.ID}}/bulk"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="folder" value="{{.Folder}}">{{if .Principal.Admin}}<a class="btn secondary icon-btn" href="/?inbox={{.Inbox.ID}}" title="Inbox settings" aria-label="Inbox settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg></a>{{end}}{{if eq .Folder "trash"}}<button name="action" value="restore" class="secondary">Restore</button><button name="action" value="purge" class="secondary danger" data-confirm="Delete permanently? This cannot be undone.">Delete forever</button>{{else}}<button name="action" value="read" class="secondary">Mark read</button><button name="action" value="unread" class="secondary">Mark unread</button><button name="action" value="delete" class="secondary danger" data-confirm="Move selected messages to Trash?">Move to trash</button>{{end}}</form></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
{{if not .OutboundReady}}<div class="banner warn">{{if .SendingPausedExternal}}Sending paused — configure the sending connector for the selected sender ({{.SendingPausedAddress}}). Mail will queue. <a href="{{.SendingPausedURL}}">Configure</a>.{{else}}Sending is paused until a provider is configured for this domain. Mail will queue. <a href="{{.DomainSendingSettingsURL}}">Add one</a>.{{end}}</div>{{end}}
{{if not .InboundReady}}<div class="banner warn">Not receiving — no receive path is configured for this domain. <a href="{{.DomainReceivingSettingsURL}}">Add one</a>.</div>{{end}}
{{if .SendRequests}}<section class="card"><div class="card-head"><h2>Draft send requests</h2></div><div class="mailheader"><span></span><span></span><span>To</span><span>Subject</span><span class="hcenter">Date</span><span class="hcenter">Size</span><span></span></div><div class="mailrows">{{range .SendRequests}}<div class="mailrow"><a class="mailrowlink" href="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.Draft.ID}}/edit"><span class="maildot"></span><span class="mailsender">{{join .Draft.To ", "}}</span><span class="mailsubject">{{if eq .Request.Status "pending"}}{{if eq .Request.NotificationStatus "failed"}}<span class="pill danger">Notification failed</span> {{else if eq .Request.NotificationStatus "queued"}}<span class="pill amber">Notification queued</span> {{else}}<span class="pill amber">Awaiting approval</span> {{end}}{{end}}{{if .Draft.Subject}}{{.Draft.Subject}}{{else}}(no subject){{end}}{{if .Draft.Text}} <span class="mailsnippet">— {{snippet .Draft.Text 80}}</span>{{end}}{{if .Request.Feedback}} <span class="muted small">— {{.Request.Feedback}}</span>{{end}}</span><span class="maildate">{{mailDate .Request.RequestedAt}}</span><span class="mailsize">{{filesize .SizeBytes}}</span></a><span class="mailaction">{{if eq .Request.Status "pending"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.Draft.ID}}/approve" data-confirm="Approve and send this draft?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="btn-sm">Send</button></form>{{end}}</span></div>{{end}}</div></section>{{end}}
<section class="card">{{if .Messages}}<div class="mailheader"><span class="mailcheck"><input type="checkbox" id="select-all" aria-label="Select all messages"></span><span></span><span>{{if eq .Folder "sent"}}To{{else}}From{{end}}</span><span>Subject</span><span class="hcenter">Date</span><span class="hcenter">Size</span><span></span></div><div class="mailrows">{{range .Messages}}<div class="mailrow{{if not .Read}} unread{{end}}"><span class="mailcheck"><input type="checkbox" name="ids" value="{{.ID}}" form="bulk-form" aria-label="Select message"></span><a class="mailrowlink" href="/ui/messages/{{.ID}}"><span class="maildot">{{if not .Read}}<span class="dot"></span>{{end}}</span><span class="mailsender">{{if eq $.Folder "sent"}}{{join .To ", "}}{{else}}{{if .From.Name}}{{.From.Name}}{{else}}{{.From.Address}}{{end}}{{end}}</span><span class="mailsubject">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .HasAttachments}} <span class="pill">attach</span>{{end}}{{range .Labels}} <span class="labelpill">{{.}}</span>{{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{mailDate .CreatedAt}}</span><span class="mailsize">{{filesize .SizeBytes}}</span></a><span class="mailaction">{{if eq $.Folder "spam"}}<form method="post" action="/ui/messages/{{.ID}}/spam"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="spam" value="0"><button class="secondary btn-sm">Not spam</button></form>{{else if eq $.Folder "trash"}}<form method="post" action="/ui/messages/{{.ID}}/restore"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Restore</button></form>{{else}}<form method="post" action="/ui/messages/{{.ID}}/read"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="read" value="{{if .Read}}0{{else}}1{{end}}"><button class="secondary btn-sm">{{if .Read}}Mark unread{{else}}Mark read{{end}}</button></form>{{end}}</span></div>{{end}}</div>{{if .HasMore}}<p><a href="{{.BasePath}}?before={{.Before}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No messages in this folder yet.</p>{{end}}</section>`

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

const draftsBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar"><a class="btn" href="/ui/inboxes/{{.Inbox.ID}}/compose">Compose</a><a class="btn secondary{{if eq .Folder "inbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}">Inbox</a><a class="btn secondary{{if eq .Folder "drafts"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/drafts">Drafts{{if .DraftCount}} ({{.DraftCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "outbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/outbox">Outbox{{if .OutboxCount}} ({{.OutboxCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "sent"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/sent">Sent</a><a class="btn secondary{{if eq .Folder "trash"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/trash">Trash{{if .TrashCount}} ({{.TrashCount}}){{end}}</a></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
<section class="card">{{if .Drafts}}<div class="mailheader drafts"><span></span><span></span><span>To</span><span>Subject</span><span class="hcenter">Updated</span><span></span></div><div class="mailrows">{{range .Drafts}}<div class="mailrow drafts"><a class="mailrowlink" href="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/edit"><span class="maildot"></span><span class="mailsender">{{join .To ", "}}</span><span class="mailsubject">{{if .SendRequest}}{{if eq .SendRequest.Status "pending"}}<span class="draft-status pending"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 8h10"/><path d="m8.5 3.5 4.5 4.5-4.5 4.5"/></svg>Pending Send</span> {{else if eq .SendRequest.Status "rejected"}}<span class="draft-status rejected">Rejected</span> {{else if eq .SendRequest.Status "approved"}}<span class="draft-status sent">Sent</span> {{end}}{{end}}{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{mailDate .UpdatedAt}}</span></a><span class="mailaction">{{if .SendRequest}}{{if eq .SendRequest.Status "pending"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/approve" data-confirm="Approve and send this draft?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="btn-sm">Send</button></form>{{end}}{{end}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/drafts/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger" data-confirm="Delete this draft?">Delete</button></form></span></div>{{end}}</div>{{else}}<p class="muted">No drafts yet.</p>{{end}}</section>`

const outboxBody = `<div class="inboxhead"><h1 class="inboxtitle">{{.Inbox.DisplayName}} <span class="inboxaddr">{{.Inbox.Address}}</span></h1></div>
<div class="inboxbar"><a class="btn" href="/ui/inboxes/{{.Inbox.ID}}/compose">Compose</a><a class="btn secondary{{if eq .Folder "inbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}">Inbox</a><a class="btn secondary{{if eq .Folder "drafts"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/drafts">Drafts{{if .DraftCount}} ({{.DraftCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "outbox"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/outbox">Outbox{{if .OutboxCount}} ({{.OutboxCount}}){{end}}</a><a class="btn secondary{{if eq .Folder "sent"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/sent">Sent</a><a class="btn secondary{{if eq .Folder "trash"}} active{{end}}" href="/ui/inboxes/{{.Inbox.ID}}/trash">Trash{{if .TrashCount}} ({{.TrashCount}}){{end}}</a></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}
<section class="card">{{if .Messages}}<div class="mailheader"><span></span><span>To</span><span>Subject</span><span class="hcenter">Status</span><span></span></div><div class="mailrows">{{range .Messages}}<div class="mailrow outbox"><a class="mailrowlink" href="/ui/messages/{{.ID}}"><span class="maildot"></span><span class="mailsender">{{join .To ", "}}</span><span class="mailsubject">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}{{if .Text}} <span class="mailsnippet">— {{snippet .Text 80}}</span>{{end}}</span><span class="maildate">{{if eq .Status "failed"}}<span class="pill danger">Failed</span>{{else if .Sending}}<span class="pill amber">Sending…</span>{{else}}<span class="pill">Pending</span>{{end}}</span></a><span class="mailaction">{{if eq .Status "failed"}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/outbox/{{.ID}}/retry"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Retry</button></form>{{end}}<form method="post" action="/ui/inboxes/{{$.Inbox.ID}}/outbox/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger" data-confirm="Remove this message from the outbox?">Delete</button></form></span></div>{{end}}</div>{{else}}<p class="muted">No messages in the outbox.</p>{{end}}</section>`

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
	if f.Input.InboxID != "" {
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, f.Input.InboxID); err == nil {
			data.ComposeFromOptions, data.ComposeFrom = composeFromOptions(box, f.Input.FromAddress)
		}
	}
	s.render(w, r, composeBody, data)
}

// externalSenderFor returns the external sending alias the inbox's
// default_sender names, or false when the default is the primary or a managed
// alias (both resolve through a domain connector).
func externalSenderFor(box model.Inbox) (model.ExternalAlias, bool) {
	if strings.TrimSpace(box.DefaultSender) == "" {
		return model.ExternalAlias{}, false
	}
	for _, a := range box.ExternalAliases {
		if strings.EqualFold(a.Address, box.DefaultSender) {
			return a, true
		}
	}
	return model.ExternalAlias{}, false
}

// composeFromOptions returns the selectable From addresses for an inbox
// (primary first, then managed aliases, then external sending aliases) and the
// one to preselect: the requested address when present, otherwise the inbox
// default, otherwise the primary. A label shows "Name <address>" when the
// inbox or alias has a display name.
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
	for _, a := range box.ExternalAliases {
		options = append(options, fromOption{Address: a.Address, Label: label(a.Address, a.DisplayName)})
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

func (s *Server) uiDrafts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	drafts, err := s.Service.Store.ListDrafts(r.Context(), p, box.ID)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, box.ID)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, box.ID)
	s.render(w, r, draftsBody, pageData{
		Title:       box.Address + " · Drafts",
		Principal:   p,
		CSRF:        csrf(r),
		Account:     acc,
		Inbox:       &box,
		Drafts:      drafts,
		Folder:      "drafts",
		DraftCount:  draftCount,
		OutboxCount: outboxCount,
		Notice:      r.URL.Query().Get("notice"),
	})
}

func (s *Server) uiOutbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	msgs, err := s.Service.Store.ListOutbox(r.Context(), p, box.ID, inboxPageSize)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, box.ID)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, box.ID)
	s.render(w, r, outboxBody, pageData{
		Title:       box.Address + " · Outbox",
		Principal:   p,
		CSRF:        csrf(r),
		Account:     acc,
		Inbox:       &box,
		Messages:    msgs,
		Folder:      "outbox",
		DraftCount:  draftCount,
		OutboxCount: outboxCount,
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
		http.Error(w, err.Error(), 400)
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
			http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
		return
	}
	// Persist any uploaded attachments to disk.
	atts, err := s.formAttachments(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if _, err = s.persistDraftAttachments(r.Context(), p, d.ID, atts); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if action == "request-send" {
		if _, err = s.Service.RequestSend(r.Context(), p, d.ID, false); err != nil {
			http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	direction := "inbound"
	basePath := "/ui/inboxes/" + id
	spamOnly := false
	trashed := false
	switch folder {
	case "sent":
		direction = "outbound"
		basePath += "/sent"
	case "spam":
		spamOnly = true
		basePath += "/spam"
	case "trash":
		trashed = true
		basePath += "/trash"
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	msgs, err := s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{InboxID: id, Direction: direction, SpamOnly: spamOnly, Trashed: trashed, IncludeSpam: trashed, Before: before, Limit: inboxPageSize + 1})
	if err != nil {
		http.Error(w, err.Error(), 400)
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
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	spamCount, _ := s.Service.Store.CountSpam(r.Context(), p, id)
	trashCount, _ := s.Service.Store.CountTrash(r.Context(), p, id)
	draftCount, _ := s.Service.Store.CountDrafts(r.Context(), p, id)
	outboxCount, _ := s.Service.Store.CountOutbox(r.Context(), p, id)
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	// Sending readiness follows the selected/default sender's connector: an
	// external sending alias is ready only when its own connector is set,
	// otherwise the inbox domain's sending config applies.
	outboundReady := true
	pausedExternal := false
	pausedAddress := ""
	pausedURL := ""
	if a, ok := externalSenderFor(box); ok {
		outboundReady = a.Configured
		if !a.Configured {
			pausedExternal = true
			pausedAddress = a.Address
			pausedURL = externalAliasConfigureURL(a.ID)
		}
	} else {
		_, outErr := s.Service.Store.ResolveDomainSendingConfig(r.Context(), p.AccountID, box.DomainID)
		outboundReady = outErr == nil
	}
	recvDomain, inErr := s.Service.Store.GetDomain(r.Context(), p.AccountID, box.DomainID)
	var sendRequests []SendRequestRow
	if folder == "inbox" {
		if all, lerr := s.Service.Store.ListSendRequests(r.Context(), p, id, true, 20); lerr == nil {
			for _, sr := range all {
				if sr.Status != model.SendRequestPending {
					continue
				}
				d, gerr := s.Service.Store.GetDraft(r.Context(), p, sr.DraftID)
				if gerr != nil {
					continue
				}
				atts, _ := s.Service.Store.ListDraftAttachments(r.Context(), p, d.ID)
				sendRequests = append(sendRequests, SendRequestRow{Request: sr, Draft: d, SizeBytes: draftDisplaySize(d, atts)})
			}
		}
	}
	s.render(w, r, inboxBody, pageData{
		Title:                      box.Address,
		Principal:                  p,
		CSRF:                       csrf(r),
		Account:                    acc,
		Inbox:                      &box,
		Messages:                   msgs,
		SendRequests:               sendRequests,
		HasMore:                    hasMore,
		Before:                     cursor,
		Folder:                     folder,
		BasePath:                   basePath,
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
	})
}

func (s *Server) uiBulk(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	box, err := s.Service.Store.GetInbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	action := r.Form.Get("action")
	ids := r.Form["ids"]
	count := 0
	for _, id := range ids {
		m, err := s.Service.Store.GetMessage(r.Context(), p, id)
		if err != nil || m.InboxID != box.ID {
			continue
		}
		switch action {
		case "read", "unread":
			read := action == "read"
			if err := s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); err == nil {
				count++
			}
		case "delete":
			if _, ev, err := s.Service.Store.TrashMessage(r.Context(), p, m.ID); err == nil {
				if ev != nil {
					s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
					s.Service.Hub.Publish(*ev)
				}
				count++
			}
		case "restore":
			if _, ev, err := s.Service.Store.RestoreMessage(r.Context(), p, m.ID); err == nil {
				if ev != nil {
					s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
					s.Service.Hub.Publish(*ev)
				}
				count++
			}
		case "purge":
			path, _, ev, err := s.Service.Store.PurgeMessage(r.Context(), p, m.ID)
			if err != nil {
				continue
			}
			if path != "" {
				s.removeDataFile(path)
			}
			s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
			s.Service.Hub.Publish(ev)
			count++
		default:
			http.Error(w, "unknown action", 400)
			return
		}
	}
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
	}
	base := "/ui/inboxes/" + box.ID
	switch r.Form.Get("folder") {
	case "sent":
		base += "/sent"
	case "trash":
		base += "/trash"
	}
	http.Redirect(w, r, base+"?notice="+url.QueryEscape(notice), 303)
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
			http.Error(w, err.Error(), 400)
			return
		}
		// Persist uploaded attachments.
		atts, aerr := s.formAttachments(r)
		if aerr != nil {
			http.Error(w, aerr.Error(), 400)
			return
		}
		if _, err = s.persistDraftAttachments(r.Context(), p, d.ID, atts); err != nil {
			http.Error(w, err.Error(), 500)
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
		http.Error(w, "message not found", 404)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	data := pageData{Principal: p, CSRF: csrf(r), Account: acc, ComposeCancel: "/ui/messages/" + m.ID}
	if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, m.InboxID); err == nil {
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
		title := "Reply"
		if kind == "forward" {
			title = "Forward"
		}
		s.renderComposeError(w, r, title, formURL, "/ui/messages/"+m.ID+"/"+kind, "/ui/messages/"+m.ID, in, err)
		return
	}
	in.InboxID = m.InboxID
	title := "Reply"
	action := "/ui/messages/" + m.ID + "/reply"
	if kind == "forward" {
		title = "Forward"
		action = "/ui/messages/" + m.ID + "/forward"
		in.ForwardOfMessageID = m.ID
	} else {
		in.ReplyToMessageID = m.ID
	}
	s.submitMessage(w, r, p, in, title, formURL, action, "/ui/messages/"+m.ID)
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
	f := composeFlash{Title: title, Action: action, Cancel: cancel, Err: err.Error(), Input: in}
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
		http.Error(w, err.Error(), 400)
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
		http.Error(w, "message not found", 404)
		return
	}
	read := r.Form.Get("read") == "1"
	if err := s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read); err != nil {
		http.Error(w, err.Error(), 400)
		return
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
		http.Error(w, "message not found", 404)
		return
	}
	spam := r.Form.Get("spam") == "1"
	if _, ev, err := s.Service.Store.SetMessageSpam(r.Context(), p, m.ID, spam); err != nil {
		http.Error(w, err.Error(), 400)
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
		http.Error(w, "message not found", 404)
		return
	}
	_, ev, err := s.Service.Store.TrashMessage(r.Context(), p, m.ID)
	if err != nil {
		http.Error(w, err.Error(), 400)
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
	http.Redirect(w, r, target, 303)
}

func (s *Server) uiMessageRestore(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "message not found", 404)
		return
	}
	_, ev, err := s.Service.Store.RestoreMessage(r.Context(), p, m.ID)
	if err != nil {
		http.Error(w, err.Error(), 400)
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
		http.Error(w, "message not found", 404)
		return
	}
	path, _, ev, err := s.Service.Store.PurgeMessage(r.Context(), p, m.ID)
	if err != nil {
		http.Error(w, err.Error(), 400)
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
		http.Error(w, err.Error(), 400)
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
