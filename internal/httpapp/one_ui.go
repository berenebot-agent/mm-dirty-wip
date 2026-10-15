package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

const oneProviderTile = `<button type="button" class="provider-choice" data-standalone-provider="one"><span class="provider-logo" aria-hidden="true">One</span><span class="provider-text"><span class="provider-title">One (Easy Gmail Setup)</span><span class="provider-desc">Connect Gmail with your own One account. No Google Cloud app required. Email data passes through One.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button>`

// oneAdvancedButton is the same One option offered alongside "Continue with
// Google Gmail" on the Google advanced-setup warning step, so the easier route
// stays one click away. It is spliced into that panel at the ONE_ADVANCED_BUTTON
// marker so One keeps a single definition.
const oneAdvancedButton = `<button type="button" class="provider-choice provider-choice-advanced" data-google-advanced-one><span class="provider-logo" aria-hidden="true">One</span><span class="provider-text"><span class="provider-title">One (Easy Gmail Setup)</span><span class="provider-desc">Connect Gmail through your own One account instead. No Google Cloud app required. Email data passes through One.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button>`

// wizardBackNavOne is the One setup page's left-aligned Back control, matching
// the wizard steps. The One page is reached from the Add-inbox dialog, the
// wizard's One option, or a direct link, so Back follows the browser history to
// return to whichever screen the user came from and falls back to the dashboard.
const wizardBackNavOne = `<div class="wizard-step-nav"><button type="button" class="secondary wizard-back" data-one-back aria-label="Back">Back</button></div>`

var dashboardBody = stringsReplaceOneTile(strings.Replace(dashboardBaseBody,
	`<section class="card" style="grid-column:1/-1" data-open-inbox=`,
	`<section class="card dashboard-inboxes-card" data-group-scope="{{.Principal.AccountID}}:{{.Principal.UserID}}" data-dashboard-csrf="{{.CSRF}}" style="grid-column:1/-1" data-open-inbox=`, 1))

func stringsReplaceOneTile(body string) string {
	const marker = `</svg></button></div></section><form id="inbox-add-form"`
	body = strings.Replace(body, marker, `</svg></button>`+oneProviderTile+`</div></section><form id="inbox-add-form"`, 1)
	body = strings.Replace(body, `<!--ONE_ADVANCED_BUTTON-->`, oneAdvancedButton, 1)
	return body
}

type oneMailboxStatus struct {
	Name                          string
	InboxID, Email, Status, Error string
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

// uiOne is the add-only One import screen: enter a key, select Gmail mailboxes,
// import. It deliberately carries no account-wide list of imported mailboxes and
// no per-inbox management — a One inbox's key is changed on that inbox itself
// (its Identity tab, which opens the reconnect dialog in place), and an address
// can be imported again only after its existing inbox is deleted. The old
// per-inbox settings view is therefore gone; a bookmarked ?inbox= link lands on
// that inbox instead of a dead end.
func (s *Server) uiOne(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p := principal(r)
	d := pageData{Title: "One · Gmail connections", Principal: p, CSRF: csrf(r), Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"), OneSetupID: r.URL.Query().Get("setup")}
	// The completion panel names its mailboxes with repeated `inbox` params, so
	// the retired-link catch below must not treat that as a per-inbox link.
	if r.URL.Query().Get("imported") == "1" {
		selected := make(map[string]bool)
		for _, id := range r.URL.Query()["inbox"] {
			selected[id] = true
		}
		for _, b := range s.oneImportSummary(r, p, selected) {
			d.OneMailboxes = append(d.OneMailboxes, b)
		}
		d.Title = "One import complete"
		s.render(w, r, oneCompleteBody, d)
		return
	}
	if inbox := strings.TrimSpace(r.URL.Query().Get("inbox")); inbox != "" {
		s.redirectOneInboxLink(w, r, inbox)
		return
	}
	if d.OneSetupID != "" {
		choices, err := s.remoteMailbox().DiscoverOne(r.Context(), p, d.OneSetupID)
		if err != nil {
			d.Error = "Import setup expired or discovery failed; enter the key again"
			d.OneSetupID = ""
		} else if len(choices) == 0 {
			d.Notice = "No Gmail accounts found. Connect Gmail in One → Connected Apps, then refresh connections."
		} else {
			d.OneChoices = choices
			d.OneHistoryWindows = app.OneHistoryWindowOptions()
		}
	}
	s.render(w, r, oneBody, d)
}

// oneImportSummary builds the completion panel's per-mailbox cards for the
// imported inbox ids, in the order the import reported them.
func (s *Server) oneImportSummary(r *http.Request, p model.Principal, selected map[string]bool) []oneMailboxStatus {
	out := []oneMailboxStatus{}
	for _, b := range s.oneBindings(r.Context(), p) {
		if !selected[b.InboxID] {
			continue
		}
		v := oneMailboxStatus{InboxID: b.InboxID, Name: b.GmailEmail, Email: b.GmailEmail, Status: b.Status, Error: b.LastError}
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, b.InboxID); err == nil && box.DisplayName != "" {
			v.Name = box.DisplayName
		}
		out = append(out, v)
	}
	return out
}

// oneBindings loads the account's One bindings, returning an empty list rather
// than failing the page: a missing list only costs the completion cards.
func (s *Server) oneBindings(ctx context.Context, p model.Principal) []store.OneBinding {
	bindings, err := s.Service.Store.ListOneBindings(ctx, p.AccountID)
	if err != nil {
		return nil
	}
	return bindings
}

func (s *Server) uiOneSave(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	id, err := s.remoteMailbox().PrepareOneImport(r.Context(), principal(r), r.Form.Get("api_key"))
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?setup="+url.QueryEscape(id), 303)
}

// redirectOneInboxLink sends a retired /ui/connections/one?inbox=<id> link to the
// inbox it named, asking the page to open the in-place reconnect dialog. An id
// that no longer exists (the inbox was deleted) falls back to the dashboard
// rather than a dead end.
func (s *Server) redirectOneInboxLink(w http.ResponseWriter, r *http.Request, inbox string) {
	if _, err := s.Service.Store.GetInboxInternal(r.Context(), principal(r).AccountID, inbox); err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+url.PathEscape(inbox)+"?reconnect=1", http.StatusSeeOther)
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

const oneBody = `<section class="card" data-one-page>` + wizardBackNavOne + `<h1>One (Easy Gmail Setup)</h1>
<p>Connect Gmail with your own One account. No Google Cloud app required. <b>Gmail content and API calls pass through One's cloud infrastructure.</b></p>
{{if .Notice}}<div class="ok" role="status">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error" role="alert">{{.Error}}</div>{{end}}
{{if .OneSetupID}}<h2>Select mailboxes</h2><p>Only selected Gmail connections are supported at this time.</p>
<div class="muted"><p>A full metadata import spends about one request per message, so a large mailbox can consume a big share of your monthly quota. If a count below looks high, choose a shorter history window for that mailbox.</p></div>
<form method="post" action="/ui/connections/one/import"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="setup_id" value="{{.OneSetupID}}">{{if .OneChoices}}<div class="table-wrap"><table class="dense one-import-table"><thead><tr><th>Choose</th><th>Mailbox</th><th>Provider</th><th>One status</th><th>Import status</th><th>Messages</th><th>Index history</th></tr></thead><tbody>{{range .OneChoices}}<tr><td><input type="checkbox" name="connection_id" value="{{.ID}}" {{if not .Available}}disabled{{end}} aria-label="Import {{.Email}}"></td><td>{{if .Email}}{{.Email}}{{else}}Identity unavailable{{end}}</td><td>{{.Provider}}</td><td>{{.OneStatus}} · {{.Environment}}</td><td>{{.Status}}</td><td>{{if .MessagesTotal}}~{{.MessagesTotal}}{{else}}unknown{{end}}</td><td><select name="window_{{.ID}}" aria-label="History window for {{.Email}}" {{if not .Available}}disabled{{end}}><option value="" selected>None</option>{{range $.OneHistoryWindows}}<option value="{{.Key}}">{{.Label}}</option>{{end}}</select></td></tr>{{end}}</tbody></table></div>{{end}}<div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><a class="btn secondary" style="margin-right:auto" href="/ui/connections/one?setup={{.OneSetupID}}">Refresh</a><button {{if not .OneChoices}}disabled{{end}}>Import selected</button></div></form>
{{else}}<p>Connect Gmail in <a href="https://app.withone.ai/" target="_blank" rel="noopener noreferrer">One → Connected Apps</a>, then generate a REST key under <b>Developers → API Keys</b>.</p>
<form method="post" action="/ui/connections/one/config" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>One API key</label><input name="api_key" type="text" required autocomplete="off" spellcheck="false" autocapitalize="off" maxlength="4096"><div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><button>Next</button></div></form>{{end}}
<p class="muted small">Changing the key for an inbox you already imported is done on that inbox: open it, then <b>Identity → Advanced → Reconnect Gmail via One</b>. To import an address again, delete its inbox first.</p></section>`
const oneCompleteBody = `<section class="card wizard-box"><h1>Gmail inboxes connected</h1><div class="ok" role="status">{{.Notice}}</div><p>Your inboxes are ready. Messages will appear as the initial sync progresses.</p><div class="grid">{{range .OneMailboxes}}<section class="card"><h2>{{.Name}}</h2><p class="muted">{{.Email}}</p><p>Gmail via One · {{.Status}}</p><div class="dialog-actions"><a class="btn secondary" href="/ui/inboxes/{{.InboxID}}">Open inbox</a></div></section>{{end}}</div><div class="dialog-actions"><a class="btn" href="/">Close</a></div></section>`
const oneImportBody = `<section class="card"><h1>One import results</h1><p>{{.Notice}}</p><ul>{{range .OneImportResults}}<li>{{.ConnectionID}}: {{if .Error}}{{.Error}}{{else}}Imported — <a href="/ui/inboxes/{{.InboxID}}">open mailbox</a>{{end}}</li>{{end}}</ul><a href="/ui/connections/one?setup={{.OneSetupID}}">Refresh connections and retry failed selections</a></section>`
