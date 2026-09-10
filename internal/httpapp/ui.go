package httpapp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

const pageTemplate = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · Gatehouse Email</title><style>
body{font:15px system-ui,sans-serif;max-width:1180px;margin:0 auto;padding:24px;color:#202124;background:#fafafa}a{color:#1557b0}header{display:flex;flex-direction:column;align-items:flex-start;gap:8px;margin-bottom:16px}.brandrow{display:flex;align-items:center;gap:8px}.menu{display:flex;align-items:center;gap:16px;flex-wrap:wrap;width:100%}.menu-left,.menu-right{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.menu-right{margin-left:auto}.menu form{margin:0}.quota{font-size:13px;color:#666;white-space:nowrap;padding:0 4px}h1,h2,h3{margin:.4em 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,300px),1fr));gap:16px}.provider-picker{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:4px 0 12px}.provider-picker select{margin:0;flex:1;min-width:180px;max-width:320px}.cfg-form .dialog-actions{margin-top:12px}.cfg-summary{display:grid;grid-template-columns:minmax(90px,max-content) 1fr;gap:2px 12px;margin:8px 0;font-size:13px}.cfg-summary dt{color:#666}.cfg-summary dd{margin:0;overflow-wrap:anywhere;min-width:0}.actions-left{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:8px 0}.actions-left form{margin:0}.wrap{overflow-wrap:anywhere;word-break:break-all}.table-wrap{overflow-x:auto}.card{background:white;border:1px solid #ddd;border-radius:10px;padding:16px;margin-bottom:16px;display:flex;flex-direction:column}.muted{color:#666}input,select,textarea{font:inherit;padding:8px;border:1px solid #bbb;border-radius:6px;box-sizing:border-box}input,select,textarea{width:100%;margin:4px 0 10px}.btn,button{display:inline-block;font:inherit;padding:8px 14px;border:1px solid #111;border-radius:6px;background:#111;color:#fff;text-decoration:none;line-height:1.2;cursor:pointer;box-sizing:border-box}.btn:hover,button:hover{background:#000;border-color:#000}.secondary{background:#fff;color:#111;border-color:#bbb}.btn.secondary:hover,button.secondary:hover{background:#f2f3f5;border-color:#999}.danger{color:#b00020;border-color:#e0a0aa}.btn.danger:hover,button.danger:hover{background:#fdecef;border-color:#c66}.actions{display:flex;gap:8px;align-items:center;justify-content:flex-end;flex-wrap:wrap}.actions form{margin:0}.btn-sm{height:30px;padding:0 10px;font-size:13px;display:inline-flex;align-items:center;justify-content:center}.row .btn-narrow{padding:8px 7px;flex:0 0 auto}.sub{font-size:12px;color:#666;margin-top:2px}.row{display:flex;gap:8px;align-items:center}.row>*{flex:1}.slist{list-style:none;margin:0 0 12px;padding:0;border:1px solid #ddd;border-radius:8px;overflow:hidden}.slist li{display:flex;align-items:center;gap:8px;padding:8px 10px;border-bottom:1px solid #eee}.slist li:last-child{border-bottom:0}.slist .addr{flex:1;font-size:14px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.slist .empty{color:#666;font-size:13px;padding:12px}.slist .icon-btn{flex:0 0 auto}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:8px;border-bottom:1px solid #eee;vertical-align:top}code,pre{background:#f3f3f3;padding:2px 4px;border-radius:4px}pre{padding:12px;white-space:pre-wrap;overflow:auto}.secret{border:1px solid #d5b400;background:#fffbe6;padding:12px;border-radius:8px;word-break:break-all}.secret pre{background:transparent;padding:0;margin:8px 0 0;white-space:pre-wrap;word-break:break-all}.copy-note{font-size:13px;color:#8a6d00;margin-top:8px}.msgbody{white-space:pre-wrap}.pill{display:inline-block;background:#eee;border-radius:999px;padding:2px 7px;font-size:12px}.log-table{font-size:12px}.log-detail{font-size:10px;word-break:break-all}.error{background:#fee;border:1px solid #e99;padding:10px}.ok{background:#efe;border:1px solid #9c9;padding:10px}dialog{border:0;border-radius:10px;padding:20px;max-width:480px;width:92%;box-sizing:border-box}#key-dialog{width:460px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}#key-dialog.key-dialog--wide{width:820px;max-width:calc(100vw - 24px)}#key-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0}dialog::backdrop{background:rgba(0,0,0,.45)}.toolbar{display:flex;gap:12px;align-items:center;margin-bottom:12px}.toolbar a{text-decoration:none}.msghead{display:flex;justify-content:space-between;align-items:flex-start;gap:12px;flex-wrap:wrap}.msghead h1{margin-top:0}.inboxhead{display:flex;align-items:baseline;gap:12px;flex-wrap:wrap}.inboxtitle{margin:0;font-size:1.9em}.inboxaddr{font-size:1em;color:#5f6368;font-weight:400}.inboxbar{display:flex;gap:10px;align-items:center;margin:10px 0 16px}.inboxbar .active{background:#e8eaed;border-color:#999;font-weight:700}.inboxbar .active:hover{background:#dde1e6;border-color:#777}.bulkbar{display:flex;gap:8px;align-items:center;margin-left:auto}.mailheader{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;margin:0 -16px;padding:0 12px 8px;color:#5f6368;font-size:12px;text-transform:uppercase;letter-spacing:.04em;border-bottom:1px solid #e5e5e5}.mailheader>span{text-align:left}.mailheader>span.hcenter{text-align:center}.hcenter{text-align:center}.mailrows{margin:0 -16px -16px}.mailrow{display:grid;grid-template-columns:28px 22px minmax(150px,220px) 1fr 110px 84px 130px;gap:8px;align-items:center;border-bottom:1px solid #eee;background:#f2f3f5;padding:0 12px}.mailrow:last-child{border-bottom:0}.mailrow.unread{background:#fff}.mailcheck{display:flex;align-items:center;justify-content:center}.mailcheck input[type=checkbox]{width:15px;height:15px;margin:0}.mailrowlink{grid-column:2 / 7;display:grid;grid-template-columns:22px minmax(150px,220px) 1fr 110px 84px;gap:8px;align-items:center;padding:12px 0;text-decoration:none;color:#5f6368;min-width:0}.mailrow.unread .mailrowlink{color:#202124}.mailrow.unread .mailsender,.mailrow.unread .mailsubject{font-weight:700}.mailsender,.mailsubject,.mailsnippet,.maildate,.mailsize{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.mailsnippet{color:#5f6368;font-weight:400}.maildate,.mailsize{font-size:13px;color:#5f6368;text-align:center}.mailrow.unread .maildate,.mailrow.unread .mailsize{color:#202124}.mailaction{grid-column:7;display:flex;justify-content:center;align-items:center}.mailaction form{margin:0}.mailaction .btn-sm{margin:0 2px}.mailrow.outbox{grid-template-columns:22px minmax(150px,220px) 1fr 110px 150px}.mailrow.outbox .mailrowlink{grid-column:1 / 5;grid-template-columns:22px minmax(150px,220px) 1fr 110px}.mailrow.outbox .mailaction{grid-column:5;justify-content:flex-end;gap:6px}.mailaction button{width:112px;text-align:center}.maildot{display:inline-block}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#1557b0}.unread-pill{background:#1557b0;color:#fff}.banner{padding:10px 12px;border-radius:8px;margin-bottom:16px;border:1px solid}.banner.warn{background:#fff8e1;border-color:#e6c34a}.issue-dot{display:inline-flex;align-items:center;justify-content:center;width:16px;height:16px;border-radius:50%;background:#fce8e6;border:1.5px solid #d93025;vertical-align:middle;margin-left:6px}.issue-dot svg{width:9px;height:9px;display:block}.mailframe{width:100%;height:520px;border:1px solid #ddd;border-radius:8px;background:#fff}.attachments{list-style:none;padding:0;margin:8px 0}.attachments li{padding:4px 0}.brand{color:#202124;text-decoration:none}.card-head{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-bottom:8px}.card-head h2{margin:0}.small{font-size:13px}.icon-btn{height:30px;padding:0 7px;line-height:1;display:inline-flex;align-items:center;justify-content:center}.icon-btn svg{width:14px;height:14px;display:block}.dialog-actions{display:flex;justify-content:flex-end;align-items:center;gap:8px;margin-top:16px}.dialog-actions>form{margin:0;margin-right:auto}.dialog-danger{display:flex;gap:8px;align-items:center;margin-right:auto}.email-field{display:flex;align-items:stretch;margin:4px 0 10px}.email-field input,.email-field select{width:auto;margin:0}.email-field input{flex:1;border-radius:6px 0 0 6px}.email-field .at{display:flex;align-items:center;padding:0 8px;color:#666;background:#f2f3f5;border:1px solid #bbb;border-left:0;border-right:0}.email-field select{border-radius:0 6px 6px 0;border-left:0;max-width:50%}.row-link{cursor:pointer}.row-link:hover td{background:#f6f9ff}.key-fields[data-type=api]>label:first-child{display:flex;align-items:center;gap:8px;margin:4px 0 10px}.key-fields[data-type=api]>label:first-child input{width:auto;margin:0}.key-matrix{width:100%;margin:6px 0}.key-matrix th,.key-matrix td{padding:6px 8px;border-bottom:1px solid #eee;vertical-align:middle}.key-matrix td:last-child,.key-matrix th:last-child{text-align:right}.key-matrix th:last-child{white-space:nowrap}.seg{position:relative;display:inline-flex;border:1px solid #bbb;border-radius:7px;overflow:hidden;background:#fff}.seg input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0}.seg label{display:inline-block;min-width:92px;text-align:center;box-sizing:border-box;margin:0;padding:5px 11px;font-size:13px;line-height:1.2;color:#333;cursor:pointer;user-select:none;border-left:1px solid #ddd}.seg label:first-of-type{border-left:0}.seg input:checked+label{background:#111;color:#fff}.seg input:focus-visible+label{outline:2px solid #1557b0;outline-offset:-2px}.seg input:disabled+label{cursor:not-allowed}.seg button{border:0;border-left:1px solid #ddd;border-radius:0;background:#fff;color:#333;min-width:92px;text-align:center;box-sizing:border-box;font-weight:700;padding:5px 11px;font-size:13px;line-height:1.2}.seg button:first-of-type{border-left:0}.seg button:hover{background:#f2f3f5;color:#333}.seg button:disabled{color:#999}.role-legend{width:100%;margin-top:12px;font-size:13px}.role-legend th,.role-legend td{padding:5px 8px;border-bottom:1px solid #eee;text-align:left}.role-legend td:first-child{white-space:nowrap;font-weight:600}.amber{background:#fff8e1;color:#8a6d00;border-color:#e6c34a}button.amber:hover{background:#fdf0c8;border-color:#c9a52f}#key-rotate{margin-right:auto}button:disabled{opacity:.4;cursor:not-allowed}button:disabled:hover{background:#111;border-color:#111}.notice{position:fixed;top:16px;left:50%;transform:translateX(-50%);z-index:100;max-width:min(92vw,560px);box-shadow:0 6px 20px rgba(0,0,0,.15);cursor:pointer;animation:notice-in .2s ease-out}.notice.dismissing{animation:notice-out .3s ease-in forwards}@keyframes notice-in{from{opacity:0;transform:translate(-50%,-8px)}to{opacity:1;transform:translate(-50%,0)}}@keyframes notice-out{to{opacity:0;transform:translate(-50%,-8px)}}@media (prefers-reduced-motion:reduce){.notice{animation:none}.notice.dismissing{animation:none;opacity:0}}.tab{padding:10px 18px;text-decoration:none;color:#111;background:#fff;border:1px solid #bbb;border-radius:6px;font-size:15px;font-weight:600;line-height:1.2;cursor:pointer}.tab:hover{background:#f2f3f5;border-color:#999;color:#111}.tab.active{background:#111;border-color:#111;color:#fff}.tab.active:hover{background:#000;border-color:#000;color:#fff}.steps{margin:8px 0 16px;padding-left:20px}.steps li{margin:6px 0}.cf-code{max-height:420px;overflow:auto}.domains-table th,.domains-table td{padding:10px 18px}.domains-table th:first-child,.domains-table td:first-child{padding-left:8px}.cell-edit{min-width:150px;text-align:center}.cell-edit .muted{color:#666}.cell-link{background:none;border:0;padding:0;margin:0;font-size:13px;color:#1557b0;cursor:pointer;text-align:left}.cell-link:hover{background:none;border:0;text-decoration:underline}.provider-select{max-width:320px}.provider-hint{margin:0}.provider-box{border:1px solid #ddd;border-radius:8px;padding:12px 12px 4px;margin-top:8px;background:#fafafa}.domain-dialog{width:640px;max-width:calc(100vw - 24px);max-height:90vh;overflow:auto}.domain-dialog .dialog-actions{position:sticky;bottom:0;background:#fff;border-top:1px solid #eee;padding:12px 0;margin-top:8px}.attach-drop{border:2px dashed #bbb;border-radius:8px;padding:16px;margin:4px 0 10px;background:#fafafa;text-align:center;transition:border-color .15s,background .15s}.attach-drop.dragover{border-color:#1557b0;background:#eef4fd}.attach-hint{margin:0 0 8px;color:#5f6368;font-size:13px}.attach-browse{cursor:pointer}.attach-input{position:absolute;width:1px;height:1px;margin:0;padding:0;border:0;opacity:0;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}.attach-list{list-style:none;margin:10px 0 0;padding:0;text-align:left}.attach-list li{display:flex;align-items:center;gap:10px;padding:6px 8px;border:1px solid #e3e3e3;border-radius:6px;background:#fff;margin-bottom:6px}.attach-list .attach-name{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.attach-list .attach-size{color:#5f6368;font-size:12px;white-space:nowrap}.attach-list .attach-remove{border:0;background:none;color:#5f6368;font-size:18px;line-height:1;padding:0 4px;cursor:pointer}.attach-list .attach-remove:hover{background:none;color:#b00020}.attach-overlay{display:none;position:fixed;inset:0;z-index:200;align-items:center;justify-content:center;background:rgba(21,87,176,.10);border:3px dashed #1557b0;box-sizing:border-box}.attach-overlay.active{display:flex}.attach-overlay-inner{background:#fff;border:1px solid #1557b0;border-radius:10px;padding:16px 28px;font-size:18px;font-weight:600;color:#1557b0;box-shadow:0 8px 24px rgba(0,0,0,.12)}</style></head><body><header><div class="brandrow"><a class="brand" href="/"><b>Gatehouse Email</b></a>{{if .Account.ID}} <span class="muted">· {{.Account.Name}}</span>{{end}}</div>{{if .Principal.UserID}}<nav class="menu"><div class="menu-left">{{if .Principal.Admin}}<a class="tab{{if eq .Tab "home"}} active{{end}}" href="/dashboard">Home</a>{{end}}</div><div class="menu-right">{{if .Account.ID}}<span class="quota">{{bytes .Account.StorageUsedBytes}} of {{bytes .Account.StorageQuotaBytes}} stored.</span>{{end}}<a class="tab{{if eq .Tab "account"}} active{{end}}" href="/account">Account</a><form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="tab">Log Out</button></form></div></nav>{{end}}</header>{{template "body" .}}<script src="{{asset "app.js"}}" defer></script></body></html>`

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
	Tab                         string
	Principal                   model.Principal
	CSRF                        string
	Account                     model.Account
	User                        model.User
	Domains                     []model.Domain
	DomainSendingReady          map[string]bool
	DomainReceivingReady        map[string]bool
	Domain                      *model.Domain
	DomainInboxes               map[string][]model.Inbox
	DomainSendingEditors        map[string][]*domainEditorView
	DomainReceivingEditors      map[string][]*domainEditorView
	DomainSendingSelected       map[string]string
	DomainReceivingSelected     map[string]string
	DomainSendingLabel          map[string]string
	DomainReceivingLabel        map[string]string
	DomainReceivingRegenerate   map[string]bool
	DomainOpenID                string
	DomainOpenKind              string
	DomainWorkerCode            string
	DomainWorkerWebhook         string
	DomainSendingSettingsURL    string
	DomainReceivingSettingsURL  string
	Inboxes                     []model.Inbox
	Messages                    []model.Message
	Credentials                 []credentialView
	LogEntries                  []store.DomainLogEntry
	LogHasMore                  bool
	LogBefore                   string
	Message                     *model.Message
	Attachments                 []model.Attachment
	Notice, SecretLabel, Secret string
	Error                       string
	HasUsers                    bool
	BaseURL                     string

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
	InboundReady   bool

	ComposeTitle, ComposeAction, ComposeCancel string
	ComposeTo, ComposeCC, ComposeBCC           string
	ComposeSubject, ComposeText, ComposeNote   string
	ComposeError, ComposeFlash                 string
	ComposeDraftID                             string
	ComposeRequestSend                         bool

	Drafts []model.Draft

	DraftCounts  map[string]int
	SendRequests []model.DraftSendRequest
	ReviewDraft  *model.Draft

	Email string

	BootstrapRequired bool
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
		if p := principal(r); p.SessionHash != "" {
			s.Service.Hub.CancelScope("sess:" + p.SessionHash)
		}
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", 303)
}

// settingsFlash carries an error or success notice from a settings POST to the
// settings GET (Post/Redirect/Get), so a refresh cannot re-submit the form.
type settingsFlash struct {
	Error, Notice string
}

const settingsBody = `<h1>Account</h1>{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
<div class="grid">
<section class="card"><h2>Change account name</h2><p class="muted">Shown in the header. This is a display name, not your email address.</p><form method="post" action="/ui/account/account"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Account name</label><input name="name" value="{{.Account.Name}}" maxlength="80" required><div class="dialog-actions"><button>Save</button></div></form></section>
<section class="card"><h2>Change email address</h2><p class="muted">Used to log in. Current: {{.User.Email}}</p><form method="post" action="/ui/account/email"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>New email</label><input type="email" name="email" required><label>Current password</label><input type="password" name="current_password" autocomplete="current-password" required><div class="dialog-actions"><button>Update email</button></div></form></section>
<section class="card"><h2>Change password</h2><form method="post" action="/ui/account/password"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Current password</label><input type="password" name="current_password" autocomplete="current-password" required><label>New password</label><input type="password" name="new_password" minlength="10" autocomplete="new-password" required><label>Confirm new password</label><input type="password" name="confirm_password" minlength="10" autocomplete="new-password" required><div class="dialog-actions"><button>Change password</button></div></form></section>
</div>`

// settingsRedirect stores a settings flash and redirects back to the settings
// page (Post/Redirect/Get).
func (s *Server) settingsRedirect(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	dest := "/account"
	if tok := s.flashes.put(settingsFlash{Error: errMsg, Notice: notice}, len(notice)+len(errMsg)+32); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	acc, err := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	if err != nil {
		http.Error(w, "account not found", 404)
		return
	}
	user, err := s.Service.Store.GetUser(r.Context(), p.UserID)
	if err != nil {
		http.Error(w, "user not found", 404)
		return
	}
	data := pageData{Title: "Account", Tab: "account", Principal: p, CSRF: csrf(r), Account: acc, User: user}
	if v, ok := s.flashes.take(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(settingsFlash); ok {
			data.Error, data.Notice = f.Error, f.Notice
		}
	}
	s.render(w, settingsBody, data)
}

func (s *Server) uiSettingsAccount(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.Store.UpdateAccountName(r.Context(), p.AccountID, r.Form.Get("name")); err != nil {
		s.settingsRedirect(w, r, "", err.Error())
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "account.rename", "")
	s.settingsRedirect(w, r, "Account name updated", "")
}

func (s *Server) uiSettingsEmail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ip := clientIP(r, s.Service.Config.IsTrustedProxy(r.RemoteAddr))
	if !s.passwordLimiter.Allow(ip) {
		s.settingsRedirect(w, r, "", "too many attempts, try again later")
		return
	}
	err := s.Service.Store.UpdateUserEmail(r.Context(), p.UserID, p.AccountID, r.Form.Get("email"), r.Form.Get("current_password"))
	if err != nil {
		msg := err.Error()
		switch {
		case errors.Is(err, store.ErrForbidden):
			msg = "Current password is incorrect"
		case errors.Is(err, store.ErrConflict):
			msg = "That email address is already in use"
		}
		s.settingsRedirect(w, r, "", msg)
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.email_change", "")
	s.settingsRedirect(w, r, "Email address updated", "")
}

func (s *Server) uiSettingsPassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ip := clientIP(r, s.Service.Config.IsTrustedProxy(r.RemoteAddr))
	if !s.passwordLimiter.Allow(ip) {
		s.settingsRedirect(w, r, "", "too many attempts, try again later")
		return
	}
	current := r.Form.Get("current_password")
	newPassword := r.Form.Get("new_password")
	if newPassword != r.Form.Get("confirm_password") {
		s.settingsRedirect(w, r, "", "New passwords do not match")
		return
	}
	if newPassword == current {
		s.settingsRedirect(w, r, "", "New password must be different from the current one")
		return
	}
	keep := ""
	if c, err := r.Cookie("ghm_session"); err == nil {
		keep = c.Value
	}
	if err := s.Service.Store.UpdateUserPassword(r.Context(), p.UserID, current, newPassword, keep); err != nil {
		msg := err.Error()
		if errors.Is(err, store.ErrForbidden) {
			msg = "Current password is incorrect"
		}
		s.settingsRedirect(w, r, "", msg)
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.password_change", "")
	// Other devices were logged out in the database; cancel their live streams.
	s.Service.Hub.CancelScope("user:" + p.UserID)
	s.settingsRedirect(w, r, "Password changed. Other devices have been logged out.", "")
}

const dashboardBody = `{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Secret}}<div class="secret"><b>{{.SecretLabel}}</b><pre>{{.Secret}}</pre></div>{{end}}
{{if .DomainWorkerCode}}<section class="card"><h2>Cloudflare Worker code</h2><p class="muted">Paste this into a Cloudflare Worker. It contains the generated shared secret and is shown only once.</p><ol class="steps"><li>In Cloudflare, open <b>Workers &amp; Pages</b> → <b>Create application</b> → <b>Worker</b> → <b>Deploy</b>.</li><li>Open the Worker, choose <b>Edit code</b>, replace the stub with the code below, then <b>Deploy</b>.</li><li>In Email Routing, add a <b>Send to a Worker</b> rule for each receiving address and choose this Worker.</li></ol><pre class="cf-code" id="cf-code">{{.DomainWorkerCode}}</pre><p class="copy-note" id="cf-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the code above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="cf-copy">Copy code</button><a class="btn" href="/dashboard">Done</a></div></section>{{end}}
<div class="tab-panel"{{if ne .Tab "home"}} hidden{{end}}>
<div class="grid"><section class="card"><div class="card-head"><h2>Inboxes</h2><button type="button" id="add-inbox">Add Inbox</button></div>{{if .Inboxes}}<table><thead><tr><th>Name</th><th></th><th></th><th>Email</th><th></th></tr></thead><tbody>{{range .Inboxes}}<tr class="row-link" data-href="/ui/inboxes/{{.ID}}"><td><a href="/ui/inboxes/{{.ID}}">{{if .DisplayName}}{{.DisplayName}}{{else}}<span class="muted">—</span>{{end}}</a>{{$sp := index $.DomainSendingReady .DomainID}}{{$rp := index $.DomainReceivingReady .DomainID}}{{if or (not $sp) (not $rp)}} <span class="issue-dot"{{if and (not $sp) (not $rp)}} title="Sending paused and not receiving — no provider or receive path for this domain" aria-label="Sending paused and not receiving — no provider or receive path for this domain"{{else if not $sp}} title="Sending paused — no provider for this domain. Mail will queue." aria-label="Sending paused — no provider for this domain. Mail will queue."{{else}} title="Not receiving — no receive path is configured for this domain." aria-label="Not receiving — no receive path is configured for this domain."{{end}}><svg viewBox="0 0 10 10" fill="none" stroke="#b3261e" stroke-width="2.2" stroke-linecap="round"><path d="M2 2l6 6M8 2l-6 6"/></svg></span>{{end}}{{if .AllowedSenders}} <span title="Only allowed senders can email this inbox" aria-label="Restricted to allowed senders" style="color:#5f6368;vertical-align:middle"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="7" width="9" height="6.5" rx="1.2"/><path d="M5.5 7V5.5a2.5 2.5 0 0 1 5 0V7"/></svg></span>{{end}}</td><td>{{if index $.Unread .ID}}<span class="pill unread-pill">{{index $.Unread .ID}}</span>{{end}}</td><td>{{if index $.DraftCounts .ID}}<a class="pill" href="/ui/inboxes/{{.ID}}/drafts" title="Unsent drafts needing attention">{{index $.DraftCounts .ID}}</a>{{end}}</td><td>{{.Address}}</td><td class="actions"><button type="button" class="secondary icon-btn edit-inbox" data-id="{{.ID}}" data-name="{{.DisplayName}}" data-address="{{.Address}}" data-allowed="{{join .AllowedSenders ","}}" title="Edit inbox" aria-label="Edit"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button><form method="post" action="/ui/inboxes/{{.ID}}/delete" data-confirm="Delete this inbox and all of its messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No inboxes yet.</p>{{end}}</section>
<section class="card"><div class="card-head"><h2>Clients</h2><button type="button" id="add-key">Add Client</button></div>{{if .Credentials}}<table><thead><tr><th>Name</th><th>Type</th><th></th></tr></thead><tbody>{{range .Credentials}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td class="actions"><button type="button" class="secondary icon-btn edit-credential" data-id="{{.ID}}" data-kind="{{.Kind}}" data-name="{{.Name}}" data-admin="{{if .Admin}}1{{end}}" data-roles="{{.RolesJSON}}" data-inbox="{{.InboxID}}" title="Edit" aria-label="Edit"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.4 2l2.6 2.6L5.6 13l-3.1.5.5-3.1z"/></svg></button><form method="post" action="/ui/{{if eq .Kind "hermes"}}hermes{{else}}keys{{end}}/{{.ID}}/delete" data-confirm="Delete this {{.Type}}?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No clients yet.</p>{{end}}</section></div>
<section class="card"><div class="card-head"><h2>Domains</h2><button type="button" id="add-domain">Add Domain</button></div>{{if .Domains}}<div class="table-wrap"><table class="domains-table"><thead><tr><th>Domain</th><th>Catch-all</th><th>Sending</th><th>Receiving</th><th></th></tr></thead><tbody>{{range .Domains}}{{$d := .}}<tr><td><b>{{.Name}}</b></td><td>{{if .CatchAllInboxID}}<button type="button" class="cell-link open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall" title="Change catch-all inbox">{{index $.InboxAddr .CatchAllInboxID}}</button>{{else}}<button type="button" class="secondary btn-sm cell-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="catchall">Add</button>{{end}}</td><td><button type="button" class="{{if .SendingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="sending">{{if .SendingProvider}}{{index $.DomainSendingLabel .ID}}{{else}}Add{{end}}</button></td><td><button type="button" class="{{if .ReceivingProvider}}secondary{{else}}amber{{end}} btn-sm cell-edit open-domain-dialog" data-domain="{{.ID}}" data-kind="receiving">{{if .ReceivingProvider}}{{index $.DomainReceivingLabel .ID}}{{else}}Add{{end}}</button></td><td class="actions"><a class="btn secondary icon-btn" href="/ui/domains/{{.ID}}/sending/deliveries" title="Activity log" aria-label="Activity log"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="2.5" width="9" height="11" rx="1.5"/><path d="M5.5 5.5h5M5.5 8h5M5.5 10.5h3"/></svg></a><form method="post" action="/ui/domains/{{.ID}}/delete" data-confirm="Delete this domain and ALL of its inboxes and messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary icon-btn danger" title="Delete" aria-label="Delete"><svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/></svg></button></form></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">Add your first domain.</p>{{end}}</section>
<section class="card"><h2>Recent messages</h2><form method="get" action="/dashboard" class="row"><input name="q" value="" placeholder="Search mail"><button>Search</button></form>{{if .Messages}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Direction</th><th>From</th><th>To</th><th>Subject</th><th></th></tr></thead><tbody>{{range .Messages}}<tr><td style="white-space:nowrap">{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if .Blocked}}<span class="pill amber">Blocked</span>{{else if eq .Direction "outbound"}}<span class="pill">Sent</span>{{else}}<span class="pill">Received</span>{{end}}</td><td>{{if .From.Address}}{{.From.Address}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Subject}}{{.Subject}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Blocked}}<span class="muted">—</span>{{else}}<a href="/ui/messages/{{.ID}}">Open</a>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No messages yet.</p>{{end}}</section>
</div>

<dialog id="key-dialog"><form method="post" action="/ui/keys" id="key-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Type</label><select name="type" id="key-type"><option value="api">API key</option><option value="hermes">Hermes relay connection</option></select><label>Name</label><input name="name" placeholder="Hermes EA" required><fieldset class="key-fields" data-type="api" style="border:0;padding:0;margin:0"><label><input type="checkbox" name="admin" value="1"> Account Admin Key (Full permission on all mailboxes and can create and delete mailboxes)</label><fieldset id="key-matrix" style="border:0;padding:0;margin:0">{{if .Inboxes}}<table class="key-matrix"><thead><tr><th>Inbox</th><th><span class="muted">Set all:</span> <div class="seg"><button type="button" data-set-role="">None</button><button type="button" data-set-role="read">Read</button><button type="button" data-set-role="assistant">Assistant</button><button type="button" data-set-role="owner">Owner</button></div></th></tr></thead><tbody>{{range .Inboxes}}<tr><td>{{.Address}}</td><td><div class="seg"><input type="radio" id="role_{{.ID}}_none" name="role_{{.ID}}" value="" checked><label for="role_{{.ID}}_none">None</label><input type="radio" id="role_{{.ID}}_read" name="role_{{.ID}}" value="read"><label for="role_{{.ID}}_read">Read</label><input type="radio" id="role_{{.ID}}_assistant" name="role_{{.ID}}" value="assistant"><label for="role_{{.ID}}_assistant">Assistant</label><input type="radio" id="role_{{.ID}}_owner" name="role_{{.ID}}" value="owner"><label for="role_{{.ID}}_owner">Owner</label></div></td></tr>{{end}}</tbody></table>{{else}}<p class="muted">Create an inbox first to grant mailbox access.</p>{{end}}<table class="role-legend"><thead><tr><th>Role</th><th>Grants</th></tr></thead><tbody><tr><td>Read</td><td>Read messages/threads, search, download attachments</td></tr><tr><td>Assistant</td><td>Read + delete messages</td></tr><tr><td>Owner</td><td>Assistant + send/reply and mailbox settings</td></tr></tbody></table></fieldset></fieldset><fieldset class="key-fields" data-type="hermes" style="border:0;padding:0;margin:0"><label>Inbox</label><select name="inbox">{{range .Inboxes}}<option value="{{.ID}}" data-allowlist="{{if .AllowedSenders}}1{{end}}">{{.Address}}</option>{{end}}</select><div class="banner" id="key-hermes-warning" hidden style="background:#fdecef;border-color:#e0a0aa;color:#b00020"><b>This inbox has no allow list.</b> The Hermes agent will respond to anyone who emails this inbox. We strongly recommend you set an allow list of permitted senders before creating a Hermes relay connection to this mailbox. Click edit next to the mailbox to configure an allow list.</div><label id="key-hermes-ack-row" hidden style="display:flex;align-items:flex-start;gap:8px;margin-top:8px"><input type="checkbox" name="ack" value="1" id="key-hermes-ack" style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span>I understand the risk of my agent responding to anyone who emails it</span></label></fieldset><div class="error" id="key-error" hidden></div><div class="dialog-actions"><button type="button" class="amber" id="key-rotate" hidden>Rotate Key</button><button type="button" class="secondary" id="key-cancel">Cancel</button><button id="key-submit">Add Client</button></div></form><div id="key-result" hidden><h3 id="key-result-title"></h3><p class="muted" id="key-result-label"></p><div class="secret"><pre id="key-result-secret"></pre></div><p class="copy-note" id="key-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the key above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="key-copy">Copy</button><button type="button" id="key-done">Done</button></div></div></dialog>
<dialog id="add-domain-dialog"><form method="post" action="/ui/domains"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><input name="name" placeholder="example.com" required aria-describedby="add-domain-hint"><p class="muted small" id="add-domain-hint">Enter the bare domain (<code>example.com</code>). Add the whole domain even if you only use a few addresses. Configure sending and receiving from the domain's row in the Domains list.</p><div class="dialog-actions"><button type="button" class="secondary" id="add-domain-cancel">Cancel</button><button>Add Domain</button></div></form></dialog>
<dialog id="inbox-dialog"><form method="post" action="/ui/inboxes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Email address</label><div class="email-field"><input name="local" placeholder="hermes" required><span class="at">@</span><select name="domain" required>{{range .Domains}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select></div><label>Display Name</label><input name="display" placeholder="Hermes"><div class="dialog-actions"><button type="button" class="secondary" id="inbox-cancel">Cancel</button><button>Add Inbox</button></div></form></dialog>
<dialog id="inbox-edit-dialog"><form method="post" id="inbox-edit-form"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Display name</label><input name="display"><label>Email address</label><input id="inbox-edit-address" value="" disabled><label>Allowed senders</label><p class="muted" id="inbox-sender-note">Anyone can email this inbox. Add an allowed sender to restrict who can email it.</p><ul id="inbox-sender-list" class="slist"><li class="empty">Anyone can email this inbox. Add an address below to restrict who can email it.</li></ul><div class="row"><input id="inbox-sender-input" type="text" placeholder="someone@example.com"><button type="button" class="secondary btn-narrow" id="inbox-sender-add">Add</button></div><p class="muted small" id="inbox-sender-hint">Use <code>*@example.com</code> to allow any sender at a domain, or <code>*@*.example.com</code> for its subdomains.</p></form><div class="dialog-actions"><form method="post" id="inbox-edit-delete-form" data-confirm="Delete this inbox and all of its messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Delete Inbox</button></form><button type="button" class="secondary" id="inbox-edit-cancel">Cancel</button><button type="submit" form="inbox-edit-form">Save</button></div></dialog>
{{range .Domains}}{{$d := .}}{{$sel := index $.DomainSendingSelected .ID}}<dialog id="domain-sending-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "sending")}} data-open="1"{{end}}><h2>Sending · {{.Name}}</h2><form id="domain-sending-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/sending" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><label>Provider</label><select name="provider" class="provider-select"><option value="">Select a provider…</option>{{range index $.DomainSendingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}} data-next="{{if .Generated}}1{{end}}">{{.ProviderLabel}}</option>{{end}}</select><p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure sending.</p>{{range index $.DomainSendingEditors $d.ID}}{{$e := .}}<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}{{if .KeepSecrets}}<p class="muted">Saving {{.ProviderLabel}} updates this domain's sending configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted">Saving {{.ProviderLabel}} replaces this domain's sending configuration. Required secrets must be entered.</p>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}</div>{{end}}</form><div class="dialog-actions">{{if $d.SendingProvider}}<div class="dialog-danger"><form method="post" action="/ui/domains/{{$d.ID}}/sending/clear" data-confirm="Remove sending configuration for this domain? Mail will queue until a provider is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove sending</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-sending-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div></dialog>
{{end}}{{range .Domains}}{{$d := .}}{{$sel := index $.DomainReceivingSelected .ID}}<dialog id="domain-receiving-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "receiving")}} data-open="1"{{end}}><h2>Receiving · {{.Name}}</h2><form id="domain-receiving-form-{{$d.ID}}" method="post" action="/ui/domains/{{$d.ID}}/receiving" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><label>Provider</label><select name="provider" class="provider-select"><option value="">Select a provider…</option>{{range index $.DomainReceivingEditors $d.ID}}<option value="{{.Provider}}"{{if eq .Provider $sel}} selected{{end}} data-next="{{if .Generated}}1{{end}}">{{.ProviderLabel}}</option>{{end}}</select><p class="muted provider-hint"{{if $sel}} hidden{{end}}>Choose a provider to configure receiving.</p>{{range index $.DomainReceivingEditors $d.ID}}{{$e := .}}<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}{{if .WebhookURL}}{{if not .Generated}}<p class="muted">Register this webhook URL with {{.ProviderLabel}} before saving:</p><div class="secret"><pre class="setup-webhook-url">{{.WebhookURL}}</pre></div><p class="copy-note setup-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the URL above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary setup-copy">Copy webhook URL</button></div>{{end}}{{end}}{{if and .Steps (not .Generated)}}<ol class="steps">{{range .Steps}}<li>{{.}}</li>{{end}}</ol>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}{{if not .Generated}}{{if .KeepSecrets}}<p class="muted small">Saving updates this domain's receiving configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted small">Saving replaces this domain's receiving configuration. Required secrets must be entered.</p>{{end}}{{end}}</div>{{end}}</form><div class="dialog-actions">{{if $d.ReceivingProvider}}<div class="dialog-danger">{{if index $.DomainReceivingRegenerate $d.ID}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/regenerate" data-confirm="Regenerate the Worker secret? The current Worker stops working until you paste the new code."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="amber">Regenerate secret</button></form>{{end}}<form method="post" action="/ui/domains/{{$d.ID}}/receiving/clear" data-confirm="Remove receiving configuration for this domain? It will stop accepting mail until a receive path is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove receiving</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="domain-receiving-form-{{$d.ID}}" data-save-provider{{if not $sel}} disabled{{end}}>Save</button></div></dialog>
{{end}}
{{range .Domains}}{{$d := .}}<dialog id="domain-catchall-dialog-{{.ID}}" class="domain-dialog"{{if and (eq $d.ID $.DomainOpenID) (eq $.DomainOpenKind "catchall")}} data-open="1"{{end}}><h2>Catch-all · {{.Name}}</h2><form method="post" action="/ui/domains/{{.ID}}/catchall"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><p class="muted">Mail sent to an unknown address on this domain is delivered to this inbox. Only inboxes on {{.Name}} can be selected.</p><label>Catch-all inbox</label><select name="inbox"><option value="">No catch-all</option>{{range index $.DomainInboxes $d.ID}}<option value="{{.ID}}"{{if eq .ID $d.CatchAllInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button>Save catch-all</button></div></form></dialog>
{{end}}`

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin UI requires Admin", 403)
		return
	}
	ctx := r.Context()
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	domains, _ := s.Service.Store.ListDomains(ctx, p.AccountID)
	boxes, _ := s.Service.Store.ListInboxes(ctx, p)
	keys, _ := s.Service.Store.ListAPIKeys(ctx, p.AccountID)
	conns, _ := s.Service.Store.ListHermesConnections(ctx, p.AccountID)
	var msgs []model.Message
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		msgs, _ = s.Service.Store.SearchMessages(ctx, p, q, "", 100)
	} else {
		msgs, _ = s.Service.Store.ListMessages(ctx, p, store.MessageFilter{Limit: 100})
		blocked, _ := s.Service.Store.ListBlockedMessages(ctx, p, 100)
		msgs = mergeBlockedMessages(msgs, blocked, 100)
	}
	sendingReady := make(map[string]bool, len(domains))
	receivingReady := make(map[string]bool, len(domains))
	domainInboxes := make(map[string][]model.Inbox, len(domains))
	for _, b := range boxes {
		domainInboxes[b.DomainID] = append(domainInboxes[b.DomainID], b)
	}
	for _, d := range domains {
		sendingReady[d.ID] = d.SendingProvider != ""
		receivingReady[d.ID] = d.ReceivingProvider != ""
	}
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
	if unread == nil {
		unread = map[string]int{}
	}
	draftCounts, _ := s.Service.Store.DraftCountsByInbox(ctx, p)
	if draftCounts == nil {
		draftCounts = map[string]int{}
	}

	// The dialog to open is named in the query so a provider switch or a
	// post-save error can reload the dashboard with the right editor visible. A
	// foreign or stale domain id is dropped rather than trusted.
	openID := strings.TrimSpace(r.URL.Query().Get("domain"))
	openKind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if openID != "" {
		if _, err := s.Service.Store.GetDomain(ctx, p.AccountID, openID); err != nil {
			openID, openKind = "", ""
		}
	}

	sendingEditors := make(map[string][]*domainEditorView, len(domains))
	receivingEditors := make(map[string][]*domainEditorView, len(domains))
	sendingSelected := make(map[string]string, len(domains))
	receivingSelected := make(map[string]string, len(domains))
	sendingLabel := make(map[string]string, len(domains))
	receivingLabel := make(map[string]string, len(domains))
	receivingRegenerate := make(map[string]bool, len(domains))
	for _, d := range domains {
		sendProvider := normalizeDomainProvider(d.SendingProvider)
		if openID == d.ID && openKind == "sending" {
			if qp := normalizeDomainProvider(r.URL.Query().Get("provider")); qp != "" {
				sendProvider = qp
			}
		}
		sendingSelected[d.ID] = sendProvider
		editors := s.domainSendingEditors(ctx, p.AccountID, d.ID)
		for _, e := range editors {
			if e.Provider == sendProvider {
				e.Selected = true
				sendingLabel[d.ID] = e.ProviderLabel
			}
		}
		if sendingLabel[d.ID] == "" && d.SendingProvider != "" {
			sendingLabel[d.ID] = d.SendingProvider
		}
		sendingEditors[d.ID] = editors

		recvProvider := normalizeDomainProvider(d.ReceivingProvider)
		if openID == d.ID && openKind == "receiving" {
			if qp := normalizeDomainProvider(r.URL.Query().Get("provider")); qp != "" {
				recvProvider = qp
			}
		}
		receivingSelected[d.ID] = recvProvider
		recvEditors := s.domainReceivingEditors(ctx, p.AccountID, d.ID)
		for _, e := range recvEditors {
			if e.Provider == recvProvider {
				e.Selected = true
				receivingLabel[d.ID] = e.ProviderLabel
				receivingRegenerate[d.ID] = e.Generated
			}
		}
		if receivingLabel[d.ID] == "" && d.ReceivingProvider != "" {
			receivingLabel[d.ID] = d.ReceivingProvider
		}
		receivingEditors[d.ID] = recvEditors
	}

	notice, secretLabel, secret := r.URL.Query().Get("notice"), "", ""
	workerCode, workerWebhook := "", ""
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if v, ok := s.flashes.peek(tok); ok {
			switch f := v.(type) {
			case secretFlash:
				if taken, ok := s.flashes.take(tok); ok {
					if tf, ok := taken.(secretFlash); ok {
						notice, secretLabel, secret = tf.Notice, tf.Label, tf.Secret
					}
				}
			case domainWorkerFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if rc, err := s.Service.Store.GetDomainReceivingConfig(ctx, p.AccountID, f.DomainID); err == nil && rc.ID == f.ConfigID && rc.Revision == f.Revision {
						if taken, ok := s.flashes.take(tok); ok {
							if tf, ok := taken.(domainWorkerFlash); ok && tf.WorkerCode == f.WorkerCode && tf.ConfigID == f.ConfigID && tf.Revision == f.Revision && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID {
								workerCode, workerWebhook = tf.WorkerCode, tf.WebhookURL
							}
						}
					}
				}
			case domainNoticeFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == openID {
					if taken, ok := s.flashes.take(tok); ok {
						if tf, ok := taken.(domainNoticeFlash); ok && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == f.DomainID && tf.Error == f.Error {
							if tf.Kind == "sending" {
								for _, e := range sendingEditors[tf.DomainID] {
									if e.Provider == tf.Provider {
										e.Error = tf.Error
										overlayValues(e.Values, tf.Values)
										sendingSelected[tf.DomainID] = tf.Provider
									}
								}
							} else if tf.Kind == "receiving" {
								for _, e := range receivingEditors[tf.DomainID] {
									if e.Provider == tf.Provider {
										e.Error = tf.Error
										overlayValues(e.Values, tf.Values)
										receivingSelected[tf.DomainID] = tf.Provider
									}
								}
							}
							openID, openKind = tf.DomainID, tf.Kind
						}
					}
				}
			}
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	s.render(w, dashboardBody, pageData{Title: "Dashboard", Tab: "home", Principal: p, CSRF: csrf(r), Account: acc, BaseURL: s.Service.Config.BaseURL, Domains: domains, DomainSendingReady: sendingReady, DomainReceivingReady: receivingReady, DomainInboxes: domainInboxes, DomainSendingEditors: sendingEditors, DomainReceivingEditors: receivingEditors, DomainSendingSelected: sendingSelected, DomainReceivingSelected: receivingSelected, DomainSendingLabel: sendingLabel, DomainReceivingLabel: receivingLabel, DomainReceivingRegenerate: receivingRegenerate, DomainOpenID: openID, DomainOpenKind: openKind, DomainWorkerCode: workerCode, DomainWorkerWebhook: workerWebhook, Inboxes: boxes, Messages: msgs, Credentials: credentialViews(keys, conns), Unread: unread, DraftCounts: draftCounts, InboxAddr: inboxAddrMap(boxes), Notice: notice, SecretLabel: secretLabel, Secret: secret})
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
		notice, label, secret = "API key created", "Copy this API key now — you will only be able to see this key now, it will not be shown again.", plain
	}
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 201, map[string]string{"notice": notice, "label": label, "secret": secret})
		return
	}
	s.flashSecret(w, r, notice, label, secret)
}

// wantsJSON reports whether the caller asked for a JSON response, so the
// Add Client dialog can receive the one-time secret inline without a redirect.
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
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
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
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	if wantsJSON(r) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]string{"notice": "API key rotated", "label": "Copy this API key now — you will only be able to see this key now, it will not be shown again.", "secret": plain})
		return
	}
	s.flashSecret(w, r, "API key rotated", "Copy this API key now — you will only be able to see this key now, it will not be shown again.", plain)
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
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
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
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	http.Redirect(w, r, "/dashboard?notice=Connection+deleted", 303)
}

// cloudflareWorkerCode renders the embedded Worker template with the given
// base URL and generated shared secret.
func (s *Server) cloudflareWorkerCode(baseURL, secret string) string {
	code := string(cloudflareWorkerTemplate)
	code = strings.ReplaceAll(code, "__GATEHOUSE_WEBHOOK_URL__", strings.TrimRight(baseURL, "/")+"/internal/ingest/cloudflare")
	code = strings.ReplaceAll(code, "__GATEHOUSE_WEBHOOK_SECRET__", secret)
	return code
}

// credentialViews combines API keys and Hermes relay connections into the
// dashboard "Clients" list.
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

const messageBody = `<div class="toolbar"><a href="/ui/inboxes/{{.Message.InboxID}}{{if eq .Message.Direction "outbound"}}/sent{{end}}">← {{if eq .Message.Direction "outbound"}}Sent{{else}}Inbox{{end}}</a></div>
{{if not .OutboundReady}}<div class="banner warn">Sending is paused until a provider is configured for this domain. Mail will queue. <a href="{{.DomainSendingSettingsURL}}">Add one</a>.</div>{{end}}
{{if not .InboundReady}}<div class="banner warn">Not receiving — no receive path is configured for this domain. <a href="{{.DomainReceivingSettingsURL}}">Add one</a>.</div>{{end}}
<section class="card"><div class="msghead"><h1>{{if .Message.Subject}}{{.Message.Subject}}{{else}}(no subject){{end}}</h1><div class="actions"><a class="btn secondary btn-sm" href="/ui/messages/{{.Message.ID}}/reply">Reply</a><a class="btn secondary btn-sm" href="/ui/messages/{{.Message.ID}}/forward">Forward</a><form method="post" action="/ui/messages/{{.Message.ID}}/read"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="read" value="0"><button class="secondary btn-sm">Mark unread</button></form><form method="post" action="/ui/messages/{{.Message.ID}}/delete" data-confirm="Delete this message permanently?"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary btn-sm danger">Delete</button></form></div></div>
<p class="muted"><b>From:</b> {{if .Message.From.Name}}{{.Message.From.Name}} &lt;{{.Message.From.Address}}&gt;{{else}}{{.Message.From.Address}}{{end}}<br><b>To:</b> {{join .Message.To ", "}}{{if .Message.CC}}<br><b>Cc:</b> {{join .Message.CC ", "}}{{end}}<br><b>Date:</b> {{.Message.CreatedAt.Format "2006-01-02 15:04"}}{{if .Inbox}} · <b>Mailbox:</b> {{.Inbox.Address}}{{end}}</p>
{{if .Attachments}}<h3>Attachments</h3><ul class="attachments">{{range .Attachments}}<li><a href="/ui/attachments/{{.ID}}">{{.Filename}}</a> <span class="muted">· {{bytes .Size}}</span></li>{{end}}</ul>{{end}}
<hr>{{if .Message.HTML}}<iframe class="mailframe" sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox" referrerpolicy="no-referrer" loading="lazy" src="/ui/messages/{{.Message.ID}}/html"></iframe>{{else}}<div class="msgbody">{{.Message.Text}}</div>{{end}}
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
	outboundReady := box == nil
	inboundReady := box == nil
	domainSendingURL, domainReceivingURL := "/dashboard", "/dashboard"
	if box != nil {
		domainSendingURL = "/dashboard?domain=" + url.PathEscape(box.DomainID) + "&kind=sending"
		domainReceivingURL = "/dashboard?domain=" + url.PathEscape(box.DomainID) + "&kind=receiving"
		_, outErr := s.Service.Store.GetDomainSendingConfig(r.Context(), p.AccountID, box.DomainID)
		outboundReady = outErr == nil
		_, inErr := s.Service.Store.GetDomainReceivingConfig(r.Context(), p.AccountID, box.DomainID)
		inboundReady = inErr == nil
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	s.render(w, messageBody, pageData{Title: title, Principal: p, CSRF: csrf(r), Account: acc, Message: &m, Attachments: atts, Inbox: box, ThreadMessages: thread, OutboundReady: outboundReady, InboundReady: inboundReady, DomainSendingSettingsURL: domainSendingURL, DomainReceivingSettingsURL: domainReceivingURL})
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
