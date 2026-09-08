package httpapp

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

const pageTemplate = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}} · Open Agent Inbox</title><style>
body{font:15px system-ui,sans-serif;max-width:1180px;margin:0 auto;padding:24px;color:#202124;background:#fafafa}a{color:#1557b0}header{display:flex;justify-content:space-between;align-items:center;margin-bottom:24px}h1,h2,h3{margin:.4em 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px}.card{background:white;border:1px solid #ddd;border-radius:10px;padding:16px;margin-bottom:16px}.muted{color:#666}input,select,textarea,button{font:inherit;padding:8px;border:1px solid #bbb;border-radius:6px;box-sizing:border-box}input,select,textarea{width:100%;margin:4px 0 10px}button{cursor:pointer;background:#111;color:white;border-color:#111}.secondary{background:white;color:#111}.row{display:flex;gap:8px;align-items:center}.row>*{flex:1}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:8px;border-bottom:1px solid #eee;vertical-align:top}code,pre{background:#f3f3f3;padding:2px 4px;border-radius:4px}pre{padding:12px;white-space:pre-wrap;overflow:auto}.secret{border:1px solid #d5b400;background:#fffbe6;padding:12px;border-radius:8px;word-break:break-all}.msgbody{white-space:pre-wrap}.pill{display:inline-block;background:#eee;border-radius:999px;padding:2px 7px;font-size:12px}.error{background:#fee;border:1px solid #e99;padding:10px}.ok{background:#efe;border:1px solid #9c9;padding:10px}dialog{border:0;border-radius:10px;padding:20px;max-width:480px;width:92%}dialog::backdrop{background:rgba(0,0,0,.45)}</style></head><body><header><div><b>Open Agent Inbox</b>{{if .Account}} <span class="muted">· {{.Account.Name}}</span>{{end}}</div>{{if .Principal.UserID}}<form method="post" action="/logout"><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary">Log out</button></form>{{end}}</header>{{template "body" .}}</body></html>`

func (s *Server) render(w http.ResponseWriter, body string, data any) {
	t, err := template.New("page").Funcs(template.FuncMap{"bytes": formatBytes, "join": strings.Join}).Parse(pageTemplate + `{{define "body"}}` + body + `{{end}}`)
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
	Message                     *model.Message
	Attachments                 []model.Attachment
	Notice, SecretLabel, Secret string
	HasUsers                    bool
}
type outboundView struct {
	ID, Name, Provider, ConfigJSON string
	Active                         bool
}
type outboundProviderView struct {
	Name, Description string
	Fields            []transport.ConfigField
}
type credentialView struct {
	Name, Type, Scope string
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
	if _, err = r.Cookie("oai_session"); err == nil {
		http.Redirect(w, r, "/dashboard", 303)
		return
	}
	http.Redirect(w, r, "/login", 303)
}

const authBody = `<div class="card" style="max-width:460px;margin:60px auto"><h1>{{.Title}}</h1>{{if .Notice}}<div class="error">{{.Notice}}</div>{{end}}<form method="post"><input type="hidden" name="_csrf" value="{{.CSRF}}">{{if eq .Title "Set up Open Agent Inbox"}}<label>Account name</label><input name="account" required placeholder="My Inbox">{{end}}<label>Email</label><input type="email" name="email" required><label>Password</label><input type="password" name="password" minlength="10" required><button>{{.Title}}</button></form></div>`

func (s *Server) setupGet(w http.ResponseWriter, r *http.Request) {
	has, _ := s.Service.Store.HasUsers(r.Context())
	if has {
		http.Redirect(w, r, "/login", 303)
		return
	}
	s.render(w, authBody, pageData{Title: "Set up Open Agent Inbox", CSRF: s.setPreAuthCSRF(w, r)})
}
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	has, _ := s.Service.Store.HasUsers(r.Context())
	if has {
		http.Error(w, "setup complete", 403)
		return
	}
	_ = r.ParseForm()
	u, err := s.Service.Store.CreateAccountAndAdmin(r.Context(), r.Form.Get("account"), r.Form.Get("email"), r.Form.Get("password"), s.Service.Config.DefaultQuotaBytes)
	if err != nil {
		s.render(w, authBody, pageData{Title: "Set up Open Agent Inbox", Notice: err.Error(), CSRF: preAuthCSRF(r)})
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
	s.render(w, authBody, pageData{Title: "Create account", CSRF: s.setPreAuthCSRF(w, r)})
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
		s.render(w, authBody, pageData{Title: "Create account", Notice: err.Error(), CSRF: preAuthCSRF(r)})
		return
	}
	tok, _, _ := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/dashboard", 303)
}
func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	s.render(w, authBody, pageData{Title: "Log in", CSRF: s.setPreAuthCSRF(w, r)})
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.Service.Config.TrustProxyHeaders)
	if !s.loginLimiter.Allow(ip) {
		http.Error(w, "too many login attempts", 429)
		return
	}
	_ = r.ParseForm()
	u, err := s.Service.Store.AuthenticateUser(r.Context(), r.Form.Get("email"), r.Form.Get("password"))
	if err != nil {
		s.render(w, authBody, pageData{Title: "Log in", Notice: "Invalid email or password", CSRF: preAuthCSRF(r)})
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
	if c, err := r.Cookie("oai_session"); err == nil {
		s.Service.Store.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", 303)
}

const dashboardBody = `<h1>Dashboard</h1><p class="muted">{{bytes .Account.StorageUsedBytes}} of {{bytes .Account.StorageQuotaBytes}} stored.</p>{{if .Notice}}<div class="ok">{{.Notice}}</div>{{end}}{{if .Secret}}<div class="secret"><b>{{.SecretLabel}}</b><pre>{{.Secret}}</pre></div>{{end}}
<div class="grid"><section class="card"><h2>Domains</h2>{{if .Domains}}<table>{{range .Domains}}<tr><td><b>{{.Name}}</b>{{if .CatchAllInboxID}}<br><span class="muted">catch-all: {{.CatchAllInboxID}}</span>{{end}}</td><td><form method="post" action="/ui/domains/{{.ID}}/catchall"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><select name="inbox"><option value="">No catch-all</option>{{range $.Inboxes}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select><button class="secondary">Set</button></form></td></tr>{{end}}</table>{{else}}<p class="muted">Add your receiving domain.</p>{{end}}<form method="post" action="/ui/domains"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><input name="name" placeholder="example.com" required><button>Add domain</button></form></section>
<section class="card"><h2>Inboxes</h2>{{if .Inboxes}}<table>{{range .Inboxes}}<tr><td><b>{{.Address}}</b><br><span class="muted">{{.DisplayName}}</span></td><td><code>{{.ID}}</code></td></tr>{{end}}</table>{{end}}<form method="post" action="/ui/inboxes"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Domain</label><select name="domain" required>{{range .Domains}}<option value="{{.ID}}">{{.Name}}</option>{{end}}</select><div class="row"><div><label>Local part</label><input name="local" placeholder="hermes" required></div><div><label>Name</label><input name="display" placeholder="Hermes"></div></div><button>Create inbox</button></form></section></div>
<div class="grid"><section class="card"><h2>Keys &amp; connections</h2>{{if .Credentials}}<table><tr><th>Name</th><th>Type</th><th>Scope</th></tr>{{range .Credentials}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{.Scope}}</td></tr>{{end}}</table>{{else}}<p class="muted">No keys yet.</p>{{end}}<button type="button" id="add-key">Create key</button></section>
<section class="card"><h2>Outbound providers</h2>{{if .Outbound}}<table><tr><th>Name</th><th>Provider</th><th>Status</th><th></th></tr>{{range .Outbound}}<tr><td>{{.Name}}</td><td>{{.Provider}}</td><td>{{if .Active}}<span class="pill">Active</span>{{else}}<form method="post" action="/ui/outbound/{{.ID}}/active"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Set active</button></form>{{end}}</td><td class="row"><button type="button" class="secondary edit-provider" data-id="{{.ID}}" data-name="{{.Name}}" data-provider="{{.Provider}}" data-config="{{.ConfigJSON}}">Edit</button><form method="post" action="/ui/outbound/{{.ID}}/delete"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary">Delete</button></form></td></tr>{{end}}</table>{{else}}<p class="muted">No outbound provider configured.</p>{{end}}<button type="button" id="add-provider">Add outbound provider</button><p class="muted">The active provider is used for all sending.</p></section></div>
<dialog id="provider-dialog"><form method="post" action="/ui/outbound"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="id"><label>Name</label><input name="name" value="Primary" required><label>Provider</label><select name="provider" id="provider-select">{{range .OutboundProviders}}<option value="{{.Name}}">{{.Description}}</option>{{end}}</select>{{range $p := .OutboundProviders}}<fieldset class="provider-fields" data-provider="{{$p.Name}}" style="border:0;padding:0;margin:0">{{range $f := $p.Fields}}<label>{{$f.Label}}{{if $f.Required}} *{{end}}</label>{{if $f.Options}}<select name="cfg_{{$p.Name}}_{{$f.Name}}">{{range $f.Options}}<option value="{{.Value}}"{{if eq .Value $f.Default}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<input type="{{$f.Type}}" name="cfg_{{$p.Name}}_{{$f.Name}}" value="{{$f.Default}}" placeholder="{{$f.Placeholder}}"{{if $f.Required}} required{{end}}>{{end}}{{end}}</fieldset>{{end}}<div class="row"><button>Save provider</button><button type="button" class="secondary" id="provider-cancel">Cancel</button></div></form></dialog>
<dialog id="key-dialog"><form method="post" action="/ui/keys"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Type</label><select name="type" id="key-type"><option value="api">API key</option><option value="hermes">Hermes relay connection</option></select><label>Name</label><input name="name" placeholder="Hermes EA" required><fieldset class="key-fields" data-type="api" style="border:0;padding:0;margin:0"><label><input style="width:auto" type="checkbox" name="admin" value="1"> Account Admin key</label>{{range .Inboxes}}<label>{{.Address}}</label><select name="role_{{.ID}}"><option value="">No access</option><option>read</option><option>assistant</option><option>owner</option></select>{{end}}</fieldset><fieldset class="key-fields" data-type="hermes" style="border:0;padding:0;margin:0"><label>Inbox</label><select name="inbox">{{range .Inboxes}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select></fieldset><div class="row"><button>Create</button><button type="button" class="secondary" id="key-cancel">Cancel</button></div></form></dialog>
<script src="/assets/app.js" defer></script>
<section class="card"><h2>Recent messages</h2><form method="get" action="/dashboard" class="row"><input name="q" value="" placeholder="Search mail"><button>Search</button></form>{{if .Messages}}<table><tr><th>When</th><th>From</th><th>Subject</th><th></th></tr>{{range .Messages}}<tr><td>{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{.From.Address}}</td><td>{{.Subject}}</td><td><a href="/ui/messages/{{.ID}}">Open</a></td></tr>{{end}}</table>{{else}}<p class="muted">No messages yet.</p>{{end}}</section>`

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
	}
	ov := s.outboundViews(creds, acc.ActiveOutboundCredentialID)
	s.render(w, dashboardBody, pageData{Title: "Dashboard", Principal: p, CSRF: csrf(r), Account: acc, Domains: domains, Inboxes: boxes, Messages: msgs, Credentials: credentialViews(keys, conns), Outbound: ov, OutboundProviders: outboundProviderViews(), Notice: r.URL.Query().Get("notice")})
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
func (s *Server) uiCreateKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if r.Form.Get("type") == "hermes" {
		gatewayID, secret, deliveryKey, err := s.Service.CreateHermesRelay(r.Context(), p, r.Form.Get("inbox"), r.Form.Get("name"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.renderSecretDashboard(w, r, "Hermes relay connection created", "Paste these lines into the gateway .env", hermesEnvBlock(s.Service.Config.BaseURL, gatewayID, secret, deliveryKey))
		return
	}
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
	s.renderSecretDashboard(w, r, "API key created", "Copy this API key now", plain)
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
		out = append(out, credentialView{Name: k.Name, Type: "API key", Scope: apiKeyScope(k)})
	}
	for _, h := range conns {
		out = append(out, credentialView{Name: h.Name, Type: "Hermes relay", Scope: "Owner"})
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
	return fmt.Sprintf("GATEWAY_RELAY_URL=%s\nGATEWAY_RELAY_ID=%s\nGATEWAY_RELAY_SECRET=%s\nGATEWAY_RELAY_DELIVERY_KEY=%s",
		baseURL, gatewayID, secret, deliveryKey)
}

func (s *Server) renderSecretDashboard(w http.ResponseWriter, r *http.Request, notice, secretLabel, secret string) {
	p := principal(r)
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	domains, _ := s.Service.Store.ListDomains(r.Context(), p.AccountID)
	boxes, _ := s.Service.Store.ListInboxes(r.Context(), p)
	keys, _ := s.Service.Store.ListAPIKeys(r.Context(), p.AccountID)
	msgs, _ := s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{Limit: 100})
	creds, _ := s.Service.Store.ListOutboundCredentials(r.Context(), p.AccountID)
	conns, _ := s.Service.Store.ListHermesConnections(r.Context(), p.AccountID)
	ov := s.outboundViews(creds, acc.ActiveOutboundCredentialID)
	s.render(w, dashboardBody, pageData{Title: "Dashboard", Principal: p, CSRF: csrf(r), Account: acc, Domains: domains, Inboxes: boxes, Messages: msgs, Credentials: credentialViews(keys, conns), Outbound: ov, OutboundProviders: outboundProviderViews(), Notice: notice, SecretLabel: secretLabel, Secret: secret})
}

const messageBody = `<p><a href="/dashboard">← Dashboard</a></p><section class="card"><h1>{{.Message.Subject}}</h1><p><b>From:</b> {{.Message.From.Address}}<br><b>To:</b> {{join .Message.To ", "}}<br><b>Mailbox:</b> <code>{{.Message.InboxID}}</code><br><b>Thread:</b> <code>{{.Message.ThreadID}}</code></p>{{if .Attachments}}<h3>Attachments</h3><ul>{{range .Attachments}}<li>{{.Filename}} · {{bytes .Size}}</li>{{end}}</ul>{{end}}<hr><div class="msgbody">{{.Message.Text}}</div>{{if .Message.HTML}}<details><summary>Sanitized HTML source</summary><pre>{{.Message.HTML}}</pre></details>{{end}}</section>`

func (s *Server) uiMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		http.Error(w, "message not found", 404)
		return
	}
	atts, _ := s.Service.Store.ListAttachments(r.Context(), p, m.ID)
	s.render(w, messageBody, pageData{Title: m.Subject, Principal: p, CSRF: csrf(r), Message: &m, Attachments: atts})
}
