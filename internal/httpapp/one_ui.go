package httpapp

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

const oneProviderTile = `<button type="button" class="provider-choice" data-standalone-provider="one"><span class="provider-logo" aria-hidden="true">One</span><span class="provider-text"><span class="provider-title">One (Easy Gmail Setup)</span><span class="provider-desc">Connect Gmail with your own One account. No Google Cloud app required. Email data passes through One.</span></span><svg class="provider-chevron" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button>`

// Keep the fourth tile beside the existing provider tiles, not in a separate
// inbox flow. The marker is the end of that chooser before the domain form.
var dashboardBody = stringsReplaceOneTile(dashboardBaseBody)

func stringsReplaceOneTile(body string) string {
	const marker = `</svg></button></div></section><form id="inbox-add-form"`
	return strings.Replace(body, marker, `</svg></button>`+oneProviderTile+`</div></section><form id="inbox-add-form"`, 1)
}

type oneMailboxStatus struct {
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
		message = "Configuration changed or confirmation required; local data is unchanged"
	} else if errors.Is(err, store.ErrNotFound) {
		message = "One configuration not found"
	}
	http.Redirect(w, r, "/ui/connections/one?error="+url.QueryEscape(message), http.StatusSeeOther)
}

func (s *Server) uiOne(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p := principal(r)
	d := pageData{Title: "One · Gmail connections", Principal: p, CSRF: csrf(r), Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error")}
	a, err := s.Service.Store.OneAccount(r.Context(), p.AccountID, p.UserID)
	if err == nil {
		d.OneConfigured = a.Status == "active"
		d.OneConfigStatus = a.Status
		bindings, err := s.Service.Store.OneBindings(r.Context(), p.AccountID, a.ID)
		if err == nil {
			for _, b := range bindings {
				v := oneMailboxStatus{InboxID: b.InboxID, Email: b.GmailEmail, Status: b.Status, Error: b.LastError, PollMinutes: 5}
				if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, b.InboxID); err == nil && box.RemotePollSeconds != nil {
					v.PollMinutes = max(2, min(*box.RemotePollSeconds/60, 30))
				}
				if status, err := s.Service.Store.GetRemoteIndexStatus(r.Context(), p.AccountID, b.InboxID); err == nil {
					v.IndexStatus = status.Status
					if !status.IndexedAt.IsZero() {
						v.LastSync = status.IndexedAt.UTC().Format(time.RFC3339)
					}
				}
				d.OneMailboxes = append(d.OneMailboxes, v)
			}
		}
	}
	if r.URL.Query().Get("discover") == "1" && d.OneConfigured {
		d.OneChoices, err = s.remoteMailbox().DiscoverOne(r.Context(), p)
		if err != nil {
			d.Error = "Cannot discover One connections; test the key or reconnect in One"
		} else if len(d.OneChoices) == 0 {
			d.Notice = "No Gmail accounts found. Connect Gmail in One → Connected Apps, then refresh connections."
		}
	}
	s.render(w, r, oneBody, d)
}

func (s *Server) uiOneSave(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	if err := s.remoteMailbox().SaveOneKey(r.Context(), principal(r), r.Form.Get("api_key")); err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?discover=1&notice=One+key+verified+and+saved", 303)
}

func (s *Server) uiOneTest(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	var err error
	if key := r.Form.Get("api_key"); key != "" {
		err = s.remoteMailbox().TestOneKey(r.Context(), principal(r), key)
	} else {
		err = s.remoteMailbox().TestStoredOne(r.Context(), principal(r))
	}
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?notice=One+key+and+Gmail+actions+verified", 303)
}

func (s *Server) uiOneImport(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	results, err := s.remoteMailbox().ImportOne(r.Context(), principal(r), r.Form["connection_id"])
	if err != nil {
		s.oneFailure(w, r, err)
		return
	}
	success, failed := 0, 0
	for _, result := range results {
		if result.Error != "" {
			failed++
		} else {
			success++
		}
	}
	notice := strconv.Itoa(success) + " selected mailbox(es) imported; " + strconv.Itoa(failed) + " failed. Successful imports were retained. See per-mailbox sync status below."
	if failed > 0 {
		// Preserve safe, per-selection results for explicit retry/conflict handling.
		d := pageData{Title: "One import results", Principal: principal(r), CSRF: csrf(r), OneImportResults: results, Notice: notice}
		s.render(w, r, oneImportBody, d)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?discover=1&notice="+url.QueryEscape(notice), 303)
}

func (s *Server) uiOneRemove(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p := principal(r)
	if err := s.remoteMailbox().RemoveOneAccount(r.Context(), p, r.Form.Get("confirm") == "yes"); err != nil {
		s.oneFailure(w, r, err)
		return
	}
	http.Redirect(w, r, "/ui/connections/one?notice=One+credentials+removed.+Local+metadata+and+drafts+retained.+Upstream+connections+unchanged", 303)
}

func (s *Server) uiOneMailbox(w http.ResponseWriter, r *http.Request) {
	if !s.oneAdmin(w, r) {
		return
	}
	p, inbox := principal(r), r.Form.Get("inbox_id")
	var err error
	switch r.Form.Get("action") {
	case "reconnect":
		err = s.remoteMailbox().ReconnectOne(r.Context(), p, inbox)
	case "disconnect":
		err = s.Service.Store.SetOneConnected(r.Context(), p.AccountID, p.UserID, inbox, false)
	case "poll":
		minutes, e := strconv.Atoi(r.Form.Get("poll_minutes"))
		if e != nil || minutes < 2 || minutes > 30 {
			err = model.NewMailboxError(model.ErrKindInvalid, "One polling must be between 2 and 30 minutes", false, nil)
		} else if a, e := s.Service.Store.OneAccount(r.Context(), p.AccountID, p.UserID); e != nil {
			err = e
		} else if b, e := s.Service.Store.OneBinding(r.Context(), p.AccountID, inbox); e != nil || b.OneAccountID != a.ID {
			err = store.ErrForbidden
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
	http.Redirect(w, r, "/ui/connections/one?notice=One+mailbox+settings+updated", 303)
}

const oneBody = `<section class="card"><h1>One (Easy Gmail Setup)</h1>
<p>Connect Gmail with your own One account. No Google Cloud app required. <b>Gmail content and API calls pass through One's cloud infrastructure.</b> Native Google, Microsoft and IMAP + SMTP remain independent alternatives.</p>
<p>Create a free personal account at <a href="https://app.withone.ai/" target="_blank" rel="noopener noreferrer">One</a>, connect Gmail under <b>Connected Apps</b>, then generate a REST key under <b>Developers → API Keys</b>. This account-wide key may access other connections you own in One.</p>
{{if .Notice}}<div class="ok" role="status">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error" role="alert">{{.Error}}</div>{{end}}
<h2>One account</h2>{{if .OneConfigured}}<p>API key: <code>••••••••</code> (stored encrypted; never returned). Status: {{.OneConfigStatus}}</p><form method="post" action="/ui/connections/one/test"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button>Test stored key</button></form>{{end}}
<form method="post" action="/ui/connections/one/config" autocomplete="off"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>{{if .OneConfigured}}Replace key{{else}}One API key{{end}}</label><input name="api_key" type="password" required autocomplete="new-password" maxlength="4096"><button>Test &amp; Save</button><button type="submit" class="secondary" formaction="/ui/connections/one/test">Test without saving</button></form>
{{if .OneConfigured}}<h2>Discover mailboxes</h2><p>Choose only the Gmail connections you want to import. All choices start unchecked. Outlook/Microsoft through One is not supported.</p><a class="btn secondary" href="/ui/connections/one?discover=1">Refresh connections</a> <a href="https://app.withone.ai/" target="_blank" rel="noopener noreferrer">Open One → Connected Apps</a>
{{if .OneChoices}}<form method="post" action="/ui/connections/one/import"><input type="hidden" name="_csrf" value="{{.CSRF}}"><table><thead><tr><th>Choose</th><th>Mailbox / identity</th><th>Provider</th><th>One status</th><th>MailMoose status</th></tr></thead><tbody>{{range .OneChoices}}<tr><td><input type="checkbox" name="connection_id" value="{{.ID}}" {{if not .Available}}disabled{{end}} aria-label="Import {{.Email}}"></td><td>{{if .Email}}{{.Email}}{{else}}Identity unavailable{{end}}</td><td>{{.Provider}}</td><td>{{.OneStatus}} · {{.Environment}}</td><td>{{.Status}}</td></tr>{{end}}</tbody></table><button>Import selected</button></form>{{end}}{{end}}
<h2>Imported Gmail mailboxes</h2>{{range .OneMailboxes}}<section class="card"><h3><a href="/ui/inboxes/{{.InboxID}}">{{.Email}} · Gmail via One</a></h3><p>Status: {{.Status}}. Index: {{.IndexStatus}}. Last sync: {{.LastSync}}</p>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}<form method="post" action="/ui/inboxes/{{.InboxID}}/sync"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button>Sync now</button></form><form method="post" action="/ui/connections/one/mailbox"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="inbox_id" value="{{.InboxID}}"><label>Polling (minutes, 2–30)</label><input name="poll_minutes" type="number" min="2" max="30" value="{{.PollMinutes}}"><button name="action" value="poll">Save polling</button><button name="action" value="reconnect">Verify &amp; reconnect original connection</button><button class="secondary" name="action" value="disconnect">Disconnect from MailMoose only</button></form></section>{{else}}<p>No imported Gmail mailboxes.</p>{{end}}
{{if .OneConfigured}}<h2>Remove One account</h2><p>Removing these credentials pauses all imported mailboxes. Local metadata and drafts are retained; bodies and attachments still require an upstream connection. Queued sends cannot submit while disconnected. One/Gmail connections are not deleted or revoked.</p><form method="post" action="/ui/connections/one/remove"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label><input type="checkbox" name="confirm" value="yes" required> I understand the consequence for imported mailboxes.</label><button class="secondary">Remove One account</button></form>{{end}}<p><a href="/">Back to inboxes</a></p></section>`

const oneImportBody = `<section class="card"><h1>One import results</h1><p>{{.Notice}}</p><ul>{{range .OneImportResults}}<li>{{.ConnectionID}}: {{if .Error}}{{.Error}}{{else}}Imported — <a href="/ui/inboxes/{{.InboxID}}">open mailbox</a>{{end}}</li>{{end}}</ul><a href="/ui/connections/one?discover=1">Refresh connections and retry failed selections</a></section>`
