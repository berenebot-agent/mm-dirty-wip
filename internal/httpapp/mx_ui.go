package httpapp

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/store"
)

// mxFormView is the rendered shape of the domain MX receiver form. It holds the
// non-secret values an operator may edit, resolved from the persisted settings
// and overlaid with the non-secret values of a failed submission so a validation
// error never discards the operator's input. The private STARTTLS key is never
// part of this view and is never rendered back into the form.
type mxFormView struct {
	Mode string
	URL  string
	// Included SMTP controls. Empty string means "use the receiver default" and
	// is rendered as an empty input with a placeholder.
	Hostname            string
	MaxMessageBytes     string
	MaxStagingBytes     string
	MaxRecipients       string
	MaxConnections      string
	DNSResolver         string
	DNSTimeoutSeconds   string
	ReadTimeoutSeconds  string
	WriteTimeoutSeconds string
	DataTimeoutSeconds  string
	// RequireTLS is a checkbox defaulting off; Verify* default on.
	RequireTLS  bool
	VerifySPF   bool
	VerifyDKIM  bool
	VerifyDMARC bool
	// SMTPTLSCert is the public certificate PEM; it is safe to render back. The
	// private key is never included here.
	SMTPTLSCert string
	// SMTPTLSKeyConfigured drives the "leave blank to keep" hint.
	SMTPTLSKeyConfigured bool
	// KeyConfigured is the remote bearer key presence, for the same hint.
	KeyConfigured bool
	CA            string
	Revision      int64
	// Error is the user-safe validation message from a failed submission.
	Error string
}

// newMXFormView builds the form from the persisted settings.
func newMXFormView(settings app.MXReceiverSettings) mxFormView {
	return mxFormView{
		Mode:                 settings.Mode,
		URL:                  settings.URL,
		Hostname:             settings.Hostname,
		MaxMessageBytes:      intString(settings.MaxMessageBytes),
		MaxStagingBytes:      intString(settings.MaxStagingBytes),
		MaxRecipients:        intString(int64(settings.MaxRecipients)),
		MaxConnections:       intString(int64(settings.MaxConnections)),
		DNSResolver:          settings.DNSResolver,
		DNSTimeoutSeconds:    intString(int64(settings.DNSTimeoutSeconds)),
		ReadTimeoutSeconds:   intString(int64(settings.ReadTimeoutSeconds)),
		WriteTimeoutSeconds:  intString(int64(settings.WriteTimeoutSeconds)),
		DataTimeoutSeconds:   intString(int64(settings.DataTimeoutSeconds)),
		RequireTLS:           settings.RequireTLSEnabled(),
		VerifySPF:            settings.VerifySPFEnabled(),
		VerifyDKIM:           settings.VerifyDKIMEnabled(),
		VerifyDMARC:          settings.VerifyDMARCEnabled(),
		SMTPTLSCert:          settings.SMTPTLSCert,
		SMTPTLSKeyConfigured: settings.SMTPTLSKeyConfigured,
		KeyConfigured:        settings.KeyConfigured,
		CA:                   settings.CA,
		Revision:             settings.Revision,
	}
}

// intString renders a non-zero integer, or "" for zero so the input shows its
// placeholder ("use the default") rather than a literal 0.
func intString(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// mxFormFlash carries a failed domain MX submission from the POST that rejected
// it to the GET that re-renders the form. Only non-secret values are kept: the
// private STARTTLS key is never stored here. It is bound to the user so a stale
// or foreign flash can neither be shown nor consumed.
type mxFormFlash struct {
	UserID   string
	DomainID string
	Error    string
	Values   map[string]string
	Checked  map[string]bool
}

// mxReceiverEditorView keeps the shared receiver form scoped to its originating
// domain. Only system administrators receive this view.
type mxReceiverEditorView struct {
	DomainID            string
	CSRF                string
	MXForm              mxFormView
	MXStatus            app.MXReceiverStatus
	MXIncludedSupported bool
}

// mxIncludedFields is the Included receiver's editable surface. It is shared
// by the per-domain Direct MX editor and the installation-wide receiver form,
// so the two can never drift. It reads only .MXForm, which both views carry.
const mxIncludedFields = `<div><label>SMTP greeting hostname (optional)</label><input name="hostname" value="{{.MXForm.Hostname}}" placeholder="mail.example.com">
<p class="muted small">Defaults to the external receiving URL hostname, or the public URL hostname. You can change it.</p>
<details{{if .MXForm.Error}} open{{end}}><summary>Limits</summary>
<div class="row"><div style="flex:1"><label>Max message bytes</label><input name="max_message_bytes" type="number" min="0" step="1" value="{{.MXForm.MaxMessageBytes}}" placeholder="31457280"></div><div style="flex:1"><label>Max staging bytes</label><input name="max_staging_bytes" type="number" min="0" step="1" value="{{.MXForm.MaxStagingBytes}}" placeholder="268435456"></div></div>
<div class="row"><div style="flex:1"><label>Max recipients</label><input name="max_recipients" type="number" min="0" step="1" value="{{.MXForm.MaxRecipients}}" placeholder="100"></div><div style="flex:1"><label>Max connections</label><input name="max_connections" type="number" min="0" step="1" value="{{.MXForm.MaxConnections}}" placeholder="256"></div></div>
<p class="muted small">Leave a limit blank or 0 to use the receiver default (shown as the placeholder). Values apply to the Included receiver.</p>
</details><details{{if .MXForm.Error}} open{{end}}><summary>Verification</summary>
<label class="inherit-option"><input type="checkbox" name="verify_spf" value="true"{{if .MXForm.VerifySPF}} checked{{end}}> <span>Verify SPF</span></label>
<label class="inherit-option"><input type="checkbox" name="verify_dkim" value="true"{{if .MXForm.VerifyDKIM}} checked{{end}}> <span>Verify DKIM</span></label>
<label class="inherit-option"><input type="checkbox" name="verify_dmarc" value="true"{{if .MXForm.VerifyDMARC}} checked{{end}}> <span>Verify DMARC</span></label>
<p class="muted small">Authentication evidence is computed at the included receiver. Verification defaults on; clearing a box saves it off. An unauthenticated message is delivered as Spam, never rejected.</p>
</details><details{{if .MXForm.Error}} open{{end}}><summary>Network &amp; timeouts</summary>
<label>DNS resolver (optional, host:port)</label><input name="dns_resolver" value="{{.MXForm.DNSResolver}}" placeholder="system resolver">
<div class="row"><div style="flex:1"><label>DNS timeout (s)</label><input name="dns_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.DNSTimeoutSeconds}}" placeholder="10"></div><div style="flex:1"><label>Read timeout (s)</label><input name="read_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.ReadTimeoutSeconds}}" placeholder="60"></div></div>
<div class="row"><div style="flex:1"><label>Write timeout (s)</label><input name="write_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.WriteTimeoutSeconds}}" placeholder="60"></div><div style="flex:1"><label>Data timeout (s)</label><input name="data_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.DataTimeoutSeconds}}" placeholder="300"></div></div>
</details><details{{if .MXForm.Error}} open{{end}}><summary>STARTTLS</summary>
<label class="inherit-option"><input type="checkbox" name="require_tls" value="true"{{if .MXForm.RequireTLS}} checked{{end}}> <span>Require STARTTLS (refuse plaintext SMTP; needs a certificate below)</span></label>
<p class="muted small">Optional certificate pair the receiver offers to senders. Set both to enable STARTTLS; clear the certificate to remove the pair (clearing the certificate also clears the stored private key).</p>
<label>Certificate (PEM, public)</label><textarea name="smtp_tls_cert" rows="4" placeholder="-----BEGIN CERTIFICATE-----">{{.MXForm.SMTPTLSCert}}</textarea>
<label>Private key (PEM)</label><textarea name="smtp_tls_key" rows="4" autocomplete="off" placeholder="{{if .MXForm.SMTPTLSKeyConfigured}}Leave blank to keep the stored private key{{else}}-----BEGIN PRIVATE KEY-----{{end}}"></textarea>
<p class="muted small">The private key is never shown again. A blank field keeps the stored key.</p></details></div>`

// mxReceiverSection is the per-domain Direct MX receiver editor. It edits the
// installation-wide shared receiver, but only its Included mode: a domain
// never chooses where the shared receiver lives.
const mxReceiverSection = `<div class="mx-editor">
<p class="muted small">Shared receiver · changes apply to all Direct MX domains.</p>
{{if not .MXIncludedSupported}}<div class="error">The Included receiver is unavailable in this deployment: the container must start as root to launch the isolated receiver child.</div>{{end}}
{{if .MXForm.Error}}<div class="error">{{.MXForm.Error}}</div>{{end}}
<input type="hidden" name="revision" value="{{.MXForm.Revision}}">
<input type="hidden" name="mode" value="included">
<p class="muted small"><b>Included receiver</b> · runs inside this deployment with automatically generated credentials.</p>
{{if eq .MXStatus.Mode "remote"}}<p class="muted small">Saving switches the shared receiver from Remote to Included.</p>{{end}}
{{template "mx-included-fields" .}}
<p class="muted small">Receiver settings are stored in the database and managed here; they are authoritative and legacy MX environment variables are no longer read at startup.</p>
</div>`

// mxInstallSection is the installation-wide receiver form on the Admin page.
// It is where the shared receiver is added: the one bundled with this
// deployment, or one the operator runs themselves. The choice is not a
// per-domain setting — every Direct MX domain points at this one receiver —
// so it lives with the other installation settings.
//
// The two field sets are rendered together and the selected mode decides which
// one is saved, exactly as the API decides it: the server reads only the
// fields of the submitted mode. The blocks carry data-mx-mode so the client
// can show the selected one and disable the other.
const mxInstallSection = `<div class="mx-editor" data-mx-mode-form>
{{if not .MXIncludedSupported}}<div class="error">The Included receiver is unavailable in this deployment: the container must start as root to launch the isolated receiver child. Choose <b>Remote</b> and run the receiver in its own container or on another host.</div>{{end}}
{{if .MXForm.Error}}<div class="error">{{.MXForm.Error}}</div>{{end}}
{{if .MXStatus.Configured}}<p>Current: <span class="pill">{{if eq .MXStatus.Mode "included"}}Included{{else if eq .MXStatus.Mode "remote"}}Remote{{else}}{{.MXStatus.Mode}}{{end}}</span> <span class="pill{{if eq .MXStatus.State "active"}} ok{{else}} amber{{end}}">{{.MXStatus.State}}</span>{{if .MXStatus.SMTPAddr}} <span class="muted small">SMTP listener: <code>{{.MXStatus.SMTPAddr}}</code> · session: <code>{{.MXStatus.SessionAddr}}</code></span>{{end}}{{if .MXStatus.Detail}} <span class="muted small">{{.MXStatus.Detail}}</span>{{end}}</p>
{{else}}<p class="muted">No receiver configured. Domains set to Direct MX will reject mail until one is configured.</p>{{end}}
<form method="post" action="/ui/admin/mx" autocomplete="off">
<input type="hidden" name="_csrf" value="{{.CSRF}}">
<input type="hidden" name="revision" value="{{.MXForm.Revision}}">
<fieldset style="border:1px solid #ddd;border-radius:8px;padding:8px 12px;margin:4px 0 10px"><legend class="muted">Receiver</legend>
<label style="display:flex;align-items:flex-start;gap:8px{{if not .MXIncludedSupported}};opacity:.55{{end}}"><input type="radio" name="mode" value="included"{{if not .MXIncludedSupported}} disabled{{end}}{{if eq .MXForm.Mode "included"}} checked{{end}} style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span><b>Included</b> <span class="muted small">— the receiver runs inside this deployment, with automatically generated credentials. Publish port 25 to reach it.</span></span></label>
<label style="display:flex;align-items:flex-start;gap:8px"><input type="radio" name="mode" value="remote"{{if eq .MXForm.Mode "remote"}} checked{{end}} style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span><b>Remote</b> <span class="muted small">— the receiver runs separately, in its own container or on another host. You supply its URL and bearer key.</span></span></label>
</fieldset>
<div data-mx-mode="included"{{if ne .MXForm.Mode "included"}} hidden{{end}}>
{{template "mx-included-fields" .}}
</div>
<div data-mx-mode="remote"{{if ne .MXForm.Mode "remote"}} hidden{{end}}>
<label>Receiver URL</label><input name="url" value="{{.MXForm.URL}}" placeholder="https://mx.example.com">
<p class="muted small">The URL MailMoose connects to outbound. Use the origin of your standalone receiver, running with <code>DIALMX_MODE=single</code>.</p>
<label>Bearer key{{if .MXForm.KeyConfigured}} <span class="muted small">(leave blank to keep the stored key)</span>{{end}}</label>
<input name="bearer_key" type="password" autocomplete="new-password" placeholder="{{if .MXForm.KeyConfigured}}unchanged{{else}}the receiver's DIALMX_CORE_KEY{{end}}">
<p class="muted small">This must match <code>DIALMX_CORE_KEY</code> on the receiver. The saved key is never displayed.</p>
<label>Private CA certificate bundle (PEM, optional)</label>
<textarea name="ca" rows="3" placeholder="-----BEGIN CERTIFICATE-----">{{.MXForm.CA}}</textarea>
<p class="muted small">Only needed when the receiver's TLS certificate is signed by a CA this server does not already trust. This certificate verifies TLS; the bearer key authenticates the connection.</p>
</div>
<div class="dialog-actions"><button type="submit">Save receiver</button></div>
</form>
{{if .MXStatus.Configured}}<form method="post" action="/ui/admin/mx/clear" data-confirm="Clear the MX receiver? Domains set to Direct MX will stop accepting mail."><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="revision" value="{{.MXForm.Revision}}"><button class="secondary danger" type="submit">Clear receiver</button></form>{{end}}
<p class="muted small">Receiver settings are stored in the database and managed here; they are authoritative and legacy MX environment variables are no longer read at startup.</p>
</div>`

// ---------------------------------------------------------------------------
// Installation receiver (Admin page). The shared receiver is added here: the
// one bundled with this deployment, or one the operator runs themselves. The
// choice is installation-wide, not per domain: every Direct MX domain points
// at this one receiver, so only a system administrator may change it.
// ---------------------------------------------------------------------------

// uiInstallMXSave saves the installation-wide MX receiver from the Admin page.
// It validates through the same service path the API uses, so the form and the
// API cannot diverge.
func (s *Server) uiInstallMXSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("revision")), 10, 64)
	mode := strings.ToLower(strings.TrimSpace(r.Form.Get("mode")))
	if mode != app.MXModeIncluded && mode != app.MXModeRemote {
		http.Error(w, "choose the Included or the Remote receiver", http.StatusBadRequest)
		return
	}
	in := app.MXReceiverInput{Mode: mode, Revision: rev}
	// The selected mode decides which fields are read, exactly as the API
	// decides it, so a value left in the other block can never fail the save.
	switch mode {
	case app.MXModeIncluded:
		in = mxIncludedInput(r, in)
	case app.MXModeRemote:
		in.URL = strings.TrimSpace(r.Form.Get("url"))
		in.BearerKey = strings.TrimSpace(r.Form.Get("bearer_key"))
		in.CA = strings.TrimSpace(r.Form.Get("ca"))
	}
	if _, err := s.Service.SaveMXReceiverSettings(r.Context(), p, in); err != nil {
		s.installMXError(w, r, err, mode)
		return
	}
	http.Redirect(w, r, "/admin?notice=MX+receiver+saved", http.StatusSeeOther)
}

// uiInstallMXClear removes the configured installation receiver. The revision
// travels in the form, so the clear is CAS-protected like the save.
func (s *Server) uiInstallMXClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("revision")), 10, 64)
	if err := s.Service.ClearMXReceiverSettings(r.Context(), p, rev); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			http.Redirect(w, r, "/admin?"+url.Values{"mx_error": {"The MX receiver configuration changed in another session. Reload and try again."}}.Encode(), http.StatusSeeOther)
		case errors.Is(err, store.ErrForbidden):
			http.Error(w, "system administrator required", 403)
		default:
			s.Log.Error("MX receiver clear failed", "error", err)
			http.Error(w, "could not clear the MX receiver", 500)
		}
		return
	}
	http.Redirect(w, r, "/admin?notice=MX+receiver+cleared", http.StatusSeeOther)
}

// installMXError maps an installation receiver save failure back to the Admin
// page. A validation fault is carried as a user-bound flash holding the
// non-secret submitted values, so the form is repopulated with the operator's
// input and the chosen mode; the bearer key and the private STARTTLS key are
// deliberately excluded.
func (s *Server) installMXError(w http.ResponseWriter, r *http.Request, err error, mode string) {
	p := principal(r)
	switch {
	case errors.Is(err, app.ErrMXInvalidInput):
		f := mxFormFlash{UserID: p.UserID, Error: mxUserMessage(err), Values: mxInstallSubmittedValues(r, mode)}
		if mode == app.MXModeIncluded {
			f.Checked = mxSubmittedChecks(r)
		}
		dest := "/admin"
		if tok := s.putFlash(w, r, f, mxFlashSize(f)); tok != "" {
			dest += "?" + url.Values{"_flash": {tok}}.Encode()
		} else {
			dest += "?" + url.Values{"mx_error": {f.Error}}.Encode()
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	case errors.Is(err, store.ErrConflict):
		http.Redirect(w, r, "/admin?"+url.Values{"mx_error": {"The MX receiver configuration changed in another session. Reload and try again."}}.Encode(), http.StatusSeeOther)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "system administrator required", 403)
	default:
		s.Log.Error("MX receiver save failed", "error", err)
		http.Error(w, "could not save the MX receiver", 500)
	}
}

// mxInstallSubmittedValues collects the non-secret form values of a failed
// installation save: the chosen mode plus the fields of that mode only, so a
// failed Remote save cannot blank the Included fields in the re-rendered form.
// The bearer key and the private STARTTLS key are never collected.
func mxInstallSubmittedValues(r *http.Request, mode string) map[string]string {
	values := map[string]string{"mode": mode}
	if mode == app.MXModeRemote {
		for _, name := range []string{"url", "ca"} {
			values[name] = strings.TrimSpace(r.Form.Get(name))
		}
		return values
	}
	for _, name := range []string{
		"hostname", "max_message_bytes", "max_staging_bytes", "max_recipients", "max_connections",
		"dns_resolver", "dns_timeout_seconds", "read_timeout_seconds", "write_timeout_seconds", "data_timeout_seconds",
		"smtp_tls_cert",
	} {
		values[name] = strings.TrimSpace(r.Form.Get(name))
	}
	return values
}

// uiAdminMXSave handles the domain MX form submission. It validates through the
// same service path the API uses, so the form and the API cannot diverge.
func (s *Server) uiAdminMXSave(w http.ResponseWriter, r *http.Request) {
	if !s.saveIncludedMXForm(w, r) {
		return
	}
	http.Redirect(w, r, mxDomainDestination(r)+"&notice=MX+receiver+saved", http.StatusSeeOther)
}

// saveIncludedMXForm saves the shared receiver before the domain opts into it.
// On validation/conflict failure the domain selection is left untouched.
func (s *Server) saveIncludedMXForm(w http.ResponseWriter, r *http.Request) bool {
	p := principal(r)
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return false
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("revision")), 10, 64)
	if !s.mxDomainAllowed(w, r) {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(r.Form.Get("mode")))
	if mode == "" {
		mode = app.MXModeIncluded
	}
	if mode != app.MXModeIncluded {
		http.Error(w, "Direct MX uses the Included receiver", http.StatusBadRequest)
		return false
	}
	in := app.MXReceiverInput{Mode: mode, Revision: rev}
	// Only Included fields are accepted by the domain editor. The Remote
	// receiver is chosen on the installation's Admin page.
	in = mxIncludedInput(r, in)
	if _, err := s.Service.SaveMXReceiverSettings(r.Context(), p, in); err != nil {
		s.mxAdminError(w, r, err)
		return false
	}
	return true
}

// mxIncludedInput fills the Included receiver's fields from a submitted form.
// It is shared by the domain editor and the installation receiver form, so
// both send exactly the same Included configuration to the service.
func mxIncludedInput(r *http.Request, in app.MXReceiverInput) app.MXReceiverInput {
	in.Hostname = r.Form.Get("hostname")
	in.MaxMessageBytes = parseNonNegativeInt(r.Form.Get("max_message_bytes"))
	in.MaxStagingBytes = parseNonNegativeInt(r.Form.Get("max_staging_bytes"))
	in.MaxRecipients = int(parseNonNegativeInt(r.Form.Get("max_recipients")))
	in.MaxConnections = int(parseNonNegativeInt(r.Form.Get("max_connections")))
	in.DNSResolver = r.Form.Get("dns_resolver")
	in.DNSTimeoutSeconds = int(parseNonNegativeInt(r.Form.Get("dns_timeout_seconds")))
	in.ReadTimeoutSeconds = int(parseNonNegativeInt(r.Form.Get("read_timeout_seconds")))
	in.WriteTimeoutSeconds = int(parseNonNegativeInt(r.Form.Get("write_timeout_seconds")))
	in.DataTimeoutSeconds = int(parseNonNegativeInt(r.Form.Get("data_timeout_seconds")))
	in.SMTPTLSCert = r.Form.Get("smtp_tls_cert")
	in.SMTPTLSKey = r.Form.Get("smtp_tls_key")
	// Checkboxes default checked where noted; an unchecked box is an explicit
	// false, not "unset".
	in.RequireTLS = checkboxBool(r.Form, "require_tls")
	in.VerifySPF = checkboxBool(r.Form, "verify_spf")
	in.VerifyDKIM = checkboxBool(r.Form, "verify_dkim")
	in.VerifyDMARC = checkboxBool(r.Form, "verify_dmarc")
	return in
}

// uiAdminMXClear removes the configured receiver. The revision travels in the
// form so the clear is CAS-protected like the save.
func (s *Server) uiAdminMXClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("revision")), 10, 64)
	if !s.mxDomainAllowed(w, r) {
		return
	}
	if err := s.Service.ClearMXReceiverSettings(r.Context(), p, rev); err != nil {
		s.mxAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, mxDomainDestination(r)+"&notice=MX+receiver+cleared", http.StatusSeeOther)
}

func (s *Server) mxDomainAllowed(w http.ResponseWriter, r *http.Request) bool {
	if _, err := s.Service.Store.GetDomain(r.Context(), principal(r).AccountID, r.PathValue("id")); err != nil {
		http.Error(w, "domain not found", http.StatusNotFound)
		return false
	}
	return true
}

func mxDomainDestination(r *http.Request) string {
	return "/?" + url.Values{"domain": {r.PathValue("id")}, "kind": {"receiving"}, "provider": {"mx"}}.Encode()
}

// mxAdminError maps a save/clear failure to the originating domain. A validation fault
// is stored as a user-bound flash carrying the non-secret submitted values so
// the form is repopulated with the operator's input; the private STARTTLS key
// is deliberately excluded. A CAS conflict reloads the persisted form; internal
// faults are logged and redacted.
func (s *Server) mxAdminError(w http.ResponseWriter, r *http.Request, err error) {
	p := principal(r)
	switch {
	case errors.Is(err, app.ErrMXInvalidInput):
		dest := mxDomainDestination(r) + "&"
		f := mxFormFlash{UserID: p.UserID, DomainID: r.PathValue("id"), Error: mxUserMessage(err), Values: mxSubmittedValues(r), Checked: mxSubmittedChecks(r)}
		if tok := s.putFlash(w, r, f, mxFlashSize(f)); tok != "" {
			dest += url.Values{"_flash": {tok}}.Encode()
		} else {
			dest += url.Values{"error": {f.Error}}.Encode()
		}

		http.Redirect(w, r, dest, http.StatusSeeOther)
	case errors.Is(err, store.ErrConflict):
		http.Redirect(w, r, mxDomainDestination(r)+"&"+url.Values{"error": {"The MX receiver configuration changed in another session. Reload and try again."}}.Encode(), http.StatusSeeOther)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "system administrator required", 403)
	default:
		s.Log.Error("MX receiver save failed", "error", err)
		http.Error(w, "could not save MX receiver", 500)
	}
}

// mxSubmittedValues collects the non-secret form values for a failed save. The
// private STARTTLS key and the remote bearer key are excluded so neither can be
// echoed.
func mxSubmittedValues(r *http.Request) map[string]string {
	fields := []string{
		"mode", "url", "ca", "hostname",
		"max_message_bytes", "max_staging_bytes", "max_recipients", "max_connections",
		"dns_resolver", "dns_timeout_seconds", "read_timeout_seconds", "write_timeout_seconds", "data_timeout_seconds",
		"smtp_tls_cert",
	}
	values := map[string]string{}
	for _, name := range fields {
		values[name] = strings.TrimSpace(r.Form.Get(name))
	}
	return values
}

// mxSubmittedChecks records the checkbox states for a failed save so they are
// preserved on the re-rendered form.
func mxSubmittedChecks(r *http.Request) map[string]bool {
	checks := map[string]bool{}
	for _, name := range []string{"require_tls", "verify_spf", "verify_dkim", "verify_dmarc"} {
		if v := checkboxBool(r.Form, name); v != nil && *v {
			checks[name] = true
		}
	}
	return checks
}

// mxFlashSize bounds the flash payload so an oversized submission is dropped
// rather than evicting unrelated flashes.
func mxFlashSize(f mxFormFlash) int {
	size := len(f.Error) + 64
	for k, v := range f.Values {
		size += len(k) + len(v) + 8
	}
	return size
}

// takeMXFormFlash consumes a bound form flash for this user, if present. The
// token is only consumed when it belongs to the caller, so a stale or foreign
// flash is neither shown nor destroyed.
func (s *Server) takeMXFormFlash(r *http.Request, tok string, userID, domainID string) (mxFormFlash, bool) {
	if tok == "" {
		return mxFormFlash{}, false
	}
	v, ok := s.peekFlash(r, tok)
	if !ok {
		return mxFormFlash{}, false
	}
	f, ok := v.(mxFormFlash)
	if !ok || f.UserID != userID || f.DomainID != domainID {
		return mxFormFlash{}, false
	}
	if taken, ok := s.takeFlash(r, tok); ok {
		if tf, ok := taken.(mxFormFlash); ok && tf.UserID == userID && tf.DomainID == domainID {
			return tf, true
		}
	}
	return mxFormFlash{}, false
}

// applyMXFlash overlays a failed submission's non-secret values onto the form
// so the operator sees their input again. The private STARTTLS key is never
// overlaid (it is never stored in the flash).
func applyMXFlash(form *mxFormView, f mxFormFlash) {
	if f.Error != "" {
		form.Error = f.Error
	}
	set := func(dst *string, name string) {
		if v, ok := f.Values[name]; ok {
			*dst = v
		}
	}
	set(&form.Mode, "mode")
	set(&form.URL, "url")
	set(&form.CA, "ca")
	set(&form.Hostname, "hostname")
	set(&form.MaxMessageBytes, "max_message_bytes")
	set(&form.MaxStagingBytes, "max_staging_bytes")
	set(&form.MaxRecipients, "max_recipients")
	set(&form.MaxConnections, "max_connections")
	set(&form.DNSResolver, "dns_resolver")
	set(&form.DNSTimeoutSeconds, "dns_timeout_seconds")
	set(&form.ReadTimeoutSeconds, "read_timeout_seconds")
	set(&form.WriteTimeoutSeconds, "write_timeout_seconds")
	set(&form.DataTimeoutSeconds, "data_timeout_seconds")
	set(&form.SMTPTLSCert, "smtp_tls_cert")
	if f.Checked != nil {
		form.RequireTLS = f.Checked["require_tls"]
		form.VerifySPF = f.Checked["verify_spf"]
		form.VerifyDKIM = f.Checked["verify_dkim"]
		form.VerifyDMARC = f.Checked["verify_dmarc"]
	}
}

// mxUserMessage strips the sentinel prefix from a validation error so the form
// shows the operator-facing reason only.
func mxUserMessage(err error) string {
	msg := err.Error()
	return strings.TrimSpace(strings.TrimPrefix(msg, app.ErrMXInvalidInput.Error()+":"))
}

// checkboxBool resolves a rendered checkbox to an explicit tri-state pointer. A
// checked box submits "true"; an unchecked box submits nothing, which is an
// explicit false (the operator cleared a check that may default on). It always
// returns a non-nil pointer so a save records the operator's choice rather than
// leaving the field at its default.
func checkboxBool(form map[string][]string, name string) *bool {
	v := false
	for _, raw := range form[name] {
		if strings.EqualFold(strings.TrimSpace(raw), "true") || raw == "1" || raw == "on" {
			v = true
		}
	}
	return &v
}

// parseNonNegativeInt parses a form integer, treating blank or invalid input as
// zero (the service validates negativity, but a blank field means "use the
// default").
func parseNonNegativeInt(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}
