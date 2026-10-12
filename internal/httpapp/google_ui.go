package httpapp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func googleStateHash(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }

// displayOr returns the first non-blank candidate, trimmed. It resolves an
// inbox label from the operator's optional override, then the provider-supplied
// identity, then the address.
func displayOr(candidates ...string) string {
	for _, c := range candidates {
		if c = strings.TrimSpace(c); c != "" {
			return c
		}
	}
	return ""
}

func googleRandom() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (s *Server) googleCallbackURL() string {
	return strings.TrimRight(s.Service.Config.BaseURL, "/") + "/ui/oauth/google/callback"
}
func validGoogleRedirect(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return u.Scheme == "http" || u.Scheme == "https"
	}
	return u.Scheme == "https" && strings.Contains(h, ".") && net.ParseIP(h) == nil && !strings.HasSuffix(h, ".local")
}
func (s *Server) uiStandaloneWizard(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if r.URL.Query().Get("provider") == "one" {
		http.Redirect(w, r, "/ui/connections/one", http.StatusSeeOther)
		return
	}
	d := pageData{Title: "Connect standalone inbox", Principal: principal(r), CSRF: csrf(r), Notice: r.URL.Query().Get("notice")}
	body := strings.Replace(standaloneWizardBody, `<button type="button" class="secondary" disabled>Microsoft / Outlook — Coming next</button>`, `<label><input type="radio" name="standalone_provider" value="microsoft"> Microsoft / Outlook</label>`, 1)
	body = strings.TrimSuffix(body, "</section>") + microsoftWizardBody + "</section>"
	body = strings.Replace(body, `<div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a>`, `<label><input type="radio" name="standalone_provider" value="one"> One (Easy Gmail Setup)</label><div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a>`, 1)
	if id := r.URL.Query().Get("microsoft_inbox"); id != "" {
		if _, err := s.Service.Store.GetInbox(r.Context(), principal(r), id); err == nil && s.remoteMailbox().IsMicrosoft(r.Context(), principal(r).AccountID, id) {
			status := "Push setup pending; polling fallback."
			if c, err := s.Service.Store.MicrosoftConnection(r.Context(), principal(r).AccountID, id); err == nil && c.Shared {
				status = "Shared delegated mailbox: delta polling (Microsoft does not support delegated shared-mailbox push)."
			} else if push, err := s.Service.Store.MicrosoftPush(r.Context(), principal(r).AccountID, id); err == nil {
				status = push.Status
				if push.Status == "push active" && !push.ExpiresAt.After(time.Now()) {
					status = "Push expired; polling fallback."
				}
			}
			d.Notice = "Microsoft incoming delivery: " + status
		}
	}
	s.render(w, r, body, d)
}
func (s *Server) uiGoogleInfo(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		http.Error(w, "admin required", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"callback_url": s.googleCallbackURL(), "valid": validGoogleRedirect(s.googleCallbackURL())})
}
func (s *Server) uiGoogleBegin(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	id, secret := strings.TrimSpace(r.Form.Get("client_id")), r.Form.Get("client_secret")
	redirect := s.googleCallbackURL()
	if !validGoogleRedirect(redirect) {
		http.Error(w, "Set BASE_URL to your HTTPS hostname or local localhost URL before connecting Google", 400)
		return
	}
	if id == "" || secret == "" || len(id) > 512 || len(secret) > 4096 {
		http.Error(w, "Google client ID and client secret are required", 400)
		return
	}
	inbox := strings.TrimSpace(r.Form.Get("inbox_id"))
	if inbox != "" {
		box, e := s.Service.Store.GetInbox(r.Context(), p, inbox)
		if e != nil || box.Kind != "standalone" || !s.remoteMailbox().IsGoogle(r.Context(), p.AccountID, inbox) || s.Service.Store.IsOne(r.Context(), p.AccountID, inbox) {
			http.Error(w, "Google inbox not found", 404)
			return
		}
	}
	state, verifier := googleRandom(), googleRandom()
	pending, _ := json.Marshal(map[string]string{"secret": secret, "verifier": verifier})
	enc, e := s.Service.EncryptSecretAAD("google_attempt:"+p.AccountID+":"+googleStateHash(state), pending)
	if e != nil {
		http.Error(w, "Could not prepare Google connection", 500)
		return
	}
	a := store.GoogleAttempt{StateHash: googleStateHash(state), AccountID: p.AccountID, UserID: p.UserID, InboxID: inbox, DisplayName: strings.TrimSpace(r.Form.Get("display")), ClientID: id, RedirectURI: redirect, EncryptedPending: enc, ExpiresAt: time.Now().Add(10 * time.Minute)}
	if e = s.Service.Store.SaveGoogleAttempt(r.Context(), a); e != nil {
		http.Error(w, "Could not prepare Google connection", 500)
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	v := url.Values{"client_id": {id}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {gmail.AuthorizationScopes}, "access_type": {"offline"}, "prompt": {"consent select_account"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"authorization_url": "https://accounts.google.com/o/oauth2/v2/auth?" + v.Encode()})
}
func (s *Server) uiGoogleFinish(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	if r.Method == "POST" {
		raw := strings.TrimSpace(r.Form.Get("callback_url"))
		if len(raw) > 8192 {
			http.Error(w, "Invalid callback URL", 400)
			return
		}
		u, e := url.Parse(raw)
		expected, _ := url.Parse(s.googleCallbackURL())
		if e != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.User != nil || u.Fragment != "" {
			http.Error(w, "Paste the complete registered MailMoose callback URL", 400)
			return
		}
		q = u.Query()
	}
	if q.Get("error") == "access_denied" {
		http.Error(w, "Google denied access. If the OAuth app is In production it must complete Google verification; for personal Gmail keep it in Testing and add your account as a test user, then connect again", 400)
		return
	}
	if len(q["state"]) != 1 || len(q["code"]) != 1 || q.Get("error") != "" || q.Get("code") == "" {
		http.Error(w, "Google authorization was declined or incomplete; start again", 400)
		return
	}
	a, e := s.Service.Store.ConsumeGoogleAttempt(r.Context(), googleStateHash(q.Get("state")), p.AccountID, p.UserID)
	if e != nil {
		http.Error(w, "Google connection attempt expired or was already used; start again", 400)
		return
	}
	if a.RedirectURI != s.googleCallbackURL() {
		http.Error(w, "MailMoose callback configuration changed; start again", 400)
		return
	}
	b, e := s.Service.DecryptSecretAAD("google_attempt:"+p.AccountID+":"+a.StateHash, a.EncryptedPending)
	if e != nil {
		http.Error(w, "Connection attempt unavailable; start again", 400)
		return
	}
	var pending map[string]string
	if json.Unmarshal(b, &pending) != nil {
		http.Error(w, "Connection attempt unavailable", 400)
		return
	}
	rm := s.remoteMailbox()
	token, e := rm.Google.Exchange(r.Context(), a.ClientID, pending["secret"], a.RedirectURI, q.Get("code"), pending["verifier"])
	if e != nil {
		http.Error(w, "Google authorization failed; check app credentials and connect again", 400)
		return
	}
	profile, e := rm.Google.Profile(r.Context(), token.AccessToken)
	if e != nil {
		http.Error(w, "Could not verify Google mailbox; check Gmail API permissions", 400)
		return
	}
	// Basic identity is optional: absent permission or an unavailable profile
	// must not prevent connecting a mailbox with valid Gmail permission.
	identity, _ := rm.Google.Identity(r.Context(), token.AccessToken)
	inbox := a.InboxID
	if inbox != "" {
		box, e := s.Service.Store.GetInbox(r.Context(), p, inbox)
		if e != nil || !strings.EqualFold(box.Address, profile.EmailAddress) {
			http.Error(w, "Choose the Google account already connected to this inbox", 400)
			return
		}
	} else {
		box, e := s.Service.Store.CreateStandaloneInbox(r.Context(), p.AccountID, store.StandaloneCreate{Address: profile.EmailAddress, DisplayName: displayOr(a.DisplayName, identity.Name, profile.EmailAddress)})
		if e != nil {
			http.Error(w, "Could not create Google inbox; the address may already be connected", 400)
			return
		}
		inbox = box.ID
	}
	if e = rm.SaveGoogleGrant(r.Context(), p.AccountID, inbox, a.ClientID, pending["secret"], token); e != nil {
		if a.InboxID == "" {
			_, _ = s.Service.Store.PurgeInbox(r.Context(), p.AccountID, inbox)
		}
		http.Error(w, "Google offline mailbox access was not granted; connect again and approve mailbox access", 400)
		return
	}
	if a.InboxID == "" {
		_ = s.Service.Store.SaveGoogleDetection(r.Context(), inbox, profile.HistoryID)
	} else if name := strings.TrimSpace(identity.Name); name != "" {
		if e = s.Service.Store.UpgradeInboxDisplayName(r.Context(), p.AccountID, inbox, name); e != nil {
			http.Error(w, "Google connected, but could not update inbox display name", 500)
			return
		}
	}
	rm.ScheduleRefresh(p.AccountID, inbox)
	if r.Method == "POST" {
		writeJSON(w, 200, map[string]string{"inbox_id": inbox})
		return
	}
	http.Redirect(w, r, "/ui/inboxes/"+inbox+"?notice="+url.QueryEscape("Google inbox connected. Metadata indexing has started."), 303)
}

const standaloneWizardBody = `<section class="card" data-standalone-wizard><h1>Connect standalone inbox</h1><p class="muted">Connect an existing mailbox. Your provider remains the source of truth.</p>
<section data-wizard-step="provider"><h2>Choose provider</h2><label><input type="radio" name="standalone_provider" value="imap" checked> IMAP + SMTP</label><label><input type="radio" name="standalone_provider" value="google"> Google / Gmail</label><button type="button" class="secondary" disabled>Microsoft / Outlook — Coming next</button><div class="dialog-actions"><a class="btn secondary" href="/">Cancel</a><button type="button" data-wizard-next>Next</button></div></section>
<section data-wizard-step="imap" hidden><h2>IMAP + SMTP</h2><form method="post" action="/ui/inboxes/standalone"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Display name</label><input name="display"><label>Email address</label><input name="address" type="email" required><label>IMAP host</label><input name="host" required placeholder="imap.example.com"><label>Port</label><input name="port" type="number" value="993" min="1" max="65535"><label>Username</label><input name="username" required><label>Password or app password</label><input name="imap_password" type="password" required autocomplete="new-password"><label>Security</label><select name="security"><option value="tls">TLS</option><option value="starttls">STARTTLS</option><option value="plain">Plain</option></select><label>Sync root</label><input name="namespace" placeholder="INBOX"><details><summary>Outbound SMTP (optional)</summary><label>SMTP host</label><input name="smtp_host"><label>Port</label><input name="smtp_port" type="number" value="465"><label>Username</label><input name="smtp_username"><label>Password</label><input name="smtp_password" type="password" autocomplete="new-password"><label>Security</label><select name="smtp_security"><option value="tls">TLS</option><option value="starttls">STARTTLS</option><option value="plain">Plain</option></select></details><div class="dialog-actions"><button type="button" class="secondary" data-wizard-back>Back</button><button>Connect IMAP inbox</button></div></form></section>
<section data-wizard-step="google" hidden><h2 class="wizard-step-title"><span class="wizard-logo" aria-hidden="true"><svg viewBox="0 0 48 48"><path fill="#EA4335" d="M24 9.5c3.54 0 6.71 1.22 9.21 3.6l6.85-6.85C35.9 2.38 30.47 0 24 0 14.62 0 6.51 5.38 2.56 13.22l7.98 6.19C12.43 13.72 17.74 9.5 24 9.5z"/><path fill="#4285F4" d="M46.98 24.55c0-1.57-.15-3.09-.38-4.55H24v9.02h12.94c-.58 2.96-2.26 5.48-4.78 7.18l7.73 6c4.51-4.18 7.09-10.36 7.09-17.65z"/><path fill="#FBBC05" d="M10.53 28.59c-.48-1.45-.76-2.99-.76-4.59s.27-3.14.76-4.59l-7.98-6.19C.92 16.46 0 20.12 0 24c0 3.88.92 7.54 2.56 10.78l7.97-6.19z"/><path fill="#34A853" d="M24 48c6.48 0 11.93-2.13 15.89-5.81l-7.73-6c-2.15 1.45-4.92 2.3-8.16 2.3-6.26 0-11.57-4.22-13.47-9.91l-7.98 6.19C6.51 42.62 14.62 48 24 48z"/></svg></span>Google / Gmail</h2><p class="muted">Each Gmail inbox connects through your own Google Cloud OAuth client. The Google mailbox stays the source of truth.</p><ol class="steps"><li>Open <a href="https://console.cloud.google.com/" target="_blank" rel="noopener noreferrer">Google Cloud Console</a> and sign in with the account you are connecting. Create a project (or select one) and keep it selected.</li><li>Open the <a href="https://console.cloud.google.com/apis/library/gmail.googleapis.com" target="_blank" rel="noopener noreferrer">Gmail API</a> and click <b>Enable</b>.</li><li>Open <a href="https://console.cloud.google.com/auth/overview" target="_blank" rel="noopener noreferrer">Google Auth Platform</a>. If you see <b>Get started</b>, click it, then set: <b>App name</b> (any name, e.g. MailMoose); <b>User support email</b> and <b>Contact email</b> (your Google address); and <b>Audience</b> — choose <b>External</b> for a normal <code>@gmail.com</code> account (<b>Internal</b> is only available to Google Workspace organisations). Accept the policy and finish.</li><li>Keep the app in <b>Testing</b> and add your account as a test user. Open <a href="https://console.cloud.google.com/auth/audience" target="_blank" rel="noopener noreferrer">Audience</a> → <b>Test users</b> → <b>Add users</b> and add the exact address you are connecting. Do <b>not</b> click <b>Publish app</b>: for a restricted Gmail scope, an unverified External app in production is blocked outright.</li><li>Open <a href="https://console.cloud.google.com/auth/scopes" target="_blank" rel="noopener noreferrer">Data Access</a> → <b>Add or remove scopes</b> and add <code>https://www.googleapis.com/auth/gmail.modify</code>, <code>openid</code>, and <code>https://www.googleapis.com/auth/userinfo.profile</code> (use manual scope entry if a scope is not listed), then save. The Gmail scope covers reading, labels, drafts and sending — no separate <code>gmail.send</code> scope is needed — and does not grant permanent deletion. Basic profile access supplies your Google account name for the inbox label; if it is unavailable, the email address is used instead.</li><li>Open <a href="https://console.cloud.google.com/auth/clients" target="_blank" rel="noopener noreferrer">Clients</a> → <b>Create client</b>, choose <b>Web application</b>, and leave <b>Authorised JavaScript origins</b> empty. Under <b>Authorised redirect URIs</b> add this exact URL, then create the client and copy its ID and secret:<input data-google-callback readonly aria-label="Google callback URL"><button type="button" class="secondary" data-google-copy>Copy callback URL</button></li><li>Paste the client ID and client secret into the form below and click <b>Connect Google</b>, then select the mailbox account and approve access. Google shows a <b>"Google hasn't verified this app"</b> screen — click <b>Advanced → Go to &lt;app name&gt;</b> to continue.</li></ol><ul class="muted setup-notes"><li>The browser returns directly to this MailMoose installation, so Google never needs inbound access. The callback URL is built from <code>BASE_URL</code> and must be an HTTPS hostname or a localhost/loopback URL on the browser's computer; raw LAN IPs, <code>.local</code> names and plain HTTP hosts are not registerable.</li><li>An External app in <b>Testing</b> issues Gmail refresh tokens that expire after seven days; reconnect from the inbox settings when that happens. Publishing does not remove this — an unverified app in production is blocked outright, and only Google verification (which includes a security assessment) lifts the limit. For personal use, stay in Testing.</li><li>If the return page cannot load after you approve access, use <b>Callback failed? Paste the return URL</b> below.</li></ul>
<form class="wizard-box" data-google-begin method="post" action="/ui/oauth/google/begin" autocomplete="off"><h3 class="section-head">Connect your Google app</h3><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="inbox_id" data-google-inbox><label>Client ID</label><input name="client_id" required><label>Client secret</label><input name="client_secret" type="password" required autocomplete="new-password"><div class="dialog-actions"><button type="button" class="secondary" data-wizard-back>Back</button><button>Connect Google</button></div><div data-google-status role="status" aria-live="polite"></div></form>
<p class="muted small">Access covers reading, labels, drafts and sending. Permanent deletion is handled in Gmail. Gmail UI delegation and Google Groups are not included.</p><details data-google-fallback><summary>Callback failed? Paste the return URL</summary><p>If the return page cannot load after approving access, copy the <b>complete final URL</b> from the browser address bar and paste it here. It must start with the registered callback URL above. This does not bypass Google's redirect URL rules.</p><form data-google-finish method="post" action="/ui/oauth/google/finish"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Complete callback URL</label><input name="callback_url" type="url" required autocomplete="off"><button>Finish connection</button></form></details></section></section>`
