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

// mxFormView is the rendered shape of the /admin MX receiver form. It holds the
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

// mxFormFlash carries a failed /admin MX submission from the POST that rejected
// it to the GET that re-renders the form. Only non-secret values are kept: the
// private STARTTLS key is never stored here. It is bound to the user so a stale
// or foreign flash can neither be shown nor consumed.
type mxFormFlash struct {
	UserID  string
	Error   string
	Values  map[string]string
	Checked map[string]bool
}

// mxAdminSection is the system administrator's installation-wide MX receiver
// panel, appended to the /admin page. It offers three receiver choices:
//
//   - Auto: not yet implemented, shown disabled as "coming soon" so the
//     intended default is visible without pretending it works;
//   - Included: the embedded receiver child (auto credentials, port forward and
//     DNS records handled by the container) with the full advanced SMTP
//     controls the child supports;
//   - Remote: a separate receiver on this host, another LAN host or a remote
//     URL, authenticated with a bearer key.
//
// The bearer key and the private STARTTLS key are never echoed: a blank field
// retains the stored secret and the inputs are always rendered empty, including
// after a validation error.
const mxAdminSection = `<section class="card" id="mx-settings"><h2>MX receiver</h2>
<p class="muted">Installation-wide direct-SMTP (MX) receiver used by every domain that selects MX receiving. This is a system setting: one receiver serves all accounts.</p>
{{if not .MXIncludedSupported}}<div class="error">The embedded (Included) receiver is unavailable in this deployment: the container did not start as root, so the isolated receiver child cannot be launched. Choose <b>Remote</b> and run the receiver in its own container or on another host.</div>{{end}}
{{if .MXForm.Error}}<div class="error">{{.MXForm.Error}}</div>{{end}}
{{if .MXStatus.Configured}}<p>Current: <span class="pill">{{if eq .MXStatus.Mode "included"}}Included{{else if eq .MXStatus.Mode "remote"}}Remote{{else}}{{.MXStatus.Mode}}{{end}}</span> <span class="pill{{if eq .MXStatus.State "active"}} ok{{else}} amber{{end}}">{{if eq .MXStatus.State "connecting"}}connecting…{{else}}{{.MXStatus.State}}{{end}}</span> <span class="muted small">{{if eq .MXStatus.State "active"}}receiver connected{{else if eq .MXStatus.State "connecting"}}not ready yet{{end}}</span>{{if .MXStatus.Detail}} <span class="muted small">{{.MXStatus.Detail}}</span>{{end}}</p>{{if .MXStatus.SMTPAddr}}<p class="muted small">SMTP listener: <code>{{.MXStatus.SMTPAddr}}</code> · session: <code>{{.MXStatus.SessionAddr}}</code></p>{{end}}{{else}}<p class="muted">No receiver configured. Domains set to MX receiving will reject mail until one is configured.</p>{{end}}
<form method="post" action="/ui/admin/mx"><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="revision" value="{{.MXForm.Revision}}">
<fieldset style="border:1px solid #ddd;border-radius:8px;padding:8px 12px;margin:4px 0 10px"><legend class="muted">Receiver</legend>
<label style="display:flex;align-items:flex-start;gap:8px;opacity:.55"><input type="radio" name="mode" value="auto" disabled style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span><b>Auto</b> <span class="muted small">— choose the best receiver automatically. Coming soon.</span></span></label>
<label style="display:flex;align-items:flex-start;gap:8px{{if not .MXIncludedSupported}};opacity:.55{{end}}"><input type="radio" name="mode" value="included"{{if not .MXIncludedSupported}} disabled{{end}}{{if eq .MXForm.Mode "included"}} checked{{end}} style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span><b>Included</b> <span class="muted small">— run the receiver inside this deployment. Credentials are generated automatically and the port forward and DNS records are handled for you.</span></span></label>
<label style="display:flex;align-items:flex-start;gap:8px"><input type="radio" name="mode" value="remote"{{if eq .MXForm.Mode "remote"}} checked{{end}} style="width:auto;margin:2px 0 0;flex:0 0 auto"> <span><b>Remote</b> <span class="muted small">— connect to a receiver running in its own container, on another host, or on the LAN. You supply its URL and bearer key.</span></span></label>
</fieldset>
<div id="mx-included-fields"><label>SMTP greeting hostname (optional)</label><input name="hostname" value="{{.MXForm.Hostname}}" placeholder="mail.example.com">
<div class="row"><div style="flex:1"><label>Max message bytes</label><input name="max_message_bytes" type="number" min="0" step="1" value="{{.MXForm.MaxMessageBytes}}" placeholder="31457280"></div><div style="flex:1"><label>Max staging bytes</label><input name="max_staging_bytes" type="number" min="0" step="1" value="{{.MXForm.MaxStagingBytes}}" placeholder="268435456"></div></div>
<div class="row"><div style="flex:1"><label>Max recipients</label><input name="max_recipients" type="number" min="0" step="1" value="{{.MXForm.MaxRecipients}}" placeholder="100"></div><div style="flex:1"><label>Max connections</label><input name="max_connections" type="number" min="0" step="1" value="{{.MXForm.MaxConnections}}" placeholder="256"></div></div>
<p class="muted small">Leave a limit blank or 0 to use the receiver default (shown as the placeholder). Values apply to the Included receiver.</p>
<label class="inherit-option"><input type="checkbox" name="require_tls" value="true"{{if .MXForm.RequireTLS}} checked{{end}}> <span>Require STARTTLS (refuse plaintext SMTP; needs a certificate below)</span></label>
<label class="inherit-option"><input type="checkbox" name="verify_spf" value="true"{{if .MXForm.VerifySPF}} checked{{end}}> <span>Verify SPF</span></label>
<label class="inherit-option"><input type="checkbox" name="verify_dkim" value="true"{{if .MXForm.VerifyDKIM}} checked{{end}}> <span>Verify DKIM</span></label>
<label class="inherit-option"><input type="checkbox" name="verify_dmarc" value="true"{{if .MXForm.VerifyDMARC}} checked{{end}}> <span>Verify DMARC</span></label>
<p class="muted small">Authentication evidence is computed at the included receiver. Verification defaults on; clearing a box saves it off. An unauthenticated message is delivered as Spam, never rejected.</p>
<label>DNS resolver (optional, host:port)</label><input name="dns_resolver" value="{{.MXForm.DNSResolver}}" placeholder="system resolver">
<div class="row"><div style="flex:1"><label>DNS timeout (s)</label><input name="dns_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.DNSTimeoutSeconds}}" placeholder="10"></div><div style="flex:1"><label>Read timeout (s)</label><input name="read_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.ReadTimeoutSeconds}}" placeholder="60"></div></div>
<div class="row"><div style="flex:1"><label>Write timeout (s)</label><input name="write_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.WriteTimeoutSeconds}}" placeholder="60"></div><div style="flex:1"><label>Data timeout (s)</label><input name="data_timeout_seconds" type="number" min="0" step="1" value="{{.MXForm.DataTimeoutSeconds}}" placeholder="300"></div></div>
<h3 class="section-head">SMTP TLS (STARTTLS)</h3>
<p class="muted small">Optional certificate pair the receiver offers to senders. Set both to enable STARTTLS; clear the certificate to remove the pair (clearing the certificate also clears the stored private key).</p>
<label>Certificate (PEM, public)</label><textarea name="smtp_tls_cert" rows="4" placeholder="-----BEGIN CERTIFICATE-----">{{.MXForm.SMTPTLSCert}}</textarea>
<label>Private key (PEM)</label><textarea name="smtp_tls_key" rows="4" autocomplete="off" placeholder="{{if .MXForm.SMTPTLSKeyConfigured}}Leave blank to keep the stored private key{{else}}-----BEGIN PRIVATE KEY-----{{end}}"></textarea>
<p class="muted small">The private key is never shown again. A blank field keeps the stored key. This field is never re-displayed, even when another field fails validation.</p></div>
<div id="mx-remote-fields"><label>Receiver URL</label><input name="url" value="{{.MXForm.URL}}" placeholder="https://receiver.example:8443"><label>Bearer key</label><input name="bearer_key" type="password" autocomplete="new-password" placeholder="{{if .MXForm.KeyConfigured}}Leave blank to keep the stored key{{else}}Required{{end}}"><p class="muted small">The key is never shown again. A blank field keeps the stored key.</p><label>Private CA certificate (PEM, optional)</label><textarea name="ca" rows="3" placeholder="Only for a receiver with a private CA">{{.MXForm.CA}}</textarea></div>
<div class="dialog-actions"><button>Save receiver</button></div></form>
{{if .MXStatus.Configured}}<form method="post" action="/ui/admin/mx/clear" data-confirm="Clear the MX receiver? Domains set to MX receiving will stop accepting mail."><input type="hidden" name="_csrf" value="{{.CSRF}}"><input type="hidden" name="revision" value="{{.MXForm.Revision}}"><button class="secondary danger">Clear receiver</button></form>{{end}}
<p class="muted small">Existing <code>MX_ENABLE</code>/<code>MX_RECEIVER_URL</code>/<code>DIALMX_CORE_KEY</code> and the legacy <code>MX_*</code> SMTP settings are imported once on first start; after that these settings are authoritative and the environment is ignored.</p>
</section>`

// uiAdminMXSave handles the /admin MX form submission. It validates through the
// same service path the API uses, so the form and the API cannot diverge.
func (s *Server) uiAdminMXSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.SystemAdmin {
		http.Error(w, "system administrator required", 403)
		return
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("revision")), 10, 64)
	mode := strings.ToLower(strings.TrimSpace(r.Form.Get("mode")))
	in := app.MXReceiverInput{Mode: mode, Revision: rev}
	// The form renders both the included and remote blocks at once, so only the
	// fields that apply to the selected mode are forwarded. The service rejects
	// a remote save that carries included-only fields (and vice versa), which is
	// what keeps the API strict; blanking the inapplicable block here is what
	// lets an operator switch modes without a stale value failing the save.
	switch mode {
	case app.MXModeIncluded:
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
		// Checkboxes default checked where noted; an unchecked box is an
		// explicit false, not "unset".
		in.RequireTLS = checkboxBool(r.Form, "require_tls")
		in.VerifySPF = checkboxBool(r.Form, "verify_spf")
		in.VerifyDKIM = checkboxBool(r.Form, "verify_dkim")
		in.VerifyDMARC = checkboxBool(r.Form, "verify_dmarc")
	case app.MXModeRemote:
		in.URL = r.Form.Get("url")
		in.BearerKey = r.Form.Get("bearer_key")
		in.CA = r.Form.Get("ca")
	}
	if _, err := s.Service.SaveMXReceiverSettings(r.Context(), p, in); err != nil {
		s.mxAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin?notice=MX+receiver+saved", http.StatusSeeOther)
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
	if err := s.Service.ClearMXReceiverSettings(r.Context(), p, rev); err != nil {
		s.mxAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin?notice=MX+receiver+cleared", http.StatusSeeOther)
}

// mxAdminError maps a save/clear failure to the /admin page. A validation fault
// is stored as a user-bound flash carrying the non-secret submitted values so
// the form is repopulated with the operator's input; the private STARTTLS key
// is deliberately excluded. A CAS conflict reloads the persisted form; internal
// faults are logged and redacted.
func (s *Server) mxAdminError(w http.ResponseWriter, r *http.Request, err error) {
	p := principal(r)
	switch {
	case errors.Is(err, app.ErrMXInvalidInput):
		dest := "/admin?"
		f := mxFormFlash{UserID: p.UserID, Error: mxUserMessage(err), Values: mxSubmittedValues(r), Checked: mxSubmittedChecks(r)}
		if tok := s.flashes.put(f, mxFlashSize(f)); tok != "" {
			dest += url.Values{"_flash": {tok}}.Encode()
		} else {
			dest += url.Values{"error": {f.Error}}.Encode()
		}

		http.Redirect(w, r, dest, http.StatusSeeOther)
	case errors.Is(err, store.ErrConflict):
		http.Redirect(w, r, "/admin?"+url.Values{"error": {"The MX receiver configuration changed in another session. Reload and try again."}}.Encode(), http.StatusSeeOther)
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
		if v := strings.TrimSpace(r.Form.Get(name)); v != "" {
			values[name] = v
		}
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
func (s *Server) takeMXFormFlash(tok string, userID string) (mxFormFlash, bool) {
	if tok == "" {
		return mxFormFlash{}, false
	}
	v, ok := s.flashes.peek(tok)
	if !ok {
		return mxFormFlash{}, false
	}
	f, ok := v.(mxFormFlash)
	if !ok || f.UserID != userID {
		return mxFormFlash{}, false
	}
	if taken, ok := s.flashes.take(tok); ok {
		if tf, ok := taken.(mxFormFlash); ok && tf.UserID == userID {
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
