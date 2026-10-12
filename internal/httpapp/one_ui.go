package httpapp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

const oneProviderTile = `<button type="button" class="provider-choice" data-standalone-provider="one"><span class="provider-logo" aria-hidden="true">One</span><span class="provider-text"><span class="provider-title">One (Easy Gmail Setup)</span><span class="provider-desc">Connect Gmail with your own One account. No Google Cloud app required. Email data passes through One.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button>`

var dashboardBody = stringsReplaceOneTile(dashboardBaseBody)

func stringsReplaceOneTile(body string) string {
	const marker = `</svg></button></div></section><form id="inbox-add-form"`
	return strings.Replace(body, marker, `</svg></button>`+oneProviderTile+`</div></section><form id="inbox-add-form"`, 1)
}

type oneMailboxStatus struct {
	Name                                                 string
	InboxID, Email, Status, Error, IndexStatus, LastSync string
	PollMinutes                                          int
}

func (s *Server) oneAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := principal(r)
	if !p.Admin || p.UserID == "" {
		http.Error(w, "signed-in account administrator required", http.StatusForbidden)
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	return true
}
func (s *Server) oneFailure(w http.ResponseWriter, r *http.Request, err error) {
	message := "One operation failed; refresh settings and try again"
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		message = mb.Message
	} else if errors.Is(err, store.ErrConflict) {
		message = "Configuration changed; local data is unchanged"
	} else if errors.Is(err, store.ErrNotFound) {
		message = "One connection or import setup not found; enter the key again"
	}
	q := url.Values{"error": {message}}
	if inbox := r.Form.Get("inbox_id"); inbox != "" {
		q.Set("inbox", inbox)
	}
	http.Redirect(w, r, "/ui/connections/one?"+q.Encode(), http.StatusSeeOther)
}
func (s *Server) uiOne(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p := principal(r)
	d := pageData{Title: "One · Gmail connections", Principal: p, CSRF: csrf(r), Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"), OneSetupID: r.URL.Query().Get("setup"), OneInboxID: r.URL.Query().Get("inbox")}
	complete := r.URL.Query().Get("imported") == "1"
	bindings, err := s.Service.Store.ListOneBindings(r.Context(), p.AccountID)
	if err != nil {
		http.Error(w, "Cannot load One connections", 500)
		return
	}
	found := d.OneInboxID == ""
	for _, b := range bindings {
		if complete {
			selected := false
			for _, id := range r.URL.Query()["inbox"] {
				if id == b.InboxID {
					selected = true
					break
				}
			}
			if !selected {
				continue
			}
		}
		if !complete && d.OneInboxID != "" && b.InboxID != d.OneInboxID {
			continue
		}
		found = true
		v := oneMailboxStatus{InboxID: b.InboxID, Name: b.GmailEmail, Email: b.GmailEmail, Status: b.Status, Error: b.LastError, PollMinutes: 5}
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, b.InboxID); err == nil {
			if box.DisplayName != "" {
				v.Name = box.DisplayName
			}
			if box.RemotePollSeconds != nil {
				v.PollMinutes = max(2, min(*box.RemotePollSeconds/60, 30))
			}
		}
		if status, err := s.Service.Store.GetRemoteIndexStatus(r.Context(), p.AccountID, b.InboxID); err == nil {
			v.IndexStatus = status.Status
			if !status.IndexedAt.IsZero() {
				v.LastSync = status.IndexedAt.UTC().Format(time.RFC3339)
			}
		}
		d.OneMailboxes = append(d.OneMailboxes, v)
	}
	if complete {
		d.Title = "One import complete"
		d.OneInboxID = ""
		s.render(w, r, oneCompleteBody, d)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if d.OneSetupID != "" && d.OneInboxID == "" {
		d.OneChoices, err = s.remoteMailbox().DiscoverOne(r.Context(), p, d.OneSetupID)
		if err != nil {
			d.Error = "Import setup expired or discovery failed; enter the key again"
			d.OneSetupID = ""
		} else if len(d.OneChoices) == 0 {
			d.Notice = "No Gmail accounts found. Connect Gmail in One → Connected Apps, then refresh connections."
		} else {
			d.OneHistoryWindows = app.OneHistoryWindowOptions()
		}
	}
	s.render(w, r, oneBody, d)
}
func (s *Server) uiOneSave(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p, inbox := principal(r), r.Form.Get("inbox_id")
	if inbox != "" {
		if err := s.remoteMailbox().SaveOneInboxKey(r.Context(), p, inbox, r.Form.Get("api_key")); err != nil {
			s.oneFailure(w, r, err)
			return
		}
		http.Redirect(w, r, "/ui/connections/one?inbox="+url.QueryEscape(inbox)+"&notice=Connection+saved", 303)
		return
	}
	id, err := s.remoteMailbox().PrepareOneImport(r.Context(), p, r.Form.Get("api_key"))
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?setup="+url.QueryEscape(id), 303)
}

// uiOneReconnect is the in-place reconnect endpoint behind the Identity tab's
// One reconnect dialog. It takes the inbox id and a replacement One API key as
// JSON, verifies the key against the inbox's original Gmail connection, stores
// it only on success, and returns a small JSON verdict so the dialog can report
// a failure without leaving the page or opening the full import wizard.
func (s *Server) uiOneReconnect(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	var body struct {
		InboxID string `json:"inbox_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		writeError(w, 400, "invalid request")
		return
	}
	inbox := strings.TrimSpace(body.InboxID)
	key := strings.TrimSpace(body.APIKey)
	if inbox == "" || key == "" {
		writeError(w, 400, "inbox_id and api_key are required")
		return
	}
	// Only a One-bound inbox has an in-place reconnect; refuse anything else so
	// the endpoint can never be used to attach a key to an unrelated inbox.
	if !s.Service.Store.IsOne(r.Context(), principal(r).AccountID, inbox) {
		writeError(w, 404, "not a One-connected inbox")
		return
	}
	if err := s.remoteMailbox().SaveOneInboxKey(r.Context(), principal(r), inbox, key); err != nil {
		message := "The key was rejected. Check it and try again."
		var mb *model.MailboxError
		if errors.As(err, &mb) && strings.TrimSpace(mb.Message) != "" {
			message = mb.Message
		} else if errors.Is(err, store.ErrNotFound) {
			message = "One connection not found; refresh settings and try again"
		}
		writeJSON(w, 400, map[string]any{"status": "error", "error": message})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "connected"})
}

func (s *Server) uiOneImport(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	id := r.Form.Get("setup_id")
	ids := r.Form["connection_id"]
	// One window selector per selected connection. A missing key defaults to
	// unbounded (import the full history).
	windows := make(map[string]string, len(ids))
	for _, cid := range ids {
		windows[cid] = r.Form.Get("window_" + cid)
	}
	results, err := s.remoteMailbox().ImportOne(r.Context(), principal(r), id, ids, windows)
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	success, failed := 0, 0
	completion := url.Values{"imported": {"1"}}
	for _, result := range results {
		if result.Error != "" {
			failed++
		} else {
			success++
			completion.Add("inbox", result.InboxID)
		}
	}
	notice := strconv.Itoa(success) + " selected mailbox(es) imported; " + strconv.Itoa(failed) + " failed. Each imported inbox owns its connection."
	if failed > 0 {
		s.render(w, r, oneImportBody, pageData{Title: "One import results", Principal: principal(r), CSRF: csrf(r), OneSetupID: id, OneImportResults: results, Notice: notice})
		return
	}
	mailboxes := "mailboxes"
	if success == 1 {
		mailboxes = "mailbox"
	}
	notice = strconv.Itoa(success) + " selected " + mailboxes + " imported."
	completion.Set("notice", notice)
	http.Redirect(w, r, "/ui/connections/one?"+completion.Encode(), 303)
}
func (s *Server) uiOneMailbox(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p, inbox := principal(r), r.Form.Get("inbox_id")
	var err error
	switch r.Form.Get("action") {
	case "delete":
		err = s.remoteMailbox().DeleteOneConnection(r.Context(), p, inbox)
	case "poll":
		minutes, e := strconv.Atoi(r.Form.Get("poll_minutes"))
		if e != nil || minutes < 2 || minutes > 30 {
			err = model.NewMailboxError(model.ErrKindInvalid, "One polling must be between 2 and 30 minutes", false, nil)
		} else if _, e := s.Service.Store.OneBinding(r.Context(), p.AccountID, inbox); e != nil {
			err = e
		} else {
			seconds := minutes * 60
			box, e := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inbox)
			if e != nil {
				err = e
			} else {
				err = s.Service.Store.SetInboxRemoteSync(r.Context(), p.AccountID, inbox, &seconds, box.RemoteFullSyncMinutes)
			}
		}
	default:
		err = model.NewMailboxError(model.ErrKindInvalid, "Unknown One mailbox action", false, nil)
	}
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?inbox="+url.QueryEscape(inbox)+"&notice=Inbox+connection+updated.+Other+inboxes+unchanged", 303)
}

const oneBody = `<section class="card"><h1>One (Easy Gmail Setup)</h1>
<p>Connect Gmail with your own One account. No Google Cloud app required. <b>Gmail content and API calls pass through One's cloud infrastructure.</b></p>
{{if .Notice}}<div class="ok" role="status">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error" role="alert">{{.Error}}</div>{{end}}
{{if not .OneInboxID}}{{if .OneSetupID}}<h2>Select mailboxes</h2><p>Only selected Gmail connections will be imported. Setup expires after 30 minutes; the key is never returned to your browser.</p>
<div class="muted"><p>One is paced at roughly 15–20 provider requests per minute per One account, shared by every inbox imported with this key. A full metadata import spends about one request per message, so a large mailbox can consume a big share of your monthly quota. If a count below looks high, choose a shorter history window for that mailbox.</p></div>
<form method="post" action="/ui/connections/one/import"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="setup_id" value="{{.OneSetupID}}">{{if .OneChoices}}<table><thead><tr><th>Choose</th><th>Mailbox</th><th>Provider</th><th>One status</th><th>Import status</th><th>Messages</th><th>Index history</th></tr></thead><tbody>{{range .OneChoices}}<tr><td><input type="checkbox" name="connection_id" value="{{.ID}}" {{if not .Available}}disabled{{end}} aria-label="Import {{.Email}}"></td><td>{{if .Email}}{{.Email}}{{else}}Identity unavailable{{end}}</td><td>{{.Provider}}</td><td>{{.OneStatus}} · {{.Environment}}</td><td>{{.Status}}</td><td>{{if .MessagesTotal}}~{{.MessagesTotal}}{{else}}unknown{{end}}</td><td><select name="window_{{.ID}}" aria-label="History window for {{.Email}}" {{if not .Available}}disabled{{end}}>{{range $.OneHistoryWindows}}<option value="{{.Key}}">{{.Label}}</option>{{end}}</select></td></tr>{{end}}</tbody></table>{{end}}<div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><a class="btn secondary" style="margin-right:auto" href="/ui/connections/one?setup={{.OneSetupID}}">Refresh</a><button {{if not .OneChoices}}disabled{{end}}>Import selected</button></div></form>
{{else}}<p>Connect Gmail in <a href="https://app.withone.ai/" target="_blank" rel="noopener noreferrer">One → Connected Apps</a>, then generate a REST key under <b>Developers → API Keys</b>. Each selected inbox gets its own encrypted copy of the key and connection.</p>
<form method="post" action="/ui/connections/one/config" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>One API key</label><input name="api_key" type="text" required autocomplete="off" spellcheck="false" autocapitalize="off" maxlength="4096"><div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><button>Next</button></div></form>{{end}}{{end}}
{{if and (not .OneSetupID) .OneMailboxes}}<h2>{{if .OneInboxID}}Inbox connection{{else}}Imported Gmail mailboxes{{end}}</h2>{{range .OneMailboxes}}<section class="card"><h3><a href="/ui/inboxes/{{.InboxID}}">{{.Name}}</a></h3><p class="muted">{{.Email}} · Gmail via One</p><p>Status: {{.Status}}. Index: {{.IndexStatus}}. Last sync: {{.LastSync}}</p>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
{{if $.OneInboxID}}<p>Save a key to replace or reconnect <b>this inbox only</b>. Its original Gmail identity is verified automatically.</p><form method="post" action="/ui/connections/one/config" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="inbox_id" value="{{.InboxID}}"><label>One API key</label><input name="api_key" type="text" required autocomplete="off" spellcheck="false" autocapitalize="off" maxlength="4096"><button>Save</button></form>
<form method="post" action="/ui/inboxes/{{.InboxID}}/sync"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button>Sync now</button></form><form method="post" action="/ui/connections/one/mailbox"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="inbox_id" value="{{.InboxID}}"><label>Polling (minutes, 2–30)</label><input name="poll_minutes" type="number" min="2" max="30" value="{{.PollMinutes}}"><button name="action" value="poll">Save polling</button></form>
<p>Deleting this connection clears only this inbox's credentials. Other inboxes, local metadata and drafts are retained. Nothing is revoked or deleted in One or Gmail.</p><form method="post" action="/ui/connections/one/mailbox"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="inbox_id" value="{{.InboxID}}"><button class="secondary" name="action" value="delete">Delete connection</button></form>
{{else}}<a href="/ui/connections/one?inbox={{.InboxID}}">Connection settings</a>{{end}}</section>{{end}}{{end}}{{if .OneInboxID}}<div class="dialog-actions"><a class="btn secondary" href="/">Close</a></div>{{end}}</section>`
const oneCompleteBody = `<section class="card wizard-box"><h1>Gmail inboxes connected</h1><div class="ok" role="status">{{.Notice}}</div><p>Your inboxes are ready. Messages will appear as the initial sync progresses.</p><div class="grid">{{range .OneMailboxes}}<section class="card"><h2>{{.Name}}</h2><p class="muted">{{.Email}}</p><p>Gmail via One · {{.Status}}</p><div class="dialog-actions"><a class="btn secondary" href="/ui/inboxes/{{.InboxID}}">Open inbox</a></div></section>{{end}}</div><div class="dialog-actions"><a class="btn" href="/">Close</a></div></section>`
const oneImportBody = `<section class="card"><h1>One import results</h1><p>{{.Notice}}</p><ul>{{range .OneImportResults}}<li>{{.ConnectionID}}: {{if .Error}}{{.Error}}{{else}}Imported — <a href="/ui/inboxes/{{.InboxID}}">open mailbox</a>{{end}}</li>{{end}}</ul><a href="/ui/connections/one?setup={{.OneSetupID}}">Refresh connections and retry failed selections</a></section>`
