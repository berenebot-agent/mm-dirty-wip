package httpapp

import (
	"net/http"
	"sort"
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

// memberView is a mailbox operator as rendered in the account page: the login
// email plus the mailboxes it owns, both as display names and as a CSV of ids
// for the edit dialog's data attribute.
type memberView struct {
	ID             string
	Email          string
	InboxesCSV     string
	InboxAddresses []string
}

// operatorViews narrows an account's users to its mailbox operators (non-admin,
// non-system members) and resolves their owned mailboxes to addresses.
func operatorViews(users []model.User, addresses map[string]string) []memberView {
	out := []memberView{}
	for _, u := range users {
		if u.IsAdmin || u.SystemAdmin {
			continue
		}
		ids := make([]string, 0, len(u.Roles))
		for id := range u.Roles {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		mv := memberView{ID: u.ID, Email: u.Email, InboxesCSV: strings.Join(ids, ",")}
		for _, id := range ids {
			if addr := addresses[id]; addr != "" {
				mv.InboxAddresses = append(mv.InboxAddresses, addr)
			}
		}
		out = append(out, mv)
	}
	return out
}

// inviteFlash carries a one-time setup link from the POST that created it to
// the page that displays it, so refreshing cannot create it again.
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

// ---------------------------------------------------------------------------
// System administrator plane (/admin): create new accounts and see every
// invitation. Account-level work (mailer, operators) lives on the account page.
// ---------------------------------------------------------------------------

const adminPlaneBody = `<h1>Admin</h1>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{if .InviteLink}}<section class="card"><h2>Setup link</h2><p class="muted">Share this single-use link now — it is shown only once. Sending the invitation or creating another one replaces it.</p><div class="secret"><pre>{{.InviteLink}}</pre></div></section>{{end}}
<section class="card"><div class="card-head"><h2>Accounts</h2><button type="button" id="add-account">Create invitation</button></div><p class="muted">Each account has its own Admin, domains and mailboxes. Invite a new person to create a separate account.</p>
{{if .Accounts}}<div class="table-wrap"><table class="dense"><thead><tr><th>Account</th><th>Admin</th><th>Status</th><th></th></tr></thead><tbody>{{range .Accounts}}<tr><td>{{.Name}}</td><td>{{if .AdminEmail}}{{.AdminEmail}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .AdminEmail}}<span class="pill">Active</span>{{else if .InviteID}}<span class="pill amber">Invitation pending</span> <span class="muted">· expires {{mailDate .InviteExpiresAt}}</span>{{else}}<span class="muted">No admin</span>{{end}}</td><td class="actions">{{if and (not .AdminEmail) .InviteID}}<form method="post" action="/ui/admin/invites/{{.InviteID}}/send"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm"{{if not $.AccountMailerInboxID}} disabled title="Set a mailer on your account page first"{{end}}>Send</button></form><form method="post" action="/ui/admin/invites/{{.InviteID}}/reissue"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Reissue link</button></form><form method="post" action="/ui/admin/invites/{{.InviteID}}/revoke" data-confirm="Revoke this invitation?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger">Revoke</button></form>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No accounts yet.</p>{{end}}</section>
<dialog id="account-invite-dialog"><form method="post" action="/ui/admin/invites"><input type="hidden" name="_csrf" value="{{.CSRF}}"><h2>Invite a new account</h2><p class="muted">Creates a separate account whose Admin sets their own password from a link. The invitation is sent from your own account's mailer.</p><label>Email</label><input type="email" name="email" required><label>Account name (optional)</label><input name="account_name"><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit">Create invitation</button></div></form></dialog>`

func (s *Server) adminPlane(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	ctx := r.Context()
	accounts, err := s.Service.Store.ListAccounts(ctx)
	if err != nil {
		http.Error(w, "cannot list accounts", 500)
		return
	}
	mailer, _ := s.Service.Store.AccountMailerInboxID(ctx, p.AccountID)
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	s.render(w, adminPlaneBody, pageData{Title: "Admin", Tab: "admin", Principal: p, CSRF: csrf(r), Account: acc, Accounts: accounts, AccountMailerInboxID: mailer, InviteLink: s.peekInviteLink(r), Notice: r.URL.Query().Get("notice")})
}

// createInvite handles the shared invite creation. accountID is the target
// account for operator invites and "" for a brand-new account.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, accountID, kind, redirect string) {
	p := principal(r)
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
	s.createInvite(w, r, "", model.InviteKindAccountAdmin, "/admin")
}

func (s *Server) uiAdminSendInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	s.sendInviteEmail(w, r, p, "", r.PathValue("id"), "/admin")
}

func (s *Server) uiAdminReissueInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !isSystemAdmin(w, p) {
		return
	}
	inv, err := s.Service.Store.GetInviteByID(r.Context(), r.PathValue("id"))
	if err != nil || inv.Kind != model.InviteKindAccountAdmin {
		http.Error(w, "invitation not found", 404)
		return
	}
	if !inv.Pending(time.Now().UTC()) {
		http.Error(w, "invitation is no longer pending", 409)
		return
	}
	token, err := s.Service.Store.RotateInviteToken(r.Context(), "", inv.ID, inviteTTL)
	if err != nil {
		http.Error(w, "cannot refresh invitation token", 500)
		return
	}
	dest := "/admin"
	if tok := s.flashes.put(inviteFlash{Link: s.inviteLink(token)}, len(token)+64); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, 303)
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

// ---------------------------------------------------------------------------
// Account page: mailer selection and mailbox-operator management.
// ---------------------------------------------------------------------------

// accountOperatorsSection is appended to the account settings body and only
// shown to an account Admin. It carries the mailer selector, the operator list
// and pending invitations, and the create/edit dialog.
const accountOperatorsSection = `{{if .Principal.Admin}}
{{if .InviteLink}}<section class="card"><h2>Setup link</h2><p class="muted">Share this single-use link now — it is shown only once. Sending the invitation or creating another one replaces it.</p><div class="secret"><pre>{{.InviteLink}}</pre></div></section>{{end}}
<section class="card"><h2>Mailer</h2><p class="muted">The mailbox this account sends its invitations from. Only this account's mailboxes can be selected.</p><form method="post" action="/ui/account/mailer"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Mailbox</label><select name="inbox"><option value="">None</option>{{range .Inboxes}}<option value="{{.ID}}"{{if eq .ID $.AccountMailerInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button>Save</button></div></form></section>
<section class="card"><div class="card-head"><h2>Mailbox operators</h2><button type="button" id="add-operator">Create invitation</button></div><p class="muted">Operators sign in with their own login and are Owner of the mailboxes you select. They cannot manage domains, clients or account settings.</p>
{{if .Operators}}<div class="table-wrap"><table class="dense"><thead><tr><th>Email</th><th>Mailboxes</th><th></th></tr></thead><tbody>{{range .Operators}}<tr><td>{{.Email}}</td><td>{{range .InboxAddresses}}<span class="pill">{{.}}</span> {{end}}{{if not .InboxAddresses}}<span class="muted">—</span>{{end}}</td><td class="actions"><button type="button" class="secondary btn-sm edit-operator" data-id="{{.ID}}" data-email="{{.Email}}" data-inboxes="{{.InboxesCSV}}">Edit</button><form method="post" action="/ui/account/operators/{{.ID}}/delete" data-confirm="Remove this operator?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger">Remove</button></form></td></tr>{{end}}</tbody></table></div>{{else}}<p class="muted">No operators yet.</p>{{end}}
{{if .Invites}}<h3>Pending invitations</h3><div class="table-wrap"><table class="dense"><thead><tr><th>Email</th><th>Expires</th><th></th></tr></thead><tbody>{{range .Invites}}<tr><td>{{.Email}}</td><td class="muted">{{mailDate .Expires}}</td><td class="actions"><form method="post" action="/ui/account/operators/invites/{{.ID}}/send"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm"{{if not $.AccountMailerInboxID}} disabled title="Set a mailer above first"{{end}}>Send</button></form><form method="post" action="/ui/account/operators/invites/{{.ID}}/reissue"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm">Reissue link</button></form><form method="post" action="/ui/account/operators/invites/{{.ID}}/revoke" data-confirm="Revoke this invitation?"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary btn-sm danger">Revoke</button></form></td></tr>{{end}}</tbody></table></div>{{end}}</section>
<dialog id="operator-invite-dialog"><form method="post" action="/ui/account/operators/invites"><input type="hidden" name="_csrf" value="{{.CSRF}}"><h2 id="operator-invite-title">Create invitation</h2>{{if .Inboxes}}<fieldset style="border:1px solid #ddd;border-radius:8px;padding:8px 12px;margin:4px 0 10px"><legend class="muted">Mailboxes</legend>{{range .Inboxes}}<label style="display:flex;align-items:center;gap:8px"><input type="checkbox" name="inboxes" value="{{.ID}}" style="width:auto;margin:0"> {{.Address}}</label>{{end}}</fieldset>{{else}}<p class="muted">Create a mailbox first.</p>{{end}}<label>Email</label><input type="email" name="email" required><div class="dialog-actions"><button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" id="operator-invite-submit">Create invitation</button></div></form></dialog>
{{end}}`

func (s *Server) uiAccountMailer(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.SetAccountMailerInbox(r.Context(), p.AccountID, strings.TrimSpace(r.Form.Get("inbox"))); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	http.Redirect(w, r, "/account?notice=Mailer+saved", 303)
}

func (s *Server) uiOperatorCreateInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	s.createInvite(w, r, p.AccountID, model.InviteKindOperator, "/account")
}

func (s *Server) uiOperatorSendInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	s.sendInviteEmail(w, r, p, p.AccountID, r.PathValue("id"), "/account")
}

func (s *Server) uiOperatorReissueInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	inv, err := s.Service.Store.GetInviteByID(r.Context(), r.PathValue("id"))
	if err != nil || inv.AccountID != p.AccountID || inv.Kind != model.InviteKindOperator {
		http.Error(w, "invitation not found", 404)
		return
	}
	if !inv.Pending(time.Now().UTC()) {
		http.Error(w, "invitation is no longer pending", 409)
		return
	}
	token, err := s.Service.Store.RotateInviteToken(r.Context(), p.AccountID, inv.ID, inviteTTL)
	if err != nil {
		http.Error(w, "cannot refresh invitation token", 500)
		return
	}
	dest := "/account"
	if tok := s.flashes.put(inviteFlash{Link: s.inviteLink(token)}, len(token)+64); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, 303)
}

func (s *Server) uiOperatorRevokeInvite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.RevokeInvite(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, "invitation not found", 404)
		return
	}
	http.Redirect(w, r, "/account?notice=Invitation+revoked", 303)
}

func (s *Server) uiOperatorSetRoles(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	// The dialog submits one checkbox per owned mailbox, all named "inboxes";
	// every checked mailbox grants Owner.
	_ = r.ParseForm()
	roles := map[string]string{}
	for _, id := range r.Form["inboxes"] {
		roles[id] = "owner"
	}
	if err := s.Service.Store.SetUserRoles(r.Context(), p.AccountID, r.PathValue("id"), roles); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.Service.Hub.CancelScope("user:" + r.PathValue("id"))
	http.Redirect(w, r, "/account?notice=Operator+access+updated", 303)
}

func (s *Server) uiOperatorDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteAccountMember(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, "operator not found", 404)
		return
	}
	s.Service.Hub.CancelScope("user:" + r.PathValue("id"))
	http.Redirect(w, r, "/account?notice=Operator+removed", 303)
}

// sendInviteEmail rotates the invite's one-time token and enqueues the
// invitation through the ordinary outbound queue from the caller's account
// mailer. inviteAccountID scopes the lookup ("" allows any account, for the
// system administrator); the sending mailbox always belongs to the caller's
// account, so no account sends from another's mailbox.
func (s *Server) sendInviteEmail(w http.ResponseWriter, r *http.Request, p model.Principal, inviteAccountID, inviteID, redirect string) {
	mailer, err := s.Service.Store.AccountMailerInboxID(r.Context(), p.AccountID)
	if err != nil || mailer == "" {
		http.Error(w, "set the account mailer first", 400)
		return
	}
	inv, err := s.Service.Store.GetInviteByID(r.Context(), inviteID)
	if err != nil || (inviteAccountID != "" && inv.AccountID != inviteAccountID) {
		http.Error(w, "invitation not found", 404)
		return
	}
	if !inv.Pending(time.Now().UTC()) {
		http.Error(w, "invitation is no longer pending", 409)
		return
	}
	token, err := s.Service.Store.RotateInviteToken(r.Context(), inviteAccountID, inviteID, inviteTTL)
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

// operatorBody is the landing page for a non-admin mailbox operator: the
// mailboxes they own, rendered with the same counts and readiness flags as the
// account Admin's dashboard table.
const operatorBody = `<h1>Mailboxes</h1><p class="muted">You have Owner access to the following mailboxes.</p>
{{if .Inboxes}}<section class="card">{{template "inboxes-table" .}}</section>{{else}}<p class="muted">No mailboxes have been assigned to you yet.</p>{{end}}`

func (s *Server) operatorDashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ctx := r.Context()
	inboxes, err := s.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		http.Error(w, "cannot list mailboxes", 500)
		return
	}
	domains, _ := s.Service.Store.ListDomains(ctx, p.AccountID)
	_, receivingReady, inboxSendingReady := inboxReadiness(domains, inboxes)
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
	if unread == nil {
		unread = map[string]int{}
	}
	draftCounts, _ := s.Service.Store.PendingDraftCountsByInbox(ctx, p)
	if draftCounts == nil {
		draftCounts = map[string]int{}
	}
	mailboxSizes, _ := s.Service.Store.MessageSizesByInbox(ctx, p)
	if mailboxSizes == nil {
		mailboxSizes = map[string]int64{}
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	s.render(w, operatorBody, pageData{
		Title:                "Mailboxes",
		Principal:            p,
		CSRF:                 csrf(r),
		Account:              acc,
		Inboxes:              inboxes,
		Unread:               unread,
		DraftCounts:          draftCounts,
		MailboxSizes:         mailboxSizes,
		InboxSendingReady:    inboxSendingReady,
		DomainReceivingReady: receivingReady,
	})
}
