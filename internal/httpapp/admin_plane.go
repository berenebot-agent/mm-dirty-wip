package httpapp

import (
	"net/http"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// inviteTTL is how long an account or operator setup link remains valid.
const inviteTTL = 7 * 24 * time.Hour

// inviteView is the rendered shape of an invitation: the stored record plus a
// human-readable kind label and lifecycle status.
type inviteView struct {
	model.Invite
	KindLabel string
	Status    string
	Expires   time.Time
}

func newInviteView(inv model.Invite, now time.Time) inviteView {
	kind := "New account"
	if inv.Kind == model.InviteKindOperator {
		kind = "Mailbox operator"
	}
	status := "Pending"
	switch {
	case inv.RevokedAt != nil:
		status = "Revoked"
	case inv.AcceptedAt != nil:
		status = "Accepted"
	case !now.Before(inv.ExpiresAt):
		status = "Expired"
	}
	return inviteView{Invite: inv, KindLabel: kind, Status: status, Expires: inv.ExpiresAt}
}

// inviteFlash carries a one-time setup link from the POST that created it to
// the admin page that displays it, so refreshing cannot create it again.
type inviteFlash struct {
	Link string
}

func (s *Server) peekInviteLink(r *http.Request) string {
	if v, ok := s.flashes.take(r.URL.Query().Get("_flash")); ok {
		if f, ok := v.(inviteFlash); ok {
			return f.Link
		}
	}
	return ""
}

// inviteLink builds the absolute, canonical setup URL for a plaintext token.
// It uses the configured base URL, never the request Host, because the link is
// emailed and must not be influenced by an attacker-controlled header.
func (s *Server) inviteLink(token string) string {
	return strings.TrimRight(s.Service.Config.BaseURL, "/") + "/invite/" + token
}

func isSystemAdmin(w http.ResponseWriter, p model.Principal) bool {
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return false
	}
	return true
}

const adminPlaneBody = `<h1>Admin</h1>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if .InviteLink}}<section class="card"><h2>Setup link</h2><p class="muted">Share this single-use link now — it is shown only once. Sending the invitation or creating another one replaces it.</p><div class="secret"><pre>{{.InviteLink}}</pre></div></section>{{end}}
<div class="grid">
<section class="card"><h2>System mailer</h2><p class="muted">The mailbox MailMoose uses to send account invitations. Only mailboxes on your own account can be selected.</p><form method="post" action="/ui/admin/mailer"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Mailbox</label><select name="inbox"><option value="">None</option>{{range .Inboxes}}<option value="{{.ID}}"{{if eq .ID $.SystemMailerInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button>Save</button></div></form></section>
<section class="card"><h2>Invite a new account</h2><p class="muted">Creates a separate account whose Admin sets their own password from an emailed link.</p><form method="post" action="/ui/admin/invites"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="kind" value="account_admin"><label>Email</label><input type="email" name="email" required><label>Account name (optional)</label><input name="account_name"><div class="dialog-actions"><button>Create invitation</button></div></form></section>
</div>
<section class="card"><h2>Invitations</h2>{{if .Invites}}<div class="table-wrap"><table class="dense"><thead><tr><th>Email</th><th>Type</th><th>Status</th><th></th></tr></thead><tbody>{{range .Invites}}<tr><td>{{.Email}}{{if .AccountName}} <span class="muted">· {{.AccountName}}</span>{{end}}</td><td>{{.KindLabel}}</td><td>{{.Status}}{{if eq .Status "Pending"}} <span class="muted">· expires {{mailDate .Expires}}</span>{{end}}</td><td class="actions"><form method="post" action="/ui/admin/invites/{{.ID}}/send"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm"{{if not $.SystemMailerInboxID}} disabled title="Select a system mailer first"{{end}}>Send</button></form><form method="post" action="/ui/admin/invites/{{.ID}}/revoke" data-confirm="Revoke this invitation?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger">Revoke</button></form></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No invitations yet.</p>{{end}}</section>`

func (s *Server) adminPlane(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	ctx := r.Context()
	inboxes, _ := s.Service.Store.ListInboxes(ctx, p)
	invites, err := s.Service.Store.ListInvites(ctx, "")
	if err != nil {
		http.Error(w, "cannot list invitations", 500)
		return
	}
	mailer, _ := s.Service.Store.SystemMailerInboxID(ctx)
	views := make([]inviteView, 0, len(invites))
	now := time.Now().UTC()
	for _, inv := range invites {
		views = append(views, newInviteView(inv, now))
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	s.render(w, adminPlaneBody, pageData{Title: "Admin", Tab: "admin", Principal: p, CSRF: csrf(r), Account: acc, Inboxes: inboxes, Invites: views, SystemMailerInboxID: mailer, InviteLink: s.peekInviteLink(r), Notice: r.URL.Query().Get("notice")})
}

func (s *Server) uiAdminMailer(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	if err := s.Service.Store.SetSystemMailerInbox(r.Context(), p.AccountID, strings.TrimSpace(r.Form.Get("inbox"))); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/admin?notice=System+mailer+saved", 303)
}

// createInvite handles the shared account/operator invite creation. accountID
// is the target account for operator invites and "" for a new account.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, accountID, redirect string) {
	p := principal(r)
	kind := strings.TrimSpace(r.Form.Get("kind"))
	if kind != model.InviteKindAccountAdmin && kind != model.InviteKindOperator {
		http.Error(w, "invalid invitation kind", 400)
		return
	}
	var inboxIDs []string
	if kind == model.InviteKindOperator {
		inboxIDs = r.Form["inboxes"]
	}
	inv, token, err := s.Service.Store.CreateInvite(r.Context(), store.InviteInput{
		AccountID:   accountID,
		AccountName: r.Form.Get("account_name"),
		Email:       r.Form.Get("email"),
		Kind:        kind,
		InboxIDs:    inboxIDs,
		Quota:       s.Service.Config.DefaultQuotaBytes,
		CreatedBy:   p.UserID,
		TTL:         inviteTTL,
	})
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Store.Audit(r.Context(), inv.AccountID, "invite.created", inv.Email)
	if token == "" {
		http.Redirect(w, r, redirect+"?notice=Invitation+created", 303)
		return
	}
	if tok := s.flashes.put(inviteFlash{Link: s.inviteLink(token)}, len(token)+64); tok != "" {
		redirect += "?_flash=" + tok
	}
	http.Redirect(w, r, redirect, 303)
}

func (s *Server) uiAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	kind := strings.TrimSpace(r.Form.Get("kind"))
	accountID := ""
	if kind == model.InviteKindOperator {
		accountID = p.AccountID
	}
	s.createInvite(w, r, accountID, "/admin")
}

func (s *Server) uiAdminSendInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	s.sendInviteEmail(w, r, p, "", r.PathValue("id"))
}

func (s *Server) uiAdminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	if err := s.Service.Store.RevokeInvite(r.Context(), "", r.PathValue("id")); err != nil {
		http.Error(w, "invitation not found", 404)
		return
	}
	http.Redirect(w, r, "/admin?notice=Invitation+revoked", 303)
}

// sendInviteEmail rotates the invite's one-time token and enqueues the
// invitation through the ordinary outbound queue from the selected system
// mailer. accountID scopes the lookup ("" allows any account, for the system
// administrator).
func (s *Server) sendInviteEmail(w http.ResponseWriter, r *http.Request, p model.Principal, accountID, inviteID string) {
	redirect := "/admin"
	if accountID != "" {
		redirect = "/members"
	}
	mailer, err := s.Service.Store.SystemMailerInboxID(r.Context())
	if err != nil || mailer == "" {
		http.Error(w, "select a system mailer mailbox first", 400)
		return
	}
	inv, err := s.Service.Store.GetInviteByID(r.Context(), inviteID)
	if err != nil || (accountID != "" && inv.AccountID != accountID) {
		http.Error(w, "invitation not found", 404)
		return
	}
	if !inv.Pending(time.Now().UTC()) {
		http.Error(w, "invitation is no longer pending", 409)
		return
	}
	token, err := s.Service.Store.RotateInviteToken(r.Context(), accountID, inviteID, inviteTTL)
	if err != nil {
		http.Error(w, "cannot refresh invitation token", 500)
		return
	}
	link := s.inviteLink(token)
	subject := "Set up your MailMoose account"
	text := "You have been invited to MailMoose.\r\n\r\n" +
		"Choose your password using this single-use link:\r\n\r\n" + link + "\r\n\r\n" +
		"This link expires on " + inv.ExpiresAt.UTC().Format(time.RFC1123) + ".\r\n" +
		"If you were not expecting this invitation you can ignore it.\r\n"
	html := "<p>You have been invited to MailMoose.</p>" +
		"<p><a href=\"" + htmlEscape(link) + "\">Choose your password</a></p>" +
		"<p>This single-use link expires on " + htmlEscape(inv.ExpiresAt.UTC().Format(time.RFC1123)) + ".</p>"
	if _, err := s.Service.SendSystemMail(r.Context(), p.AccountID, mailer, app.SendInput{To: []string{inv.Email}, Subject: subject, Text: text, HTML: html}); err != nil {
		http.Error(w, "could not queue invitation: "+err.Error(), 400)
		return
	}
	s.Service.Store.Audit(r.Context(), inv.AccountID, "invite.sent", inv.Email)
	http.Redirect(w, r, redirect+"?notice=Invitation+queued", 303)
}

// htmlEscape is a tiny helper for the invitation body; the values are all
// server-generated, but the recipient email can contain characters that must
// not break the markup.
func htmlEscape(v string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(v)
}

const membersBody = `<h1>Members</h1>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if .InviteLink}}<section class="card"><h2>Setup link</h2><p class="muted">Share this single-use link now — it is shown only once. Sending the invitation replaces it.</p><div class="secret"><pre>{{.InviteLink}}</pre></div></section>{{end}}
<section class="card"><h2>Invite a mailbox operator</h2><p class="muted">Operators sign in with their own login and are Owner of the mailboxes you select. They cannot manage domains, clients or account settings.</p><form method="post" action="/ui/members/invites"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="kind" value="operator"><label>Email</label><input type="email" name="email" required>{{if .Inboxes}}<fieldset style="border:1px solid #ddd;border-radius:8px;padding:8px 12px;margin:4px 0 10px"><legend class="muted">Mailboxes</legend>{{range .Inboxes}}<label style="display:flex;align-items:center;gap:8px"><input type="checkbox" name="inboxes" value="{{.ID}}" style="width:auto;margin:0"> {{.Address}}</label>{{end}}</fieldset>{{else}}<p class="muted">Create a mailbox first.</p>{{end}}<div class="dialog-actions"><button>Create invitation</button></div></form></section>
<section class="card"><h2>People</h2>{{if .Members}}<div class="table-wrap"><table class="dense"><thead><tr><th>Email</th><th>Access</th><th></th></tr></thead><tbody>{{range .Members}}<tr><td>{{.Email}}</td><td>{{if .IsAdmin}}<span class="pill">Account Admin</span>{{else if .IsSystemAdmin}}<span class="pill">System Admin</span>{{else}}{{$m := .}}<form method="post" action="/ui/members/{{$m.ID}}/roles"><input type="hidden" name="_csrf" value="{{$.CSRF}}">{{range $.Inboxes}}<label style="display:inline-flex;align-items:center;gap:6px;font-size:13px;margin-right:10px"><input type="checkbox" name="inbox" value="{{.ID}}" style="width:auto;margin:0"{{if index $m.Roles .ID}} checked{{end}}> {{.Address}}</label>{{end}}<button class="secondary btn-sm">Save access</button></form>{{end}}</td><td class="actions">{{if or .IsAdmin .IsSystemAdmin}}<span class="muted">—</span>{{else}}<form method="post" action="/ui/members/{{.ID}}/delete" data-confirm="Remove this member?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger">Remove</button></form>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No members yet.</p>{{end}}</section>`

func (s *Server) membersPage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	members, err := s.Service.Store.ListAccountUsers(ctx, p.AccountID)
	if err != nil {
		http.Error(w, "cannot list members", 500)
		return
	}
	inboxes, _ := s.Service.Store.ListInboxes(ctx, p)
	invites, _ := s.Service.Store.ListInvites(ctx, p.AccountID)
	views := make([]inviteView, 0, len(invites))
	now := time.Now().UTC()
	for _, inv := range invites {
		if inv.Kind != model.InviteKindOperator {
			continue
		}
		views = append(views, newInviteView(inv, now))
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	s.render(w, membersBody, pageData{Title: "Members", Tab: "members", Principal: p, CSRF: csrf(r), Account: acc, Inboxes: inboxes, Members: members, Invites: views, InviteLink: s.peekInviteLink(r), Notice: r.URL.Query().Get("notice")})
}

func (s *Server) uiMembersCreateInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	s.createInvite(w, r, p.AccountID, "/members")
}

func (s *Server) uiMembersSetRoles(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	// The members form submits one (inbox, owner) pair per checkbox. Any inbox
	// checked grants Owner; unchecked inboxes are dropped.
	roles := map[string]string{}
	_ = r.ParseForm()
	for _, id := range r.Form["inbox"] {
		roles[id] = "owner"
	}
	if err := s.Service.Store.SetUserRoles(r.Context(), p.AccountID, r.PathValue("id"), roles); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("user:" + r.PathValue("id"))
	http.Redirect(w, r, "/members?notice=Member+access+updated", 303)
}

func (s *Server) uiMembersDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteAccountMember(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, "member not found", 404)
		return
	}
	s.Service.Hub.CancelScope("user:" + r.PathValue("id"))
	http.Redirect(w, r, "/members?notice=Member+removed", 303)
}

// operatorBody is the landing page for a non-admin mailbox operator: just the
// mailboxes they own.
const operatorBody = `<h1>Mailboxes</h1><p class="muted">You have Owner access to the following mailboxes.</p>
{{if .Inboxes}}<div class="grid">{{range .Inboxes}}<section class="card"><h2><a href="/ui/inboxes/{{.ID}}">{{.Address}}</a></h2><p class="muted">{{if .DisplayName}}{{.DisplayName}}{{else}}Mailbox{{end}}{{with index $.Unread .ID}} · {{.}} unread{{end}}</p></section>{{end}}</div>{{else}}<p class="muted">No mailboxes have been assigned to you yet.</p>{{end}}`

func (s *Server) operatorDashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxes, err := s.Service.Store.ListInboxes(r.Context(), p)
	if err != nil {
		http.Error(w, "cannot list mailboxes", 500)
		return
	}
	unread, _ := s.Service.Store.UnreadCounts(r.Context(), p)
	if unread == nil {
		unread = map[string]int{}
	}
	acc, _ := s.Service.Store.GetAccount(r.Context(), p.AccountID)
	s.render(w, operatorBody, pageData{Title: "Mailboxes", Principal: p, CSRF: csrf(r), Account: acc, Inboxes: inboxes, Unread: unread})
}
