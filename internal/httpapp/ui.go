package httpapp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

const pageTemplate = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · Gatehouse Email</title><style>
body{font:15px system-ui,sans-serif;max-width:1180px;margin:0 auto;padding:24px;color:#202124;background:#fafafa}a{color:#1557b0}header{display:flex;justify-content:space-between;align-items:center;margin-bottom:24px}h1,h2,h3{margin:.4em 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px}.card{background:white;border:1px solid #ddd;border-radius:10px;padding:16px;margin-bottom:16px;display:flex;flex-direction:column}.muted{color:#666}input,select,textarea{font:inherit;padding:8px;border:1px solid #bbb;border-radius:6px;box-sizing:border-box}input,select,textarea{width:100%;margin:4px 0 10px}.btn,button{display:inline-block;font:inherit;padding:8px 14px;border:1px solid #111;border-radius:6px;background:#111;color:#fff;text-decoration:none;line-height:1.2;cursor:pointer;box-sizing:border-box}.btn:hover,button:hover{background:#000;border-color:#000}.secondary{background:#fff;color:#111;border-color:#bbb}.btn.secondary:hover,button.secondary:hover{background:#f2f3f5;border-color:#999}.danger{color:#b00020;border-color:#e0a0aa}.btn.danger:hover,button.danger:hover{background:#fdecef;border-color:#c66}.actions{display:flex;gap:8px;align-items:center;justify-content:flex-end;flex-wrap:wrap}.actions form{margin:0}.btn-sm{height:30px;padding:0 10px;font-size:13px;display:inline-flex;align-items:center;justify-content:center}.row .btn-narrow{padding:8px 7px;flex:0 0 auto}.sub{font-size:12px;color:#666;margin-top:2px}.row{display:flex;gap:8px;align-items:center}.row>*{flex:1}.slist{list-style:none;margin:0 0 12px;padding:0;border:1px solid #ddd;border-radius:8px;overflow:hidden}.slist li{display:flex;align-items:center;gap:8px;padding:8px 10px;border-bottom:1px solid #eee}.slist li:last-child{border-bottom:0}.slist .addr{flex:1;font-size:14px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .empty{color:#666;font-size:13px;padding:12px}.slist .icon-btn{flex:0 0 auto}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:8px;border-bottom:1px solid #eee;vertical-align:top}code,pre{background:#f3f3f3;padding:2px 4px;border-radius:4px}pre{padding:12px;white-space:pre-wrap;overflow:auto}.secret{border:1px solid #d5b400;background:#fffbe6;padding:12px;border-radius:8px;word-break:break-all}.secret pre{background:transparent;padding:0;margin:8px 0 0;white-space:pre-wrap;word-break:break-all}.copy-note{font-size:13px;color:#8a6d00;margin-top:8px}.msgbody{white-space:pre-wrap}.pill{display:inline-block;background:#eee;border-radius:999px;padding:2px 7px;font-size:12px}.error{background:#fee;border:1px solid #e99;padding:10px}.ok{background:#efe;border:1px solid #9c9;padding:10px}dialog{border:0;border-radius:10px;padding:20px;max-width:480px;width:92%;box-sizing:border-box}#key-dialog{width:460px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}#key-dialog.key-dialog--wide{width:820px;max-width:calc(100vw - 24px)}#key-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0}dialog::backdrop{background:rgba(0,0,0,.45)}.toolbar{display:flex;gap:12px;align-items:center;margin-bottom:12px}.toolbar a{text-decoration:none}.msghead{display:flex;justify-content:space-between;align-items:flex-start;gap:12px;flex-wrap:wrap}.msghead h1{margin-top:0}.inboxhead{display:flex;align-items:baseline;gap:12px;flex-wrap:wrap}.inboxtitle{margin:0;font-size:1.9em}.inboxaddr{font-size:1em;color:#5f6368;font-weight:400}.inboxbar{display:flex;gap:10px;align-items:center;margin:10px 0 16px}.inboxbar .active{background:#e8eaed;border-color:#999;font-weight:700}.inboxbar .active:hover{background:#dde1e6;border-color:#777}.bulkbar{display:flex;gap:8px;align-items:center;margin-left:auto}.mailheader{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;margin:0 -16px;padding:0 12px 8px;color:#5f6368;font-size:12px;text-transform:uppercase;letter-spacing:.04em;border-bottom:1px solid #e5e5e5}.mailheader>span{text-align:left}.mailheader>span.hcenter{text-align:center}.hcenter{text-align:center}.mailrows{margin:0 -16px -16px}.mailrow{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;border-bottom:1px solid #eee;background:#f2f3f5;padding:0 12px}.mailrow:last-child{border-bottom:0}.mailrow.unread{background:#fff}.mailcheck{display:flex;align-items:center;justify-content:center}.mailcheck input[type=checkbox]{width:15px;height:15px;margin:0}.mailrowlink{grid-column:2 / 7;display:grid;grid-template-columns:22px minmax(150px,220px) 1fr 110px 84px;gap:8px;align-items:center;padding:12px 0;text-decoration:none;color:#5f6368;min-width:0}.mailrow.unread .mailrowlink{color:#202124}.mailrow.unread .mailsender,.mailrow.unread .mailsubject{font-weight:700}.mailsender,.mailsubject,.mailsnippet,.maildate,.mailsize{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.mailsnippet{color:#5f6368;font-weight:400}.maildate,.mailsize{font-size:13px;color:#5f6368;text-align:center}.mailrow.unread .maildate,.mailrow.unread .mailsize{color:#202124}.mailaction{grid-column:7;display:flex;justify-content:center;align-items:center}.mailaction form{margin:0}.mailaction .btn-sm{margin:0 2px}.mailrow.outbox{grid-template-columns:22px minmax(150px,220px) 1fr 110px 150px}.mailrow.outbox .mailrowlink{grid-column:1 / 5;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.outbox .mailaction{grid-column:5;justify-content:flex-end;gap:6px}.mailaction button{width:112px;text-align:center}.maildot{display:inline-block}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#1557b0}.unread-pill{background:#1557b0;color:#fff}.banner{padding:10px 12px;border-radius:8px;margin-bottom:16px;border:1px solid}.banner.warn{background:#fff8e1;border-color:#e6c34a}.mailframe{width:100%;height:520px;border:1px solid #ddd;border-radius:8px;background:#fff}.attachments{list-style:none;padding:0;margin:8px 0}.attachments li{padding:4px 0}.brand{color:#202124;text-decoration:none}.card-footer{display:flex;justify-content:flex-end;align-items:center;gap:12px;margin-top:auto;padding-top:12px}.card-footer .small{margin-right:auto}.small{font-size:13px}.icon-btn{height:30px;padding:0 7px;line-height:1;display:inline-flex;align-items:center;justify-content:center}.icon-btn svg{width:14px;height:14px;display:block}.dialog-actions{display:flex;justify-content:flex-end;align-items:center;gap:8px;margin-top:16px}.dialog-actions form{margin:0;margin-right:auto}.email-field{display:flex;align-items:stretch;margin:4px 0 10px}.email-field input,.email-field select{width:auto;margin:0}.email-field input{flex:1;border-radius:6px 0 0 6px}.email-field .at{display:flex;align-items:center;padding:0 8px;color:#666;background:#f2f3f5;border:1px solid #bbb;border-left:0;border-right:0}.email-field select{border-radius:0 6px 6px 0;border-left:0;max-width:50%}.row-link{cursor:pointer}.row-link:hover td{background:#f6f9ff}.key-fields[data-type=api]>label:first-child{display:flex;align-items:center;gap:8px;margin:4px 0 10px}.key-fields[data-type=api]>label:first-child input{width:auto;margin:0}.key-matrix{width:100%;margin:6px 0}.key-matrix th,.key-matrix td{padding:6px 8px;border-bottom:1px solid #eee;vertical-align:middle}.key-matrix td:last-child,.key-matrix th:last-child{text-align:right}.key-matrix th:last-child{white-space:nowrap}.seg{position:relative;display:inline-flex;border:1px solid #bbb;border-radius:7px;overflow:hidden;background:#fff}.seg input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0}.seg label{display:inline-block;min-width:92px;text-align:center;box-sizing:border-box;margin:0;padding:5px 11px;font-size:13px;line-height:1.2;color:#333;cursor:pointer;user-select:none;border-left:1px solid #ddd}.seg label:first-of-type{border-left:0}.seg input:checked+label{background:#111;color:#fff}.seg input:focus-visible+label{outline:2px solid #1557b0;outline-offset:-2px}.seg input:disabled+label{cursor:not-allowed}.seg button{border:0;border-left:1px solid #ddd;border-radius:0;background:#fff;color:#333;min-width:92px;text-align:center;box-sizing:border-box;font-weight:700;padding:5px 11px;font-size:13px;line-height:1.2}.seg button:first-of-type{border-left:0}.seg button:hover{background:#f2f3f5;color:#333}.seg button:disabled{color:#999}.role-legend{width:100%;margin-top:12px;font-size:13px}.role-legend th,.role-legend td{padding:5px 8px;border-bottom:1px solid #eee;text-align:left}.role-legend td:first-child{white-space:nowrap;font-weight:600}.amber{background:#fff8e1;color:#8a6d00;border-color:#e6c34a}button.amber:hover{background:#fdf0c8;border-color:#c9a52f}#key-rotate{margin-right:auto}button:disabled{opacity:.4;cursor:not-allowed}button:disabled:hover{background:#111;border-color:#111}</style></head><body><header><div><a class="brand" href="/"><b>Gatehouse Email</b></a>{{if .Account}} <span class="muted">· {{.Account.Name}}</span>{{end}}</div>{{if .Principal.UserID}}<form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary">Log Out</button></form>{{end}}</header>{{template "body" .}}</body></html>`

func (s *Server) render(w http.ResponseWriter, body string, data any) {
	t, err := template.New("page").Funcs(template.FuncMap{"bytes": formatBytes, "join": strings.Join, "snippet": snippetText, "mailDate": mailDate, "filesize": filesize, "asset": s.assetURL}).Parse(pageTemplate + `{{define "body"}}` + body + `{{end}}`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err = t.Execute(w, data); err != nil {
		s.Log.Error("render", "error", err)
	}
}

type pageData struct {
	Title                       string
	Principal                   model.Principal
	CSRF                        string
	Account                     model.Account
	Domains                     []model.Domain
	Inboxes                     []model.Inbox
	Messages                    []model.Message
	Credentials                 []credentialView
	Outbound                    []outboundView
	OutboundProviders           []outboundProviderView
	OutboundDetail              *outboundDetailView
	DeliveryAttempts            []store.DeliveryAttempt
	DeliveryHasMore             bool
	DeliveryBefore              int64
	Message                     *model.Message
	Attachments                 []model.Attachment
	Notice, SecretLabel, Secret string
	HasUsers                    bool

	Inbox          *model.Inbox
	InboxAddr      map[string]string
	Unread         map[string]int
	UnreadCount    int
	DraftCount     int
	OutboxCount    int
	HasMore        bool
	Before         string
	Folder         string
	BasePath       string
	ThreadMessages []model.Message
	OutboundReady  bool

	ComposeTitle, ComposeAction, ComposeCancel string
	ComposeTo, ComposeCC, ComposeBCC           string
	ComposeSubject, ComposeText, ComposeNote   string
	ComposeError, ComposeFlash                 string
	ComposeDraftID                             string

	Drafts []model.Draft

	Email string

	BootstrapRequired bool
}
type outboundView struct {
	ID, Name, Provider, ConfigJSON string
	Active                         bool
}
type outboundDetailView struct {
	ID, Name, Provider, ConfigJSON string
	Active                         bool
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}
type outboundProviderView struct {
	Name, Description string
	Fields            []transport.ConfigField
}
type credentialView struct {
	ID, Kind, Name, Type, Scope, RolesJSON, InboxID string
	Admin                                           bool
}

// secretFlash carries a one-time secret from the POST that created it to the
// dashboard GET that displays it, so refreshing cannot create it again.
type secretFlash struct {
	Notice, Label, Secret string
}

func inboxAddrMap(boxes []model.Inbox) map[string]string {
	m := make(map[string]string, len(boxes))
	for _, b := range boxes {
		m[b.ID] = b.Address
	}
	return m
}

// mergeBlockedMessages folds metadata-only blocked-message records into the
// admin Recent messages list as synthetic Messages, newest first.
func mergeBlockedMessages(msgs []model.Message, blocked []model.BlockedMessage, limit int) []model.Message {
	for _, b := range blocked {
		msgs = append(msgs, model.Message{
			ID:         b.ID,
			InboxID:    b.InboxID,
			Direction:  "inbound",
			From:       b.From,
			To:         b.To,
			Subject:    b.Subject,
			SizeBytes:  b.SizeBytes,
			ReceivedAt: b.ReceivedAt,
			CreatedAt:  b.CreatedAt,
			Blocked:    true,
		})
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.After(msgs[j].CreatedAt) })
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[:limit]
	}
	return msgs
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	has, err := s.Service.Store.HasUsers(r.Context())
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if !has {
		http.Redirect(w, r, "/setup", 303)
		return
	}
	if _, err = r.Cookie("ghm_session"); err == nil {
		http.Redirect(w, r, "/dashboard", 303)
		return
	}
	http.Redirect(w, r, "/login", 303)
}

const authBody = `<div class="card" style="max-width:460px;margin:60px auto"><h1>{{.Title}}</h1>{{if .Notice}}<div class="error">{{.Notice}}</div>{{end}}<form method="post"><input type="hidden" name="_csrf" value="{{.CSRF}}">{{if eq .Title "Set Up Gatehouse Email"}}<label>Account name</label><input name="account" required placeholder="My Inbox">{{if .BootstrapRequired}}<label>Bootstrap token</label><input type="password" name="bootstrap_token" required placeholder="One-time setup token">{{end}}{{end}}<label>Email</label><input type="email" name="email" required value="{{.Email}}"><label>Password</label><input type="password" name="password" minlength="10" required><button>{{.Title}}</button></form></div>`

type authFlash struct {
	Title, Error, Email string
}

// renderAuth shows an auth page, restoring any error and email left by a
// redirect from a failed POST (Post/Redirect/Get).
func (s *Server) renderAuth(w http.ResponseWriter, r *http.Request, title string) {
	data := pageData{Title: title, CSRF: s.setPreAuthCSRF(w, r), BootstrapRequired: s.Service.Config.AdminBootstrapToken != ""}
	if v, ok := s.flashes.take(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(authFlash); ok {
			data.Title, data.Notice, data.Email = f.Title, f.Error, f.Email
		}
	}
	s.render(w, authBody, data)
}

// flashAuth stores an auth error and redirects back to the form.
func (s *Server) flashAuth(w http.ResponseWriter, r *http.Request, dest, title, msg, email string) {
	if tok := s.flashes.put(authFlash{Title: title, Error: msg, Email: email}, len(title)+len(msg)+len(email)+32); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	has, _ := s.Service.Store.HasUsers(r.Context())
	if has {
		http.Redirect(w, r, "/login", 303)
		return
	}
	s.renderAuth(w, r, "Set Up Gatehouse Email")
}
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	has, _ := s.Service.Store.HasUsers(r.Context())
	if has {
		http.Error(w, "setup complete", 403)
		return
	}
	// If a one-time bootstrap token is configured, the first Admin must present
	// it so an arbitrary first internet visitor cannot claim the instance.
	if tok := s.Service.Config.AdminBootstrapToken; tok != "" {
		got := strings.TrimSpace(r.Form.Get("bootstrap_token"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
			s.flashAuth(w, r, "/setup", "Set Up Gatehouse Email", "invalid bootstrap token", r.Form.Get("email"))
			return
		}
	}
	_ = r.ParseForm()
	u, err := s.Service.Store.CreateInitialAdmin(r.Context(), r.Form.Get("account"), r.Form.Get("email"), r.Form.Get("password"), s.Service.Config.DefaultQuotaBytes)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			http.Error(w, "setup complete", 403)
			return
		}
		s.flashAuth(w, r, "/setup", "Set Up Gatehouse Email", err.Error(), r.Form.Get("email"))
		return
	}
	tok, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/dashboard", 303)
}
func (s *Server) registerGet(w http.ResponseWriter, r *http.Request) {
	if !s.Service.Config.AllowRegistration {
		http.Error(w, "registration is closed", 403)
		return
	}
	s.renderAuth(w, r, "Create Account")
}
func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	if !s.Service.Config.AllowRegistration {
		http.Error(w, "registration is closed", 403)
		return
	}
	_ = r.ParseForm()
	name := r.Form.Get("account")
	if name == "" {
		name = strings.Split(r.Form.Get("email"), "@")[0]
	}
	u, err := s.Service.Store.CreateAccountAndAdmin(r.Context(), name, r.Form.Get("email"), r.Form.Get("password"), s.Service.Config.DefaultQuotaBytes)
	if err != nil {
		s.flashAuth(w, r, "/register", "Create Account", err.Error(), r.Form.Get("email"))
		return
	}
	tok, _, _ := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/dashboard", 303)
}
func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.renderAuth(w, r, "Log In")
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.Service.Config.IsTrustedProxy(r.RemoteAddr))
	if !s.loginLimiter.Allow(ip) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	_ = r.ParseForm()
	u, err := s.Service.Store.AuthenticateUser(r.Context(), r.Form.Get("email"), r.Form.Get("password"))
	if err != nil {
		s.flashAuth(w, r, "/login", "Log In", "Invalid email or password", r.Form.Get("email"))
		return
	}
	tok, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/dashboard", 303)
}
func (s *Server) logoutPost(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("ghm_session"); err == nil {
		s.Service.Store.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", 303)
}

const dashboardBody = `<h1>Dashboard</h1><p class="muted">{{bytes .Account.StorageUsedBytes}} of {{bytes .Account.StorageQuotaBytes}} stored.</p>{{if .Notice}}<div class="ok">{{.Notice}}</div>{{end}}{{if .Secret}}<div class="secret"><b>{{.SecretLabel}}</b><pre>{{.Secret}}</pre></div>{{end}}
<div class="grid"><section class="card"><h2>Inboxes</h2>{{if .Inboxes}}<table><thead><tr><th>Name</th><th></th><th>Email</th><th></th></tr></thead><tbody>{{range .Inboxes}}<tr class="row-link" data-href="/ui/inboxes/{{.ID}}"><td><a href="/ui/inboxes/{{.ID}}">{{if .DisplayName}}{{.DisplayName}}{{else}}<span class="muted">—</span>{{end}}</a>{{if .AllowedSenders}} <span title="Only allowed senders can email this inbox" aria-label="Restricted to allowed senders" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="7" width="9" height="6.5" rx="1.2"/><path d="M5.5 7V5.5a2.5 2.5 0 0 1 5 0V7"/></svg></span>{{end}}</td><td>{{if index $.Unread .ID}}<span class="pill unread-pill">{{index $.Unread .ID}}</span>{{end}}</td><td>{{.Address}}</td><td class="actions"><button type="button" class="secondary icon-btn edit-inbox" data-id="{{.ID}}" data-name="{{.DisplayName}}" data-address="{{.Address}}" data-allowed="{{join .AllowedSenders ","}}" title="Edit inbox" aria-label="Edit"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button><form method="post" action="/ui/inboxes/{{.ID}}/delete" data-confirm="Delete this inbox and all of its messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No inboxes yet.</p>{{end}}<div class="card-footer"><button type="button" id="add-inbox">Add Inbox</button></div></section>
<section class="card"><h2>Clients</h2>{{if .Credentials}}<table><thead><tr><th>Name</th><th>Type</th><th>Scope</th><th></th></tr></thead><tbody>{{range .Credentials}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{.Scope}}</td><td class="actions"><button type="button" class="secondary icon-btn edit-credential" data-id="{{.ID}}" data-kind="{{.Kind}}" data-name="{{.Name}}" data-admin="{{if .Admin}}1{{end}}" data-roles="{{.RolesJSON}}" data-inbox="{{.InboxID}}" title="Edit" aria-label="Edit"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button><form method="post" action="/ui/{{if eq .Kind "hermes"}}hermes{{else}}keys{{end}}/{{.ID}}/delete" data-confirm="Delete this {{.Type}}?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No keys yet.</p>{{end}}<div class="card-footer"><button type="button" id="add-key">Create Key</button></div></section></div>
<div class="grid"><section class="card"><h2>Domains</h2>{{if .Domains}}<table><thead><tr><th>Domain</th><th>Catch-all</th><th></th></tr></thead><tbody>{{range .Domains}}<tr><td><b>{{.Name}}</b></td><td class="muted">{{if .CatchAllInboxID}}{{index $.InboxAddr .CatchAllInboxID}}{{else}}—{{end}}</td><td class="actions"><button type="button" class="secondary icon-btn edit-domain" data-id="{{.ID}}" data-name="{{.Name}}" data-catchall="{{.CatchAllInboxID}}" title="Edit domain" aria-label="Edit domain"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">Add your receiving domain.</p>{{end}}<div class="card-footer"><button type="button" id="add-domain">Add Domain</button></div></section>
<section class="card"><h2>Outbound Providers</h2>{{if .Outbound}}<table><thead><tr><th>Name</th><th>Status</th><th></th></tr></thead><tbody>{{range .Outbound}}<tr class="row-link" data-href="/ui/outbound/{{.ID}}"><td><a href="/ui/outbound/{{.ID}}"><b>{{.Name}}</b></a><div class="sub">{{.Provider}}</div></td><td>{{if .Active}}<span class="pill">Active</span>{{else}}<span class="muted">Inactive</span>{{end}}</td><td class="actions">{{if not .Active}}<form method="post" action="/ui/outbound/{{.ID}}/active"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Set Active</button></form>{{end}}<button type="button" class="secondary icon-btn edit-provider" data-id="{{.ID}}" data-name="{{.Name}}" data-provider="{{.Provider}}" data-config="{{.ConfigJSON}}" title="Edit" aria-label="Edit"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button><form method="post" action="/ui/outbound/{{.ID}}/delete" data-confirm="Delete this outbound provider?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No outbound provider configured.</p>{{end}}<div class="card-footer"><span class="muted small">The active provider is used for all sending.</span><button type="button" id="add-provider">Add Outbound Provider</button></div></section></div>
<dialog id="provider-dialog"><form method="post" action="/ui/outbound"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Name</label><input name="name" placeholder="Defaults to provider"><label>Provider</label><select name="provider" id="provider-select">{{range .OutboundProviders}}<option value="{{.Name}}">{{.Description}}</option>{{end}}</select>{{range $p := .OutboundProviders}}<fieldset class="provider-fields" data-provider="{{$p.Name}}" style="border:0;padding:0;margin:0">{{range $f := $p.Fields}}<label>{{$f.Label}}{{if $f.Required}} *{{end}}</label>{{if $f.Options}}<select name="cfg_{{$p.Name}}_{{$f.Name}}">{{range $f.Options}}<option value="{{.Value}}"{{if eq .Value $f.Default}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<input type="{{$f.Type}}" name="cfg_{{$p.Name}}_{{$f.Name}}" value="{{$f.Default}}" placeholder="{{$f.Placeholder}}"{{if $f.Required}} required{{end}}>{{end}}{{end}}</fieldset>{{end}}<div class="dialog-actions"><button type="button" class="secondary" id="provider-cancel">Cancel</button><button>Save Provider</button></div></form></dialog>
<dialog id="key-dialog"><form method="post" action="/ui/keys" id="key-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Type</label><select name="type" id="key-type"><option value="api">API key</option><option value="hermes">Hermes relay connection</option></select><label>Name</label><input name="name" placeholder="Hermes EA" required><fieldset class="key-fields" data-type="api" style="border:0;padding:0;margin:0"><label><input type="checkbox" name="admin" value="1"> Account Admin Key (Full permission on all mailboxes and can create and delete mailboxes)</label><fieldset id="key-matrix" style="border:0;padding:0;margin:0">{{if .Inboxes}}<table class="key-matrix"><thead><tr><th>Inbox</th><th><span class="muted">Set all:</span> <div class="seg"><button type="button" data-set-role="">None</button><button type="button" data-set-role="read">Read</button><button type="button" data-set-role="assistant">Assistant</button><button type="button" data-set-role="owner">Owner</button></div></th></tr></thead><tbody>{{range .Inboxes}}<tr><td>{{.Address}}</td><td><div class="seg"><input type="radio" id="role_{{.ID}}_none" name="role_{{.ID}}" value="" checked><label for="role_{{.ID}}_none">None</label><input type="radio" id="role_{{.ID}}_read" name="role_{{.ID}}" value="read"><label for="role_{{.ID}}_read">Read</label><input type="radio" id="role_{{.ID}}_assistant" name="role_{{.ID}}" value="assistant"><label for="role_{{.ID}}_assistant">Assistant</label><input type="radio" id="role_{{.ID}}_owner" name="role_{{.ID}}" value="owner"><label for="role_{{.ID}}_owner">Owner</label></div></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">Create an inbox first to grant mailbox access.</p>{{end}}<table class="role-legend"><thead><tr><th>Role</th><th>Grants</th></tr></thead><tbody><tr><td>Read</td><td>Read messages/threads, search, download attachments</td></tr><tr><td>Assistant</td><td>Read + delete messages</td></tr><tr><td>Owner</td><td>Assistant + send/reply and mailbox settings</td></tr></tbody></table></fieldset></fieldset><fieldset class="key-fields" data-type="hermes" style="border:0;padding:0;margin:0"><label>Inbox</label><select name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .AllowedSenders}}1{{end}}">{{.Address}}</option>{{end}}</select><div class="banner" id="key-hermes-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> The Hermes agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating a Hermes relay connection to this mailbox.</div><label id="key-hermes-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-hermes-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label></fieldset><div class="error" id="key-error" hidden></div><div class="dialog-actions"><button type="button" class="amber" id="key-rotate" hidden>Rotate Key</button><button type="button" class="secondary" id="key-cancel">Cancel</button><button id="key-submit">Create</button></div></form><div id="key-result" hidden><h3 id="key-result-title"></h3><p class="muted" id="key-result-label"></p><div class="secret"><pre id="key-result-secret"></pre></div><p class="copy-note" id="key-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the key above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="key-copy">Copy</button><button type="button" id="key-done">Done</button></div></div></dialog>
<dialog id="domain-dialog"><h3 id="domain-dialog-title">Domain</h3><form method="post" id="domain-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Catch-all inbox</label><select name="inbox" id="domain-catchall"><option value="">No catch-all</option>{{range .Inboxes}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select></form><div class="dialog-actions"><form method="post" id="domain-delete-form" data-confirm="Delete this domain and ALL of its inboxes and messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Delete Domain</button></form><button type="button" class="secondary" id="domain-cancel">Cancel</button><button type="submit" form="domain-form">Save</button></div></dialog>
<dialog id="add-domain-dialog"><form method="post" action="/ui/domains"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><input name="name" placeholder="example.com" required><div class="dialog-actions"><button type="button" class="secondary" id="add-domain-cancel">Cancel</button><button>Add Domain</button></div></form></dialog>
<dialog id="inbox-dialog"><form method="post" action="/ui/inboxes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Email address</label><div class="email-field"><input name="local" placeholder="hermes" required><span class="at">@</span><select name="domain" required>{{range .Domains}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></div><label>Display Name</label><input name="display" placeholder="Hermes"><div class="dialog-actions"><button type="button" class="secondary" id="inbox-cancel">Cancel</button><button>Create Inbox</button></div></form></dialog>
<dialog id="inbox-edit-dialog"><form method="post" id="inbox-edit-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Display name</label><input name="display"><label>Email address</label><input id="inbox-edit-address" value="" disabled><label>Allowed senders</label><p class="muted" id="inbox-sender-note">Anyone can email this inbox. Add an allowed sender to restrict who can email it.</p><ul id="inbox-sender-list" class="slist"><li class="empty">Anyone can email this inbox. Add an address below to restrict who can email it.</li></ul><div class="row"><input id="inbox-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-sender-add">Add</button></div><p class="muted small" id="inbox-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains.</p></form><div class="dialog-actions"><form method="post" id="inbox-edit-delete-form" data-confirm="Delete this inbox and all of its messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Delete Inbox</button></form><button type="button" class="secondary" id="inbox-edit-cancel">Cancel</button><button type="submit" form="inbox-edit-form">Save</button></div></dialog>
<script src="{{asset "app.js"}}" defer></script>
<section class="card"><h2>Recent messages</h2><form method="get" action="/dashboard" class="row"><input name="q" value="" placeholder="Search mail"><button>Search</button></form>{{if .Messages}}<table><thead><tr><th>When</th><th>Direction</th><th>From</th><th>To</th><th>Subject</th><th></th></tr></thead><tbody>{{range .Messages}}<tr><td>{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if .Blocked}}<span style="color:#b8860b;font-weight:700">Blocked</span>{{else if eq .Direction "outbound"}}Sent{{else}}Received{{end}}</td><td>{{.From.Address}}</td><td>{{join .To ", "}}</td><td>{{.Subject}}</td><td>{{if .Blocked}}<span class="pill amber">Blocked</span>{{else}}<a href="/ui/messages/{{.ID}}">Open</a>{{end}}</td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No messages yet.</p>{{end}}</section>`

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin UI requires Admin", 403)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	domains, _ := s.Service.Store.ListDomains(r.Context(), p.AccountID)
	boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
	keys, _ := s.Service.Store.ListAPIKeys(r.Context(), p.AccountID)
	creds, _ := s.Service.Store.ListOutboundCredentials(r.Context(), p.AccountID)
	conns, _ := s.Service.Store.ListHermesConnections(r.Context(), p.AccountID)
	var msgs []model.Message
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		msgs, _ = s.Service.Store.SearchMessages(r.Context(), p, q, "", 100)
	} else {
		msgs, _ = s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{Limit: 100})
		blocked, _ := s.Service.Store.ListBlockedMessages(r.Context(), p, 100)
		msgs = mergeBlockedMessages(msgs, blocked, 100)
	}
	ov := s.outboundViews(creds, acc.ActiveOutboundCredentialID)
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	if unread == nil {
		unread = map[string]int{}
	}
	notice, secretLabel, secret := r.URL.Query().Get("notice"), "", ""
	if v, ok := s.flashes.take(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(secretFlash); ok {
			notice, secretLabel, secret = f.Notice, f.Label, f.Secret
		}
	}
	s.render(w, dashboardBody, pageData{Title: "Dashboard", Principal: p, CSRF: csrf(r), Account: acc, Domains: domains, Inboxes: boxes, Messages: msgs, Credentials: credentialViews(keys, conns), Outbound: ov, OutboundProviders: outboundProviderViews(), Unread: unread, InboxAddr: inboxAddrMap(boxes), Notice: notice, SecretLabel: secretLabel, Secret: secret})
}

func (s *Server) uiCreateDomain(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if _, err := s.Service.Store.CreateDomain(r.Context(), p.AccountID, r.Form.Get("name")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Domain+created", 303)
}
func (s *Server) uiCatchAll(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.SetDomainCatchAll(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("inbox")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Catch-all+updated", 303)
}
func (s *Server) uiDeleteDomain(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	paths, err := s.Service.Store.PurgeDomain(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, path := range paths {
		_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
	}
	http.Redirect(w, r, "/dashboard?notice=Domain+deleted", 303)
}
func (s *Server) uiCreateInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if _, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, r.Form.Get("domain"), r.Form.Get("local"), r.Form.Get("display")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Inbox+created", 303)
}

// normalizeAllowedSenders trims, lowercases and validates allowed-sender
// patterns. Empty entries are dropped; an empty result means "allow all".
func normalizeAllowedSenders(raw []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, entry := range raw {
		value, err := model.NormalizeAllowedSender(entry)
		if err != nil {
			return nil, err
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) > 500 {
			return nil, fmt.Errorf("too many allowed senders")
		}
	}
	return out, nil
}

// parseAllowedSenders reads the repeated `allowed` form fields.
func parseAllowedSenders(r *http.Request) ([]string, error) {
	return normalizeAllowedSenders(r.Form["allowed"])
}

func (s *Server) uiUpdateInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	id := r.PathValue("id")
	if _, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id); err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	senders, err := parseAllowedSenders(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.Service.Store.SetInboxDisplayName(r.Context(), p.AccountID, id, r.Form.Get("display")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err = s.Service.Store.SetInboxAllowedSenders(r.Context(), p.AccountID, id, senders); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Inbox+updated", 303)
}

func (s *Server) uiDeleteInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	paths, err := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, path := range paths {
		_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
	}
	http.Redirect(w, r, "/dashboard?notice=Inbox+deleted", 303)
}

func (s *Server) uiCreateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	notice, label, secret := "", "", ""
	if r.Form.Get("type") == "hermes" {
		inboxID := r.Form.Get("inbox")
		if box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inboxID); err == nil && len(box.AllowedSenders) == 0 && r.Form.Get("ack") != "1" {
			http.Error(w, "confirm the no-allow-list risk before creating a Hermes relay connection to this inbox", 400)
			return
		}
		gatewayID, gwSecret, deliveryKey, err := s.Service.CreateHermesRelay(r.Context(), p, inboxID, r.Form.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		notice, label, secret = "Hermes relay connection created", "Paste these lines into the gateway .env", hermesEnvBlock(s.Service.Config.BaseURL, gatewayID, gwSecret, deliveryKey)
	} else {
		boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
		roles := map[string]string{}
		for _, b := range boxes {
			if role := r.Form.Get("role_" + b.ID); role != "" {
				roles[b.ID] = role
			}
		}
		_, plain, err := s.Service.Store.CreateAPIKey(r.Context(), p.AccountID, r.Form.Get("name"), r.Form.Get("admin") == "1", roles)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		notice, label, secret = "API key created", "Copy this API key now", plain
	}
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]string{"notice": notice, "label": label, "secret": secret})
		return
	}
	s.flashSecret(w, r, notice, label, secret)
}

// wantsJSON reports whether the caller asked for a JSON response, so the
// Create Key dialog can receive the one-time secret inline without a redirect.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// flashSecret stores a one-time secret and redirects to the dashboard, which
// consumes it (Post/Redirect/Get). A refresh then shows a plain dashboard.
func (s *Server) flashSecret(w http.ResponseWriter, r *http.Request, notice, label, secret string) {
	dest := "/dashboard"
	if tok := s.flashes.put(secretFlash{Notice: notice, Label: label, Secret: secret}, len(notice)+len(label)+len(secret)+64); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) uiUpdateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
	roles := map[string]string{}
	for _, b := range boxes {
		if role := r.Form.Get("role_" + b.ID); role != "" {
			roles[b.ID] = role
		}
	}
	if err := s.Service.Store.UpdateAPIKey(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name"), r.Form.Get("admin") == "1", roles); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Key+updated", 303)
}

func (s *Server) uiRotateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	plain, err := s.Service.Store.RotateAPIKey(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]string{"notice": "API key rotated", "label": "Copy this API key now", "secret": plain})
		return
	}
	s.flashSecret(w, r, "API key rotated", "Copy this API key now", plain)
}

func (s *Server) uiDeleteKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.RevokeAPIKey(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Key+deleted", 303)
}

func (s *Server) uiUpdateHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.UpdateHermesConnectionName(r.Context(), p.AccountID, r.PathValue("id"), r.Form.Get("name")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Connection+updated", 303)
}

func (s *Server) uiDeleteHermes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteHermesConnection(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Connection+deleted", 303)
}

func (s *Server) uiOutbound(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	id := strings.TrimSpace(r.Form.Get("id"))
	provider := strings.ToLower(strings.TrimSpace(r.Form.Get("provider")))
	t, ok := transport.LookupOutbound(provider)
	if !ok {
		http.Error(w, "unknown provider", 400)
		return
	}
	var existing store.OutboundCredential
	sameProvider := false
	if id != "" {
		if e, err := s.Service.Store.GetOutboundCredential(r.Context(), p.AccountID, id); err == nil {
			existing = e
			sameProvider = e.Provider == provider
		}
	}
	cfg, err := outboundConfigFromForm(t, r, id == "" || !sameProvider)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if sameProvider {
		if old, err := s.Service.DecryptOutboundCredential(existing); err == nil {
			if fp, ok := t.(transport.ConfigSchemaProvider); ok {
				for _, f := range fp.ConfigFields() {
					if !f.Secret {
						continue
					}
					if _, present := cfg[f.Name]; !present {
						if v, ok := old[f.Name]; ok {
							cfg[f.Name] = v
						}
					}
				}
			}
		}
	}
	saved, err := s.Service.SaveOutboundCredential(r.Context(), p.AccountID, id, r.Form.Get("name"), provider, cfg)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if id == "" {
		if acc, err := s.Service.Store.GetAccount(r.Context(), p.AccountID); err == nil && acc.ActiveOutboundCredentialID == "" {
			_ = s.Service.Store.SetActiveOutboundCredential(r.Context(), p.AccountID, saved.ID)
		}
	}
	if rt := strings.TrimSpace(r.Form.Get("return_to")); strings.HasPrefix(rt, "/ui/outbound/") {
		http.Redirect(w, r, rt, 303)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Outbound+provider+saved", 303)
}

func (s *Server) uiOutboundActive(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.SetActiveOutboundCredential(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Active+provider+updated", 303)
}

const outboundDetailBody = `<div class="toolbar"><a class="btn secondary" href="/dashboard">← Dashboard</a></div>
<section class="card"><div style="display:flex;align-items:flex-start;justify-content:space-between;gap:12px"><div><h1>{{.OutboundDetail.Name}} <span class="sub">{{.OutboundDetail.Provider}}</span>{{if .OutboundDetail.Active}} <span class="pill">Active</span>{{end}}</h1><p class="muted">Created {{.OutboundDetail.CreatedAt.Format "2006-01-02 15:04"}} · Updated {{.OutboundDetail.UpdatedAt.Format "2006-01-02 15:04"}}</p></div><button type="button" class="edit-provider" data-id="{{.OutboundDetail.ID}}" data-name="{{.OutboundDetail.Name}}" data-provider="{{.OutboundDetail.Provider}}" data-config="{{.OutboundDetail.ConfigJSON}}" title="Provider settings" aria-label="Provider settings">Settings</button></div></section>
<section class="card"><h2>Delivery activity</h2>{{if .DeliveryAttempts}}<table><thead><tr><th>When</th><th>Address</th><th>Message</th><th>Attempt</th><th>Status</th><th>Provider ID</th><th>Error</th></tr></thead><tbody>{{range .DeliveryAttempts}}<tr><td style="white-space:nowrap">{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if .MessageID}}<div>to: {{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</div><div>from: {{if .FromAddress}}{{.FromAddress}}{{else}}<span class="muted">—</span>{{end}}</div>{{else}}<span class="muted">message_deleted</span>{{end}}</td><td>{{if .MessageID}}<a href="/ui/messages/{{.MessageID}}">{{.MessageID}}</a>{{else}}<span class="muted">—</span>{{end}}</td><td>{{.Attempt}}</td><td>{{if eq .Status "sent"}}<span class="pill">Sent</span>{{else}}<span class="pill danger">Failed</span>{{end}}</td><td class="muted" style="font-size:60%;word-break:break-all">{{.ProviderMessageID}}</td><td class="muted">{{.ErrorText}}</td></tr>{{end}}</tbody></table>{{if .DeliveryHasMore}}<p><a href="/ui/outbound/{{.OutboundDetail.ID}}?before={{.DeliveryBefore}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No delivery attempts yet.</p>{{end}}</section>
<dialog id="provider-dialog"><form method="post" action="/ui/outbound"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><input type="hidden" name="return_to" value="/ui/outbound/{{.OutboundDetail.ID}}"><label>Name</label><input name="name" placeholder="Defaults to provider"><label>Provider</label><select name="provider" id="provider-select">{{range .OutboundProviders}}<option value="{{.Name}}">{{.Description}}</option>{{end}}</select>{{range $p := .OutboundProviders}}<fieldset class="provider-fields" data-provider="{{$p.Name}}" style="border:0;padding:0;margin:0">{{range $f := $p.Fields}}<label>{{$f.Label}}{{if $f.Required}} *{{end}}</label>{{if $f.Options}}<select name="cfg_{{$p.Name}}_{{$f.Name}}">{{range $f.Options}}<option value="{{.Value}}"{{if eq .Value $f.Default}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<input type="{{$f.Type}}" name="cfg_{{$p.Name}}_{{$f.Name}}" value="{{$f.Default}}" placeholder="{{$f.Placeholder}}"{{if $f.Required}} required{{end}}>{{end}}{{end}}</fieldset>{{end}}<div class="dialog-actions"><button type="button" class="secondary" id="provider-cancel">Cancel</button><button>Save Provider</button></div></form></dialog>
<script src="{{asset "app.js"}}" defer></script>`

func (s *Server) uiOutboundDetail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	cred, err := s.Service.Store.GetOutboundCredential(r.Context(), p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "outbound provider not found", 404)
		return
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	before := int64(0)
	if v := r.URL.Query().Get("before"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	attempts, err := s.Service.Store.ListDeliveryAttempts(r.Context(), p.AccountID, cred.ID, 50, before)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	hasMore := len(attempts) > 50
	if hasMore {
		attempts = attempts[:50]
	}
	nextBefore := int64(0)
	if len(attempts) > 0 {
		nextBefore = attempts[len(attempts)-1].ID
	}
	detail := &outboundDetailView{ID: cred.ID, Name: cred.Name, Provider: cred.Provider, Active: cred.ID == acc.ActiveOutboundCredentialID, CreatedAt: cred.CreatedAt, UpdatedAt: cred.UpdatedAt}
	if t, ok := transport.LookupOutbound(cred.Provider); ok {
		if fp, ok := t.(transport.ConfigSchemaProvider); ok {
			if cfg, err := s.Service.DecryptOutboundCredential(cred); err == nil {
				public := map[string]any{}
				for _, f := range fp.ConfigFields() {
					if f.Secret {
						continue
					}
					if val, ok := cfg[f.Name]; ok {
						public[f.Name] = val
					}
				}
				if b, err := json.Marshal(public); err == nil {
					detail.ConfigJSON = string(b)
				}
			}
		}
	}
	s.render(w, outboundDetailBody, pageData{
		Title:             cred.Name + " · Outbound Provider",
		Principal:         p,
		CSRF:              csrf(r),
		Account:           acc,
		OutboundDetail:    detail,
		OutboundProviders: outboundProviderViews(),
		DeliveryAttempts:  attempts,
		DeliveryHasMore:   hasMore,
		DeliveryBefore:    nextBefore,
	})
}

func (s *Server) uiOutboundDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteOutboundCredential(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/dashboard?notice=Outbound+provider+deleted", 303)
}

func outboundConfigFromForm(t transport.OutboundTransport, r *http.Request, requireSecrets bool) (map[string]any, error) {
	fp, ok := t.(transport.ConfigSchemaProvider)
	if !ok {
		cfg := map[string]any{}
		if err := json.Unmarshal([]byte(r.Form.Get("config")), &cfg); err != nil {
			return nil, fmt.Errorf("invalid configuration JSON")
		}
		return cfg, nil
	}
	cfg := map[string]any{}
	for _, f := range fp.ConfigFields() {
		raw := strings.TrimSpace(r.Form.Get("cfg_" + t.Name() + "_" + f.Name))
		if raw == "" {
			if f.Required && (requireSecrets || !f.Secret) {
				return nil, fmt.Errorf("%s is required", f.Label)
			}
			continue
		}
		if f.Type == "number" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, fmt.Errorf("%s must be a number", f.Label)
			}
			cfg[f.Name] = n
			continue
		}
		cfg[f.Name] = raw
	}
	return cfg, nil
}

func (s *Server) outboundViews(creds []store.OutboundCredential, activeID string) []outboundView {
	out := []outboundView{}
	for _, c := range creds {
		v := outboundView{ID: c.ID, Name: c.Name, Provider: c.Provider, Active: c.ID == activeID}
		if t, ok := transport.LookupOutbound(c.Provider); ok {
			if fp, ok := t.(transport.ConfigSchemaProvider); ok {
				if cfg, err := s.Service.DecryptOutboundCredential(c); err == nil {
					public := map[string]any{}
					for _, f := range fp.ConfigFields() {
						if f.Secret {
							continue
						}
						if val, ok := cfg[f.Name]; ok {
							public[f.Name] = val
						}
					}
					if b, err := json.Marshal(public); err == nil {
						v.ConfigJSON = string(b)
					}
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func outboundProviderViews() []outboundProviderView {
	out := []outboundProviderView{}
	for _, t := range transport.ListOutbound() {
		v := outboundProviderView{Name: t.Name(), Description: t.Description()}
		if fp, ok := t.(transport.ConfigSchemaProvider); ok {
			v.Fields = fp.ConfigFields()
		}
		out = append(out, v)
	}
	return out
}
func credentialViews(keys []model.APIKey, conns []store.HermesConnection) []credentialView {
	out := make([]credentialView, 0, len(keys)+len(conns))
	for _, k := range keys {
		v := credentialView{ID: k.ID, Kind: "api", Name: k.Name, Type: "API key", Scope: apiKeyScope(k), Admin: k.Admin}
		if len(k.Roles) > 0 {
			if b, err := json.Marshal(k.Roles); err == nil {
				v.RolesJSON = string(b)
			}
		}
		out = append(out, v)
	}
	for _, h := range conns {
		out = append(out, credentialView{ID: h.ID, Kind: "hermes", Name: h.Name, Type: "Hermes relay", Scope: "Owner", InboxID: h.InboxID})
	}
	return out
}

func apiKeyScope(k model.APIKey) string {
	if k.Admin {
		return "Admin"
	}
	if len(k.Roles) == 0 {
		return "None"
	}
	seen := map[string]bool{}
	roles := make([]string, 0, len(k.Roles))
	for _, role := range k.Roles {
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	sort.Slice(roles, func(i, j int) bool { return roleRank(roles[i]) < roleRank(roles[j]) })
	for i, role := range roles {
		roles[i] = titleRole(role)
	}
	return strings.Join(roles, ", ")
}

func roleRank(role string) int {
	switch role {
	case "owner":
		return 0
	case "assistant":
		return 1
	case "read":
		return 2
	default:
		return 3
	}
}

func titleRole(role string) string {
	if role == "" {
		return role
	}
	return strings.ToUpper(role[:1]) + role[1:]
}

func hermesEnvBlock(baseURL, gatewayID, secret, deliveryKey string) string {
	return fmt.Sprintf("GATEWAY_RELAY_URL=%s\nGATEWAY_RELAY_ID=%s\nGATEWAY_RELAY_SECRET=%s\nGATEWAY_RELAY_DELIVERY_KEY=%s\nGATEWAY_RELAY_PLATFORMS=email\nGATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true",
		baseURL, gatewayID, secret, deliveryKey)
}

const messageBody = `<div class="toolbar"><a href="/ui/inboxes/{{.Message.InboxID}}{{if eq .Message.Direction "outbound"}}/sent{{end}}">← {{if eq .Message.Direction "outbound"}}Sent{{else}}Inbox{{end}}</a><a href="/dashboard">Dashboard</a></div>
<section class="card"><div class="msghead"><h1>{{if .Message.Subject}}{{.Message.Subject}}{{else}}(no subject){{end}}</h1><div class="actions"><a class="btn secondary btn-sm" href="/ui/messages/{{.Message.ID}}/reply">Reply</a><a class="btn secondary btn-sm" href="/ui/messages/{{.Message.ID}}/forward">Forward</a><form method="post" action="/ui/messages/{{.Message.ID}}/read"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="read" value="0"><button class="secondary btn-sm">Mark unread</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/delete" data-confirm="Delete this message permanently?"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary btn-sm danger">Delete</button></form></div></div>
<p class="muted"><b>From:</b> {{if .Message.From.Name}}{{.Message.From.Name}} &lt;{{.Message.From.Address}}&gt;{{else}}{{.Message.From.Address}}{{end}}<br><b>To:</b> {{join .Message.To ", "}}{{if .Message.CC}}<br><b>Cc:</b> {{join .Message.CC ", "}}{{end}}<br><b>Date:</b> {{.Message.CreatedAt.Format "2006-01-02 15:04"}}{{if .Inbox}} · <b>Mailbox:</b> {{.Inbox.Address}}{{end}}</p>
{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li><a href="/ui/attachments/{{.ID}}">{{.Filename}}</a> <span class="muted">· {{bytes .Size}}</span></li>{{end}}</ul>{{end}}
<hr>{{if .Message.HTML}}<iframe class="mailframe" sandbox="allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" src="/ui/messages/{{.Message.ID}}/html"></iframe>{{else}}<div class="msgbody">{{.Message.Text}}</div>{{end}}
{{if and .Message.HTML .Message.Text}}<details><summary>Plain text</summary><div class="msgbody">{{.Message.Text}}</div></details>{{end}}</section>
{{if gt (len .ThreadMessages) 1}}<section class="card"><h3>Conversation ({{len .ThreadMessages}})</h3><table>{{range .ThreadMessages}}<tr><td class="muted">{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if eq .Direction "outbound"}}To: {{join .To ", "}}{{else}}{{.From.Address}}{{end}}</td><td>{{if eq .ID $.Message.ID}}<b>{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</b>{{else}}<a href="/ui/messages/{{.ID}}">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</a>{{end}}</td></tr>{{end}}</table></section>{{end}}`

func (s *Server) uiMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "message not found", 404)
		return
	}
	if !m.Read {
		read := true
		if err = s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &read, nil); err == nil {
			m.Read = true
		}
	}
	atts, _ := s.Service.Store.ListAttachments(r.Context(), p, m.ID)
	var box *model.Inbox
	if b, e := s.Service.Store.GetInbox(r.Context(), p, m.InboxID); e == nil {
		box = &b
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	var thread []model.Message
	if m.ThreadID != "" {
		thread, _ = s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{InboxID: m.InboxID, ThreadID: m.ThreadID, Limit: 200})
		for i, j := 0, len(thread)-1; i < j; i, j = i+1, j-1 {
			thread[i], thread[j] = thread[j], thread[i]
		}
	}
	title := m.Subject
	if title == "" {
		title = "(no subject)"
	}
	s.render(w, messageBody, pageData{Title: title, Principal: p, CSRF: csrf(r), Message: &m, Attachments: atts, Inbox: box, ThreadMessages: thread, OutboundReady: acc.ActiveOutboundCredentialID != ""})
}

func snippetText(v string, n int) string {
	v = strings.Join(strings.Fields(v), " ")
	r := []rune(v)
	if len(r) <= n {
		return v
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func mailDate(t time.Time) string {
	return t.Format("15:04 2-Jan-06")
}

func filesize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < len(units)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}
