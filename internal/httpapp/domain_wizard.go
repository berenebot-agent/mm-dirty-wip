package httpapp

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
)

// domainWizardView carries navigation only. Provider forms, validation and
// one-time credential flashes remain owned by the existing domain editors.
type domainWizardView struct {
	Step       string
	Domain     *model.Domain
	Name       string
	Error      string
	BackURL    string
	NextURL    string
	Receiving  string
	Sending    string
	CanInherit bool
}

func domainWizardURL(domainID, step string) string {
	return "/?" + url.Values{"domain": {domainID}, "domain_wizard": {step}}.Encode()
}

// Only the two provider forms may request wizard advancement. Ordinary editor
// submissions keep their existing redirects and behaviour.
func domainWizardSubmission(r *http.Request) bool {
	return r.Form.Get("domain_wizard") == "1" && (r.PathValue("id") != "")
}

func (s *Server) domainWizardAdvance(w http.ResponseWriter, r *http.Request, kind string) bool {
	if !domainWizardSubmission(r) {
		return false
	}
	step := "sending"
	if kind == "sending" {
		step = "summary"
	}
	http.Redirect(w, r, domainWizardURL(r.PathValue("id"), step), http.StatusSeeOther)
	return true
}

func domainWizardForRequest(r *http.Request, domains []model.Domain) *domainWizardView {
	step := r.URL.Query().Get("domain_wizard")
	switch step {
	case "domain", "receiving", "sending", "summary":
	default:
		return nil
	}
	v := &domainWizardView{Step: step}
	id := r.URL.Query().Get("domain")
	if id == "" {
		if step != "domain" {
			return nil
		}
		v.Name = r.URL.Query().Get("domain_name")
		v.Error = r.URL.Query().Get("error")
		return v
	}
	for _, d := range domains {
		if d.ID == id {
			v.Domain = &d
			v.Name = d.Name
			break
		}
	}
	if v.Domain == nil {
		return nil
	}
	for _, parent := range domains {
		if parent.ID == v.Domain.ParentDomainID {
			v.CanInherit = (step == "receiving" && parent.ReceivingProvider != "") || (step == "sending" && parent.SendingProvider != "")
			break
		}
	}
	switch step {
	case "domain":
		v.NextURL = domainWizardURL(id, "receiving")
	case "receiving":
		v.BackURL = domainWizardURL(id, "domain")
		v.NextURL = domainWizardURL(id, "sending")
	case "sending":
		v.BackURL = domainWizardURL(id, "receiving")
		v.NextURL = domainWizardURL(id, "summary")
	case "summary":
		v.BackURL = domainWizardURL(id, "sending")
	}
	return v
}

func domainWizardProviderStatus(provider, label, inheritedFrom string) string {
	if provider == "" {
		return "Not configured — finish setup later"
	}
	if label == "" {
		label = provider
		if t, ok := transport.LookupInbound(provider); ok {
			label = t.Description()
		}
	}
	if inheritedFrom != "" {
		return fmt.Sprintf("%s — inherited from %s", label, inheritedFrom)
	}
	return label + " — configuration saved"
}

const domainWizardDialogs = `<style>
.domain-wizard-progress{font-size:13px;color:#666;border-bottom:1px solid #ddd;padding-bottom:10px}
.domain-wizard-actions{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.domain-wizard-actions .spacer{flex:1}
.domain-wizard-summary dd{margin-bottom:12px}.domain-wizard-summary dt{font-weight:600}
</style>
{{with .DomainWizard}}
<div hidden id="domain-wizard-state" data-step="{{.Step}}" data-domain="{{with .Domain}}{{.ID}}{{end}}" data-back="{{.BackURL}}" data-next="{{.NextURL}}" data-inherit="{{.CanInherit}}"></div>
{{if and .Domain (or (eq .Step "domain") (eq .Step "summary"))}}
<dialog id="domain-wizard-dialog" class="domain-dialog domain-wizard" data-open="1" aria-labelledby="domain-wizard-title">
<h2 id="domain-wizard-title">{{if eq .Step "domain"}}Add domain{{else}}Domain setup summary{{end}} · {{.Name}}</h2>
<p class="domain-wizard-progress">Step {{if eq .Step "domain"}}1{{else}}4{{end}} of 4 · Domain → Receiving → Sending → Summary</p>
<p class="muted small">The domain and saved configuration are kept when you close. You can finish setup from the Domains list later.</p>
{{if eq .Step "domain"}}<label>Domain</label><input value="{{.Name}}" readonly><p class="muted small">This domain has already been created.</p>
{{else}}<dl class="domain-wizard-summary"><dt>Domain</dt><dd>{{.Name}}</dd><dt>Receiving</dt><dd>{{.Receiving}}</dd><dt>Sending</dt><dd>{{.Sending}}</dd></dl>
{{with index $.DialMXSetup .Domain.ID}}<p><b>Receiving checks:</b> {{.LightTitle}}</p>{{range .DNS}}<p class="muted small">{{.Kind}} <code>{{.Name}}</code> — {{if eq .State "ok"}}published and matching{{else}}{{.Reason}}{{end}}</p>{{end}}{{end}}
{{with index $.DomainSendingLight .Domain.ID}}<p><b>Sending DNS:</b> {{.Title}}</p>{{end}}
<p class="muted small">Saved configuration does not by itself confirm delivery. Complete the provider’s DNS and routing instructions; pending DNS does not prevent finishing this wizard.</p>{{end}}
<div class="dialog-actions domain-wizard-actions"><button type="button" class="secondary" data-close-dialog>Close</button>{{if .BackURL}}<a class="btn secondary" href="{{.BackURL}}">Back</a>{{end}}<span class="spacer"></span>{{if eq .Step "domain"}}<a class="btn" href="{{.NextURL}}">Next</a>{{else}}<a class="btn" href="/">Done</a>{{end}}</div>
</dialog>{{end}}{{end}}`
