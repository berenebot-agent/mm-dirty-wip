package httpapp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

// microsoftSelectionAAD binds an exchanged-token blob to the attempt row that
// holds it during a shared-mailbox selection phase.
func microsoftSelectionAAD(account, hash string) string {
	return "microsoft_select:" + account + ":" + hash
}

func (s *Server) uiMicrosoftBegin(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id, tenant := s.Service.Config.MicrosoftClientID, s.Service.Config.MicrosoftTenantID
	if tenant == "" {
		tenant = "common"
	}
	if id == "" || len(id) > 512 || !microsoft.ValidTenant(tenant) {
		http.Error(w, "Microsoft connection is not configured. Set MICROSOFT_CLIENT_ID and MICROSOFT_TENANT_ID for a public-client Entra app.", 400)
		return
	}
	kind := r.Form.Get("mailbox_type")
	if kind != "personal" && kind != "shared" {
		http.Error(w, "Choose Personal or Personal + shared access", 400)
		return
	}
	shared := kind == "shared"
	mailbox := ""
	inbox := r.Form.Get("inbox_id")
	if inbox != "" {
		c, e := s.Service.Store.MicrosoftConnection(r.Context(), p.AccountID, inbox)
		if e != nil {
			http.Error(w, "Microsoft inbox not found", 404)
			return
		}
		id, tenant, mailbox, shared = c.ClientID, c.Tenant, c.Mailbox, c.Shared
	}
	state, verifier := googleRandom(), googleRandom()
	hash := googleStateHash(state)
	enc, e := s.Service.EncryptSecretAAD("microsoft_attempt:"+p.AccountID+":"+hash, []byte(verifier))
	if e != nil {
		http.Error(w, "Could not prepare Microsoft connection", 500)
		return
	}
	a := store.MicrosoftAttempt{StateHash: hash, AccountID: p.AccountID, UserID: p.UserID, InboxID: inbox, DisplayName: strings.TrimSpace(r.Form.Get("display")), ClientID: id, Tenant: tenant, Mailbox: mailbox, Shared: shared, EncryptedPending: enc, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if e = s.Service.Store.SaveMicrosoftAttempt(r.Context(), a); e != nil {
		http.Error(w, "Could not prepare Microsoft connection", 500)
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	v := url.Values{"client_id": {id}, "redirect_uri": {microsoft.RedirectURI}, "response_type": {"code"}, "response_mode": {"query"}, "scope": {microsoft.Scope(shared)}, "prompt": {"select_account"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	writeJSON(w, 200, map[string]string{"authorization_url": "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/authorize?" + v.Encode()})
}

// microsoftSelectedMailbox validates an operator-selected shared mailbox
// address and returns it normalized the way a standalone inbox address is.
func microsoftSelectedMailbox(raw string) (string, bool) {
	addr := strings.ToLower(strings.TrimSpace(raw))
	a, e := mail.ParseAddress(addr)
	if e != nil || !strings.EqualFold(a.Address, addr) {
		return "", false
	}
	return addr, true
}

// uiMicrosoftFinish completes a public-client sign-in. For a shared connection
// without a known mailbox it stops after token exchange, persists the token in
// the attempt row and answers with the accessible shared mailboxes so the
// wizard can offer a selector; the follow-up POST carries the chosen address.
func (s *Server) uiMicrosoftFinish(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	rm := s.remoteMailbox()
	if state := strings.TrimSpace(r.Form.Get("state")); state != "" {
		s.uiMicrosoftSelect(w, r, p, rm, state)
		return
	}
	raw := strings.TrimSpace(r.Form.Get("callback_url"))
	if len(raw) > 8192 {
		http.Error(w, "Invalid callback URL", 400)
		return
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.Host != "localhost" || u.User != nil || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		http.Error(w, "Paste the complete final http://localhost redirect URL", 400)
		return
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q["state"]) != 1 || len(q["code"]) != 1 || q.Get("code") == "" || q.Get("error") != "" {
		http.Error(w, "Microsoft authorization was declined or incomplete; start again", 400)
		return
	}
	hash := googleStateHash(q.Get("state"))
	a, e := s.Service.Store.ConsumeMicrosoftAttempt(r.Context(), hash, p.AccountID, p.UserID)
	if e != nil {
		http.Error(w, "Connection attempt expired or already used; start again", 400)
		return
	}
	b, e := s.Service.DecryptSecretAAD("microsoft_attempt:"+p.AccountID+":"+a.StateHash, a.EncryptedPending)
	if e != nil {
		http.Error(w, "Connection attempt unavailable", 400)
		return
	}
	t, e := rm.Microsoft.Exchange(r.Context(), a.Tenant, a.ClientID, microsoft.RedirectURI, q.Get("code"), string(b))
	if e != nil {
		http.Error(w, "Microsoft authorization failed; check tenant consent and connect again", 400)
		return
	}
	identity, e := rm.Microsoft.Identity(r.Context(), t.AccessToken)
	if e != nil || identity.ID == "" {
		http.Error(w, "Could not verify Microsoft identity", 400)
		return
	}
	if a.Shared && a.Mailbox == "" {
		s.uiMicrosoftChoose(w, r, p, rm, a, t, identity)
		return
	}
	address := a.Mailbox
	mailbox := a.Mailbox
	if !a.Shared {
		mailbox = identity.ID
		address = identity.Mail
		if address == "" {
			address = identity.UserPrincipalName
		}
	}
	if _, e = rm.Microsoft.Folder(r.Context(), t.AccessToken, mailbox, "inbox"); e != nil {
		http.Error(w, "Selected Microsoft mailbox is inaccessible; check delegated mailbox access", 400)
		return
	}
	inbox := a.InboxID
	if inbox != "" {
		old, e := s.Service.Store.MicrosoftConnection(r.Context(), p.AccountID, inbox)
		if e != nil || old.IdentityID != identity.ID || old.Mailbox != mailbox {
			http.Error(w, "Reconnect the original Microsoft identity and mailbox", 400)
			return
		}
	} else {
		box, e := s.Service.Store.CreateStandaloneInbox(store.WithInboxLogOperation(r.Context(), "microsoft_connect"), p.AccountID, store.StandaloneCreate{Address: address, DisplayName: displayOr(a.DisplayName, identity.DisplayName, address)})
		if e != nil {
			http.Error(w, "Could not create Microsoft inbox; the address may already be connected", 400)
			return
		}
		inbox = box.ID
	}
	c := store.MicrosoftConnection{AccountID: p.AccountID, InboxID: inbox, ClientID: a.ClientID, Tenant: a.Tenant, IdentityID: identity.ID, Mailbox: mailbox, Shared: a.Shared}
	if e = rm.SaveMicrosoftGrant(r.Context(), c, t); e != nil {
		if a.InboxID == "" {
			ctx := store.WithInboxLogOperation(r.Context(), "microsoft_connect")
			_, _ = s.Service.Store.PurgeInbox(store.WithInboxLogReason(ctx, "setup_rollback"), p.AccountID, inbox)
		}
		http.Error(w, "Microsoft offline mailbox access was not granted; approve mailbox permissions and reconnect", 400)
		return
	}
	rm.ScheduleRefresh(p.AccountID, inbox)
	writeJSON(w, 200, map[string]string{"inbox_id": inbox})
}

// uiMicrosoftChoose rotates a shared-mailbox attempt into its selection phase:
// the exchanged token is encrypted into the attempt and the accessible shared
// mailboxes (best effort) are returned with the fresh single-use state.
func (s *Server) uiMicrosoftChoose(w http.ResponseWriter, r *http.Request, p model.Principal, rm *app.RemoteMailboxService, a store.MicrosoftAttempt, t microsoft.Token, identity microsoft.User) {
	blob, e := json.Marshal(t)
	if e != nil {
		http.Error(w, "Could not prepare shared mailbox selection", 500)
		return
	}
	selectionState := googleRandom()
	selectionHash := googleStateHash(selectionState)
	enc, e := s.Service.EncryptSecretAAD(microsoftSelectionAAD(p.AccountID, selectionHash), blob)
	if e != nil {
		http.Error(w, "Could not prepare shared mailbox selection", 500)
		return
	}
	next := store.MicrosoftAttempt{StateHash: selectionHash, AccountID: a.AccountID, UserID: a.UserID, InboxID: a.InboxID, DisplayName: a.DisplayName, ClientID: a.ClientID, Tenant: a.Tenant, Mailbox: "", Shared: true, EncryptedPending: "", EncryptedToken: enc, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if e = s.Service.Store.SaveMicrosoftAttempt(r.Context(), next); e != nil {
		http.Error(w, "Could not prepare shared mailbox selection", 500)
		return
	}
	mailboxes := []map[string]string{}
	exclude := identity.Mail
	if exclude == "" {
		exclude = identity.UserPrincipalName
	}
	if found, err := rm.Microsoft.SharedMailboxes(r.Context(), t.AccessToken, exclude); err == nil {
		for _, m := range found {
			mailboxes = append(mailboxes, map[string]string{"address": m.Mail, "name": displayOr(m.DisplayName, m.Mail)})
		}
	}
	writeJSON(w, 200, map[string]any{"mailboxes": mailboxes, "selection_state": selectionState})
}

// uiMicrosoftSelect completes the selection phase with the operator-chosen
// shared mailbox. The attempt is peeked first so an access-denied choice can
// be corrected; the single-use Consume is the commit point.
func (s *Server) uiMicrosoftSelect(w http.ResponseWriter, r *http.Request, p model.Principal, rm *app.RemoteMailboxService, state string) {
	mailbox, ok := microsoftSelectedMailbox(r.Form.Get("mailbox"))
	if !ok {
		http.Error(w, "Enter the shared mailbox email address", 400)
		return
	}
	hash := googleStateHash(state)
	peek, e := s.Service.Store.PeekMicrosoftAttempt(r.Context(), hash, p.AccountID, p.UserID)
	if e != nil || peek.EncryptedToken == "" || !peek.Shared {
		http.Error(w, "Connection attempt expired or was already used; start again", 400)
		return
	}
	blob, e := s.Service.DecryptSecretAAD(microsoftSelectionAAD(p.AccountID, peek.StateHash), peek.EncryptedToken)
	if e != nil {
		http.Error(w, "Connection attempt unavailable; start again", 400)
		return
	}
	var t microsoft.Token
	if json.Unmarshal(blob, &t) != nil {
		http.Error(w, "Connection attempt unavailable; start again", 400)
		return
	}
	identity, e := rm.Microsoft.Identity(r.Context(), t.AccessToken)
	if e != nil || identity.ID == "" {
		http.Error(w, "Could not verify Microsoft identity", 400)
		return
	}
	if _, e = rm.Microsoft.Folder(r.Context(), t.AccessToken, mailbox, "inbox"); e != nil {
		http.Error(w, "Selected Microsoft mailbox is inaccessible; check delegated mailbox access", 400)
		return
	}
	a, e := s.Service.Store.ConsumeMicrosoftAttempt(r.Context(), hash, p.AccountID, p.UserID)
	if e != nil {
		http.Error(w, "Connection attempt expired or was already used; start again", 400)
		return
	}
	inbox := a.InboxID
	if inbox != "" {
		old, e := s.Service.Store.MicrosoftConnection(r.Context(), p.AccountID, inbox)
		if e != nil || old.IdentityID != identity.ID || old.Mailbox != mailbox {
			http.Error(w, "Reconnect the original Microsoft identity and mailbox", 400)
			return
		}
	} else {
		box, e := s.Service.Store.CreateStandaloneInbox(store.WithInboxLogOperation(r.Context(), "microsoft_connect"), p.AccountID, store.StandaloneCreate{Address: mailbox, DisplayName: displayOr(a.DisplayName, mailbox)})
		if e != nil {
			http.Error(w, "Could not create Microsoft inbox; the address may already be connected", 400)
			return
		}
		inbox = box.ID
	}
	c := store.MicrosoftConnection{AccountID: p.AccountID, InboxID: inbox, ClientID: a.ClientID, Tenant: a.Tenant, IdentityID: identity.ID, Mailbox: mailbox, Shared: true}
	if e = rm.SaveMicrosoftGrant(r.Context(), c, t); e != nil {
		if a.InboxID == "" {
			ctx := store.WithInboxLogOperation(r.Context(), "microsoft_connect")
			_, _ = s.Service.Store.PurgeInbox(store.WithInboxLogReason(ctx, "setup_rollback"), p.AccountID, inbox)
		}
		http.Error(w, "Microsoft offline mailbox access was not granted; approve mailbox permissions and reconnect", 400)
		return
	}
	rm.ScheduleRefresh(p.AccountID, inbox)
	writeJSON(w, 200, map[string]string{"inbox_id": inbox})
}

const microsoftWizardBody = `<section data-wizard-step="microsoft" hidden><h2 class="wizard-step-title"><span class="wizard-logo" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="1" y="1" width="10" height="10" fill="#F25022"/><rect x="13" y="1" width="10" height="10" fill="#7FBA00"/><rect x="1" y="13" width="10" height="10" fill="#00A4EF"/><rect x="13" y="13" width="10" height="10" fill="#FFB900"/></svg></span>Microsoft / Outlook</h2><p class="muted">Connect a whole personal or shared mailbox. Outlook remains the source of truth. Shared access may require tenant-admin consent and separate Send As / Send on Behalf permission.</p>
<form class="wizard-box" data-microsoft-begin method="post" action="/ui/oauth/microsoft/begin"><h3 class="section-head">Connect Microsoft</h3><p class="muted small">Personal connects only the mailbox you sign in with. Personal + shared also lets you pick a mailbox you can access but do not own; it asks for extra delegated permissions, so your tenant may need an administrator to approve them.</p><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="inbox_id" data-microsoft-inbox><input type="hidden" name="mailbox_type" value="personal"><div class="ms-connect-grid"><button type="submit" class="ms-connect" name="mailbox_type" value="personal"><span class="ms-logo" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="1" y="1" width="10" height="10" fill="#F25022"/><rect x="13" y="1" width="10" height="10" fill="#7FBA00"/><rect x="1" y="13" width="10" height="10" fill="#00A4EF"/><rect x="13" y="13" width="10" height="10" fill="#FFB900"/></svg></span><span class="ms-text"><b>Connect Microsoft</b><span>Personal Inbox only</span></span></button><button type="submit" class="ms-connect" name="mailbox_type" value="shared"><span class="ms-logo" aria-hidden="true"><svg viewBox="0 0 24 24"><rect x="1" y="1" width="10" height="10" fill="#F25022"/><rect x="13" y="1" width="10" height="10" fill="#7FBA00"/><rect x="1" y="13" width="10" height="10" fill="#00A4EF"/><rect x="13" y="13" width="10" height="10" fill="#FFB900"/></svg></span><span class="ms-text"><b>Connect Microsoft</b><span>Personal Inbox and Shared Inbox</span></span></button></div></form><p data-microsoft-status role="status"></p>
<form class="wizard-box" data-microsoft-finish method="post" action="/ui/oauth/microsoft/finish"><p class="paste-hint" data-microsoft-paste-hint hidden>Paste the resulting URL here ↓</p><h3 class="section-head">Finish the connection</h3><p class="muted small">After you approve access, your browser redirects to <code>http://localhost</code>. A connection error is expected: no listener is running on your computer. Copy the complete URL from the address bar and paste it below. If the browser hides the URL, use another desktop browser or start again.</p><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Complete localhost callback URL</label><input name="callback_url" required autocomplete="off"><button>Complete connection</button></form>
<div data-microsoft-choice hidden></div><div class="dialog-actions"><button type="button" class="secondary" data-wizard-back>Back</button></div></section>`
