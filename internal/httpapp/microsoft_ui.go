package httpapp

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

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
		http.Error(w, "Choose Personal or Shared mailbox", 400)
		return
	}
	shared := kind == "shared"
	mailbox := strings.TrimSpace(r.Form.Get("mailbox"))
	if shared {
		a, e := mail.ParseAddress(mailbox)
		if e != nil || a.Address != mailbox {
			http.Error(w, "Enter the shared mailbox email address", 400)
			return
		}
	}
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
func (s *Server) uiMicrosoftFinish(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
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
	a, e := s.Service.Store.ConsumeMicrosoftAttempt(r.Context(), googleStateHash(q.Get("state")), p.AccountID, p.UserID)
	if e != nil {
		http.Error(w, "Connection attempt expired or already used; start again", 400)
		return
	}
	b, e := s.Service.DecryptSecretAAD("microsoft_attempt:"+p.AccountID+":"+a.StateHash, a.EncryptedPending)
	if e != nil {
		http.Error(w, "Connection attempt unavailable", 400)
		return
	}
	rm := s.remoteMailbox()
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
		box, e := s.Service.Store.CreateStandaloneInbox(r.Context(), p.AccountID, store.StandaloneCreate{Address: address, DisplayName: a.DisplayName})
		if e != nil {
			http.Error(w, "Could not create Microsoft inbox; the address may already be connected", 400)
			return
		}
		inbox = box.ID
	}
	c := store.MicrosoftConnection{AccountID: p.AccountID, InboxID: inbox, ClientID: a.ClientID, Tenant: a.Tenant, IdentityID: identity.ID, Mailbox: mailbox, Shared: a.Shared}
	if e = rm.SaveMicrosoftGrant(r.Context(), c, t); e != nil {
		if a.InboxID == "" {
			_, _ = s.Service.Store.PurgeInbox(r.Context(), p.AccountID, inbox)
		}
		http.Error(w, "Microsoft offline mailbox access was not granted; approve mailbox permissions and reconnect", 400)
		return
	}
	rm.ScheduleRefresh(p.AccountID, inbox)
	writeJSON(w, 200, map[string]string{"inbox_id": inbox})
}

const microsoftWizardBody = `<section data-wizard-step="microsoft" hidden><h2>Microsoft / Outlook</h2><p>Connect a whole personal or shared mailbox. Outlook remains the source of truth. Shared access may require tenant-admin consent and separate Send As / Send on Behalf permission.</p><form method="post" action="/ui/oauth/microsoft/begin" data-microsoft-begin><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="inbox_id" data-microsoft-inbox><label>Display name</label><input name="display"><label>Mailbox type</label><select name="mailbox_type"><option value="personal">Personal</option><option value="shared">Shared</option></select><label>Shared mailbox address (Shared only)</label><input name="mailbox" type="email"><div class="dialog-actions"><button type="button" class="secondary" data-wizard-back>Back</button><button>Sign in with Microsoft</button></div></form><p>After approving access, your browser redirects to <code>http://localhost</code>. A connection error is expected: no listener is running on your computer. Copy the complete URL from the address bar and paste it below. If the browser hides the URL, use another desktop browser or start again.</p><form method="post" action="/ui/oauth/microsoft/finish" data-microsoft-finish><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Complete localhost callback URL</label><input name="callback_url" required autocomplete="off"><button>Complete connection</button></form><p data-microsoft-status role="status"></p></section>`
