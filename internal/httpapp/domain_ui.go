package httpapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

// domainConfigFieldView is one non-secret provider value shown as a read-only
// summary. Secret values are never placed in a view.
type domainConfigFieldView struct {
	Label string
	Value string
}

// domainSummaryView is the read-only state of one optional domain config slot.
type domainSummaryView struct {
	Configured    bool
	Provider      string
	ProviderLabel string
	Fields        []domainConfigFieldView
	UpdatedAt     time.Time
	WebhookURL    string
	Steps         []string
	CanRegenerate bool
	Warning       string
	LastActivity  string
}

// domainProviderChoiceView is one selectable provider in a picker.
type domainProviderChoiceView struct {
	Name        string
	Description string
}

// domainEditorView drives the save form for a chosen provider. Non-secret
// values are prefilled from the stored same-provider config or the schema
// defaults; secret inputs are always truly empty and generated fields are not
// rendered at all.
type domainEditorView struct {
	Kind          string // "sending" | "receiving"
	Provider      string
	ProviderLabel string
	Fields        []transport.ConfigField
	WebhookURL    string
	Steps         []string
	Generated     bool
	// KeepSecrets is true when the stored config already uses this provider, so
	// a blank secret retains the stored value ("leave blank to keep"). It is
	// false for a create or provider switch, where required secrets must be
	// supplied.
	KeepSecrets bool
	// Values holds only non-secret field values to prefill the inputs/selects.
	Values map[string]string
}

// domainWorkerFlash carries freshly generated Cloudflare Worker code (which
// contains the one-time shared secret) from the POST that generated it to the
// domain page. It is bound to the account, user, domain, config id and revision
// so a stale or foreign flash can never be displayed.
type domainWorkerFlash struct {
	AccountID  string
	UserID     string
	DomainID   string
	ConfigID   string
	Revision   int64
	WorkerCode string
	WebhookURL string
}

// domainNoticeFlash carries a user-safe validation error from a failed save
// back to the domain page (Post/Redirect/Get). It is bound to the account, user
// and domain so a stale or foreign flash can neither be shown nor consumed. It
// also carries the submitted non-secret values so a failed save does not lose
// the operator's input; secret fields are never stored or reflected.
type domainNoticeFlash struct {
	AccountID string
	UserID    string
	DomainID  string
	Kind      string
	Provider  string
	Error     string
	Values    map[string]string
}

// domainDetail renders the domain-first configuration page. It is the single
// place a domain's optional sending and receiving slots are managed.
func (s *Server) domainDetail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	boxes, _ := s.Service.Store.ListInboxes(ctx, p)
	own := make([]model.Inbox, 0, len(boxes))
	for _, b := range boxes {
		if b.DomainID == d.ID {
			own = append(own, b)
		}
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	lastSent, _ := s.Service.Store.LastSentByDomain(ctx, p.AccountID)
	lastRecv, _ := s.Service.Store.LastReceivedByDomain(ctx, p.AccountID)

	data := pageData{
		Title:                  d.Name,
		Tab:                    "settings",
		Principal:              p,
		CSRF:                   csrf(r),
		Account:                acc,
		Domain:                 &d,
		DomainInboxes:          own,
		DomainSendingChoices:   domainSendingChoices(),
		DomainReceivingChoices: domainReceivingChoices(),
		DomainSendingSummary:   s.domainSendingSummary(ctx, p.AccountID, d, lastSent[d.ID]),
		DomainReceivingSummary: s.domainReceivingSummary(ctx, p.AccountID, d, lastRecv[d.ID]),
		Notice:                 r.URL.Query().Get("notice"),
	}

	// A flash is peeked first and only consumed when its account/user/domain
	// binding matches this request, so a wrong principal or domain can neither
	// read the one-time generated secret nor destroy the real owner's view. The
	// value is then re-claimed with take and displayed only if take returned the
	// matching value, so two concurrent authorized GETs cannot both render the
	// same one-time Worker code.
	var noticeKind, noticeProvider string
	var noticeValues map[string]string
	if tok := r.URL.Query().Get("_flash"); tok != "" {
		if v, ok := s.flashes.peek(tok); ok {
			switch f := v.(type) {
			case domainWorkerFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == d.ID {
					if rc, err := s.Service.Store.GetDomainReceivingConfig(ctx, p.AccountID, d.ID); err == nil && rc.ID == f.ConfigID && rc.Revision == f.Revision {
						if taken, ok := s.flashes.take(tok); ok {
							if tf, ok := taken.(domainWorkerFlash); ok && tf.WorkerCode == f.WorkerCode && tf.ConfigID == f.ConfigID && tf.Revision == f.Revision && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == d.ID {
								data.DomainWorkerCode = tf.WorkerCode
								data.DomainWorkerWebhook = tf.WebhookURL
							}
						}
					}
				}
			case domainNoticeFlash:
				if f.AccountID == p.AccountID && f.UserID == p.UserID && f.DomainID == d.ID {
					if taken, ok := s.flashes.take(tok); ok {
						if tf, ok := taken.(domainNoticeFlash); ok && tf.AccountID == p.AccountID && tf.UserID == p.UserID && tf.DomainID == d.ID && tf.Error == f.Error {
							data.Error = tf.Error
							noticeKind, noticeProvider, noticeValues = tf.Kind, tf.Provider, tf.Values
						}
					}
				}
			}
		}
	}

	if sel := normalizeDomainProvider(r.URL.Query().Get("sending")); sel != "" {
		if e, ok := newDomainEditor("sending", sel, s.Service.Config.BaseURL); ok {
			e.Values, e.KeepSecrets = s.sendingEditorState(ctx, p.AccountID, d.ID, sel, e.Fields)
			if noticeKind == "sending" && noticeProvider == sel {
				overlayValues(e.Values, noticeValues)
			}
			data.DomainSendingEditor = e
			data.DomainQuerySending = sel
		} else {
			data.Error = "Unknown sending provider"
		}
	}
	if sel := normalizeDomainProvider(r.URL.Query().Get("receiving")); sel != "" {
		if e, ok := newDomainEditor("receiving", sel, s.Service.Config.BaseURL); ok {
			e.Values, e.KeepSecrets = s.receivingEditorState(ctx, p.AccountID, d.ID, sel, e.Fields)
			if noticeKind == "receiving" && noticeProvider == sel {
				overlayValues(e.Values, noticeValues)
			}
			data.DomainReceivingEditor = e
			data.DomainQueryReceiving = sel
		} else {
			data.Error = "Unknown receiving provider"
		}
	}
	// Preserve the other selection across a single picker submit.
	if data.DomainSendingEditor != nil && data.DomainQueryReceiving == "" {
		data.DomainQueryReceiving = normalizeDomainProvider(r.URL.Query().Get("receiving"))
	}
	if data.DomainReceivingEditor != nil && data.DomainQuerySending == "" {
		data.DomainQuerySending = normalizeDomainProvider(r.URL.Query().Get("sending"))
	}

	w.Header().Set("Cache-Control", "no-store")
	s.render(w, domainBody, data)
}

// uiDomainCatchAll stores the domain's catch-all inbox. Only an inbox that
// belongs to the same domain (and account) may be selected.
func (s *Server) uiDomainCatchAll(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	inboxID := strings.TrimSpace(r.Form.Get("inbox"))
	if inboxID != "" {
		box, err := s.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
		if err != nil || box.DomainID != d.ID {
			http.Error(w, "catch-all inbox must belong to this domain", 400)
			return
		}
	}
	if err := s.Service.Store.SetDomainCatchAll(ctx, p.AccountID, d.ID, inboxID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "domain not found", 404)
			return
		}
		http.Error(w, err.Error(), 400)
		return
	}
	s.domainNotice(w, r, d.ID, "Catch-all inbox updated")
}

// uiDomainSending creates, replaces or updates the domain's single sending
// configuration. The provider is always taken from the submitted form, so any
// provider (including a switch from another provider) can replace it.
func (s *Server) uiDomainSending(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	provider := normalizeDomainProvider(r.Form.Get("provider"))
	fields, ok := outboundSchemaFields(provider)
	if !ok {
		s.domainEditorError(w, r, d.ID, "sending", provider, "Unknown sending provider")
		return
	}
	cfg, err := configFromForm(provider, fields, r)
	if err != nil {
		s.domainEditorError(w, r, d.ID, "sending", provider, err.Error())
		return
	}
	if _, err := s.Service.SaveDomainSendingConfig(ctx, p.AccountID, d.ID, provider, cfg); err != nil {
		s.domainSaveError(w, r, d.ID, "sending", provider, err)
		return
	}
	s.domainNotice(w, r, d.ID, "Sending configuration saved")
}

// uiDomainSendingClear removes the domain's sending configuration. Mail for the
// domain then queues until a provider is configured again.
func (s *Server) uiDomainSendingClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteDomainSendingConfig(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "domain not found", 404)
			return
		}
		http.Error(w, err.Error(), 400)
		return
	}
	s.domainNotice(w, r, r.PathValue("id"), "Sending configuration removed")
}

// uiDomainReceiving creates, replaces or updates the domain's single receiving
// configuration. A generated provider secret (Cloudflare) is minted only when
// missing and returned once as Worker code.
func (s *Server) uiDomainReceiving(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	provider := normalizeDomainProvider(r.Form.Get("provider"))
	t, ok := transport.LookupInbound(provider)
	if !ok {
		s.domainEditorError(w, r, d.ID, "receiving", provider, "Unknown receiving provider")
		return
	}
	cfg, err := configFromForm(provider, t.ConfigFields(), r)
	if err != nil {
		s.domainEditorError(w, r, d.ID, "receiving", provider, err.Error())
		return
	}
	saved, generated, err := s.Service.SaveDomainReceivingConfig(ctx, p.AccountID, d.ID, provider, cfg, false)
	if err != nil {
		s.domainSaveError(w, r, d.ID, "receiving", provider, err)
		return
	}
	if secret := generated["webhook_secret"]; secret != "" {
		s.flashDomainWorker(w, r, p, d.ID, saved, secret)
		return
	}
	s.domainNotice(w, r, d.ID, "Receiving configuration saved")
}

// uiDomainReceivingClear removes the domain's receiving configuration.
func (s *Server) uiDomainReceivingClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if err := s.Service.Store.DeleteDomainReceivingConfig(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "domain not found", 404)
			return
		}
		http.Error(w, err.Error(), 400)
		return
	}
	s.domainNotice(w, r, r.PathValue("id"), "Receiving configuration removed")
}

// uiDomainReceivingRegenerate mints a fresh generated secret for the currently
// configured provider and returns the new one-time Worker code. It is only
// offered for providers that actually have a generated field.
func (s *Server) uiDomainReceivingRegenerate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	cfg, err := s.Service.Store.GetDomainReceivingConfig(ctx, p.AccountID, d.ID)
	if err != nil {
		http.Error(w, "no receiving configuration to regenerate", 400)
		return
	}
	t, ok := transport.LookupInbound(cfg.Provider)
	if !ok {
		http.Error(w, "unknown receiving provider", 400)
		return
	}
	hasGenerated := false
	for _, f := range t.ConfigFields() {
		if f.Generated {
			hasGenerated = true
			break
		}
	}
	if !hasGenerated {
		http.Error(w, "this provider has no generated secret", 400)
		return
	}
	saved, generated, err := s.Service.SaveDomainReceivingConfig(ctx, p.AccountID, d.ID, cfg.Provider, nil, true)
	if err != nil {
		s.domainSaveError(w, r, d.ID, "receiving", cfg.Provider, err)
		return
	}
	if secret := generated["webhook_secret"]; secret != "" {
		s.flashDomainWorker(w, r, p, d.ID, saved, secret)
		return
	}
	s.domainNotice(w, r, d.ID, "Receiving secret regenerated")
}

// domainDeliveries shows the send history owned by one domain, independent of
// whichever provider config currently exists.
func (s *Server) domainDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	ctx := r.Context()
	d, err := s.Service.Store.GetDomain(ctx, p.AccountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "domain not found", 404)
		return
	}
	before := int64(0)
	if v := r.URL.Query().Get("before"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	attempts, err := s.Service.Store.ListDomainDeliveryAttempts(ctx, p.AccountID, d.ID, 51, before)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	hasMore := len(attempts) > 50
	if hasMore {
		attempts = attempts[:50]
	}
	nextBefore := int64(0)
	if len(attempts) > 0 {
		nextBefore = attempts[len(attempts)-1].ID
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, domainDeliveriesBody, pageData{
		Title:            d.Name + " · Delivery history",
		Tab:              "settings",
		Principal:        p,
		CSRF:             csrf(r),
		Account:          acc,
		Domain:           &d,
		DeliveryAttempts: attempts,
		DeliveryHasMore:  hasMore,
		DeliveryBefore:   nextBefore,
	})
}

// domainNotice stores a success notice and redirects to the domain page.
func (s *Server) domainNotice(w http.ResponseWriter, r *http.Request, domainID, notice string) {
	dest := "/ui/domains/" + url.PathEscape(domainID) + "?" + url.Values{"notice": {notice}}.Encode()
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// domainEditorError reopens the relevant provider editor with a user-safe
// validation message. The flash is bound to this account/user/domain and keeps
// only non-secret submitted values, so no secret value is ever included and a
// foreign viewer cannot consume it.
func (s *Server) domainEditorError(w http.ResponseWriter, r *http.Request, domainID, kind, provider, msg string) {
	p := principal(r)
	q := url.Values{}
	if kind == "sending" {
		q.Set("sending", provider)
	} else {
		q.Set("receiving", provider)
	}
	values := submittedEditorValues(kind, provider, r)
	size := len(msg) + 32
	for k, v := range values {
		size += len(k) + len(v) + 8
	}
	// Keep the flash bounded: if the retained non-secret input would be
	// unreasonably large, drop it and keep the error message.
	if size > 4096 {
		values = nil
		size = len(msg) + 32
	}
	f := domainNoticeFlash{AccountID: p.AccountID, UserID: p.UserID, DomainID: domainID, Kind: kind, Provider: provider, Error: msg, Values: values}
	if tok := s.flashes.put(f, size); tok != "" {
		q.Set("_flash", tok)
	}
	http.Redirect(w, r, "/ui/domains/"+url.PathEscape(domainID)+"?"+q.Encode(), http.StatusSeeOther)
}

// submittedEditorValues collects the non-secret schema values the operator
// submitted for a provider editor, keyed by field name, so a validation error
// can repopulate them. Secret and generated fields are deliberately excluded,
// and each value is length-capped.
func submittedEditorValues(kind, provider string, r *http.Request) map[string]string {
	var fields []transport.ConfigField
	if kind == "sending" {
		f, ok := outboundSchemaFields(provider)
		if !ok {
			return nil
		}
		fields = f
	} else {
		t, ok := transport.LookupInbound(provider)
		if !ok {
			return nil
		}
		fields = t.ConfigFields()
	}
	values := map[string]string{}
	for _, f := range fields {
		if f.Secret || f.Generated {
			continue
		}
		v := strings.TrimSpace(r.Form.Get("cfg_" + provider + "_" + f.Name))
		if v == "" {
			continue
		}
		if len(v) > 256 {
			v = v[:256]
		}
		values[f.Name] = v
	}
	return values
}

// domainSaveError maps a store/service failure to a UI response. Validation
// faults show their user-safe message; internal faults are logged and redacted.
func (s *Server) domainSaveError(w http.ResponseWriter, r *http.Request, domainID, kind, provider string, err error) {
	switch {
	case errors.Is(err, app.ErrInvalidConfig), errors.Is(err, transport.ErrUnknownProvider):
		s.domainEditorError(w, r, domainID, kind, provider, err.Error())
	case errors.Is(err, store.ErrConflict):
		s.domainEditorError(w, r, domainID, kind, provider, "This configuration changed in another session. Reload and try again.")
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "domain not found", 404)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "forbidden", 403)
	default:
		s.Log.Error("domain config save failed", "kind", kind, "provider", provider, "error", err)
		http.Error(w, "could not save configuration", 500)
	}
}

// flashDomainWorker renders the Cloudflare Worker template from a freshly
// generated secret and stores it as a bound one-time flash. Only the generated
// Worker code is plaintext; no entered secret is ever flashed.
func (s *Server) flashDomainWorker(w http.ResponseWriter, r *http.Request, p model.Principal, domainID string, cfg store.DomainReceivingConfig, secret string) {
	base := s.Service.Config.BaseURL
	code := s.cloudflareWorkerCode(base, secret)
	f := domainWorkerFlash{
		AccountID:  p.AccountID,
		UserID:     p.UserID,
		DomainID:   domainID,
		ConfigID:   cfg.ID,
		Revision:   cfg.Revision,
		WorkerCode: code,
		WebhookURL: strings.TrimRight(base, "/") + "/internal/ingest/cloudflare",
	}
	dest := "/ui/domains/" + url.PathEscape(domainID)
	if tok := s.flashes.put(f, len(code)+128); tok != "" {
		dest += "?_flash=" + tok
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func normalizeDomainProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// outboundSchemaFields returns the configuration schema for an outbound
// provider, or false when the provider is unknown or has no schema.
func outboundSchemaFields(provider string) ([]transport.ConfigField, bool) {
	t, ok := transport.LookupOutbound(provider)
	if !ok {
		return nil, false
	}
	fp, ok := t.(transport.ConfigSchemaProvider)
	if !ok {
		return nil, false
	}
	return fp.ConfigFields(), true
}

// configFromForm reads the `cfg_<field>` inputs for a provider schema. Number
// fields are converted to int so the service can validate them; a non-numeric
// value is a user-safe validation error rather than a silently dropped field
// (which would otherwise apply the provider default). Unknown form keys are
// ignored and blank fields are omitted so same-provider secret retention and
// provider defaults apply.
func configFromForm(provider string, fields []transport.ConfigField, r *http.Request) (map[string]any, error) {
	cfg := map[string]any{}
	for _, f := range fields {
		if f.Generated {
			continue
		}
		raw := strings.TrimSpace(r.Form.Get("cfg_" + provider + "_" + f.Name))
		if raw == "" {
			continue
		}
		if f.Type == "number" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, fmt.Errorf("%s must be a whole number", f.Label)
			}
			cfg[f.Name] = n
			continue
		}
		cfg[f.Name] = raw
	}
	return cfg, nil
}

func (s *Server) domainSendingSummary(ctx context.Context, accountID string, d model.Domain, lastActivity time.Time) domainSummaryView {
	v := domainSummaryView{LastActivity: domainActivityLabel(lastActivity)}
	cfg, err := s.Service.Store.GetDomainSendingConfig(ctx, accountID, d.ID)
	if err != nil {
		return v
	}
	v.Configured = true
	v.Provider = cfg.Provider
	v.ProviderLabel = cfg.Provider
	v.UpdatedAt = cfg.UpdatedAt
	if t, ok := transport.LookupOutbound(cfg.Provider); ok {
		v.ProviderLabel = t.Description()
		if fp, ok := t.(transport.ConfigSchemaProvider); ok {
			if dec, err := s.Service.DecryptDomainSendingConfig(cfg); err == nil {
				v.Fields = domainPublicFields(fp.ConfigFields(), dec)
			}
		}
	}
	return v
}

func (s *Server) domainReceivingSummary(ctx context.Context, accountID string, d model.Domain, lastActivity time.Time) domainSummaryView {
	v := domainSummaryView{LastActivity: domainActivityLabel(lastActivity)}
	cfg, err := s.Service.Store.GetDomainReceivingConfig(ctx, accountID, d.ID)
	if err != nil {
		return v
	}
	v.Configured = true
	v.Provider = cfg.Provider
	v.ProviderLabel = cfg.Provider
	v.UpdatedAt = cfg.UpdatedAt
	if t, ok := transport.LookupInbound(cfg.Provider); ok {
		v.ProviderLabel = t.Description()
		v.WebhookURL = domainIngestURL(s.Service.Config.BaseURL, t)
		v.Steps = domainReceivingSteps(cfg.Provider)
		for _, f := range t.ConfigFields() {
			if f.Generated {
				v.CanRegenerate = true
			}
		}
		if dec, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
			v.Fields = domainPublicFields(t.ConfigFields(), dec)
		}
		if cfg.Provider == "cloudflare" {
			v.Warning = privateHostWarning(v.WebhookURL)
		}
	}
	return v
}

// domainPublicFields keeps only non-secret, present schema values for display.
func domainPublicFields(fields []transport.ConfigField, cfg map[string]any) []domainConfigFieldView {
	out := []domainConfigFieldView{}
	for _, f := range fields {
		if f.Secret {
			continue
		}
		val, ok := cfg[f.Name]
		if !ok || val == nil {
			continue
		}
		out = append(out, domainConfigFieldView{Label: f.Label, Value: fmt.Sprintf("%v", val)})
	}
	return out
}

func domainSendingChoices() []domainProviderChoiceView {
	out := []domainProviderChoiceView{}
	for _, t := range transport.ListOutbound() {
		out = append(out, domainProviderChoiceView{Name: t.Name(), Description: t.Description()})
	}
	return out
}

func domainReceivingChoices() []domainProviderChoiceView {
	out := []domainProviderChoiceView{}
	for _, t := range transport.ListInbound() {
		out = append(out, domainProviderChoiceView{Name: t.Name(), Description: t.Description()})
	}
	return out
}

func newDomainEditor(kind, provider, baseURL string) (*domainEditorView, bool) {
	if kind == "sending" {
		t, ok := transport.LookupOutbound(provider)
		if !ok {
			return nil, false
		}
		fields, ok := outboundSchemaFields(provider)
		if !ok {
			return nil, false
		}
		return &domainEditorView{Kind: kind, Provider: provider, ProviderLabel: t.Description(), Fields: fields}, true
	}
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return nil, false
	}
	e := &domainEditorView{Kind: kind, Provider: provider, ProviderLabel: t.Description(), Fields: t.ConfigFields(), WebhookURL: domainIngestURL(baseURL, t), Steps: domainReceivingSteps(provider)}
	for _, f := range e.Fields {
		if f.Generated {
			e.Generated = true
		}
	}
	return e, true
}

// sendingEditorState returns the non-secret values to prefill a sending editor
// and whether the stored config already uses this provider, so a blank secret
// retains the stored value. A switch to a different (or absent) provider starts
// from the schema defaults.
func (s *Server) sendingEditorState(ctx context.Context, accountID, domainID, provider string, fields []transport.ConfigField) (map[string]string, bool) {
	values := schemaDefaultValues(fields)
	cfg, err := s.Service.Store.GetDomainSendingConfig(ctx, accountID, domainID)
	if err != nil || !strings.EqualFold(cfg.Provider, provider) {
		return values, false
	}
	if dec, err := s.Service.DecryptDomainSendingConfig(cfg); err == nil {
		overlayNonSecret(values, fields, dec)
	}
	return values, true
}

// receivingEditorState is the receiving counterpart of sendingEditorState.
func (s *Server) receivingEditorState(ctx context.Context, accountID, domainID, provider string, fields []transport.ConfigField) (map[string]string, bool) {
	values := schemaDefaultValues(fields)
	cfg, err := s.Service.Store.GetDomainReceivingConfig(ctx, accountID, domainID)
	if err != nil || !strings.EqualFold(cfg.Provider, provider) {
		return values, false
	}
	if dec, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
		overlayNonSecret(values, fields, dec)
	}
	return values, true
}

// schemaDefaultValues returns the non-secret schema defaults, excluding secret
// and generated fields.
func schemaDefaultValues(fields []transport.ConfigField) map[string]string {
	values := map[string]string{}
	for _, f := range fields {
		if f.Secret || f.Generated {
			continue
		}
		if d := strings.TrimSpace(f.Default); d != "" {
			values[f.Name] = d
		}
	}
	return values
}

// overlayNonSecret replaces base values with the stored non-secret values for
// fields in the schema. Secret and generated fields are never overlaid.
func overlayNonSecret(base map[string]string, fields []transport.ConfigField, stored map[string]any) {
	for _, f := range fields {
		if f.Secret || f.Generated {
			continue
		}
		v, ok := stored[f.Name]
		if !ok || v == nil {
			continue
		}
		base[f.Name] = fmt.Sprintf("%v", v)
	}
}

// overlayValues applies retained non-secret submitted values over the editor's
// prefilled values.
func overlayValues(base, extra map[string]string) {
	for k, v := range extra {
		base[k] = v
	}
}

func domainIngestURL(baseURL string, t transport.InboundTransport) string {
	if ip, ok := t.(transport.IngestPathProvider); ok {
		if p := ip.IngestPath(); p != "" {
			return strings.TrimRight(baseURL, "/") + p
		}
	}
	return ""
}

// domainReceivingSteps returns the pre-save setup instructions for a receiving
// provider. The exact webhook URL is rendered separately above the steps.
func domainReceivingSteps(provider string) []string {
	switch provider {
	case "resend":
		return []string{
			"In Resend, open Webhooks and click Add Webhook.",
			"Paste the webhook URL shown here.",
			"Under events, tick email.received only.",
			"Click Add, reopen the webhook, and copy its signing secret (whsec_...).",
			"Paste the signing secret and a full-access Resend API key below, then save.",
		}
	case "cloudflare":
		return []string{
			"Save this form to generate a shared secret and Worker code.",
			"In Cloudflare, create a Worker and replace its code with the generated snippet.",
			"Enable Email Routing for this domain and add a Send to a Worker rule for each receiving address.",
		}
	case "mailgun":
		return []string{
			"In Mailgun, add this URL as the inbound route for raw MIME delivery.",
			"Paste the HTTP webhook signing key below, then save.",
		}
	default:
		return []string{"Register the webhook URL above with the provider, then fill in the fields below and save."}
	}
}

func domainActivityLabel(t time.Time) string {
	if t.IsZero() {
		return "Never"
	}
	return t.Format("2006-01-02 15:04")
}

const domainBody = `<div class="toolbar"><a href="/dashboard?tab=settings">← Domains</a></div>
{{if .Notice}}<div class="ok notice" role="status" aria-live="polite">{{.Notice}}</div>{{end}}{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
<section class="card"><div class="card-head"><h1>{{.Domain.Name}}</h1><form method="post" action="/ui/domains/{{.Domain.ID}}/delete" data-confirm="Delete this domain and ALL of its inboxes and messages? This cannot be undone."><input type="hidden" name="_csrf" value="{{.CSRF}}"><button class="secondary danger">Delete Domain</button></form></div><p class="muted">Created {{.Domain.CreatedAt.Format "2006-01-02 15:04"}}</p></section>
{{if .DomainWorkerCode}}<section class="card"><h2>Cloudflare Worker code</h2><p class="muted">Paste this into a Cloudflare Worker. It contains the generated shared secret and is shown only on this page.</p><ol class="steps"><li>In Cloudflare, open <b>Workers &amp; Pages</b> → <b>Create application</b> → <b>Worker</b> → <b>Deploy</b>.</li><li>Open the Worker, choose <b>Edit code</b>, replace the stub with the code below, then <b>Deploy</b>.</li><li>In Email Routing, add a <b>Send to a Worker</b> rule for each receiving address and choose this Worker.</li></ol><pre class="cf-code" id="cf-code">{{.DomainWorkerCode}}</pre><p class="copy-note" id="cf-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the code above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="cf-copy">Copy code</button></div></section>{{end}}
<div class="grid">
<section class="card"><h2>Catch-all inbox</h2><p class="muted">Mail sent to an unknown address on this domain is delivered to this inbox. Only inboxes on {{.Domain.Name}} can be selected.</p><form method="post" action="/ui/domains/{{.Domain.ID}}/catchall"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Catch-all inbox</label><select name="inbox"><option value="">No catch-all</option>{{range .DomainInboxes}}<option value="{{.ID}}"{{if eq .ID $.Domain.CatchAllInboxID}} selected{{end}}>{{.Address}}</option>{{end}}</select><div class="dialog-actions"><button>Save catch-all</button></div></form></section>
<section class="card"><h2>Sending</h2>{{with .DomainSendingSummary}}{{if .Configured}}<p><span class="pill">{{.ProviderLabel}}</span> <span class="muted">Updated {{.UpdatedAt.Format "2006-01-02 15:04"}} · Last sent {{.LastActivity}}</span> · <a href="/ui/domains/{{$.Domain.ID}}?sending={{.Provider}}">Edit</a></p>{{if .Fields}}<dl class="cfg-summary">{{range .Fields}}<dt>{{.Label}}</dt><dd>{{.Value}}</dd>{{end}}</dl>{{end}}<p class="muted small">Configured — delivery is not verified until a message is actually sent.</p><form method="post" action="/ui/domains/{{$.Domain.ID}}/sending/clear" data-confirm="Remove sending configuration for this domain? Mail will queue until a provider is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove sending</button></form>{{else}}<p class="muted">Sending is paused. Mail queues until a provider is configured.</p>{{end}}{{end}}
<p class="muted"><a href="/ui/domains/{{.Domain.ID}}/sending/deliveries">Delivery history</a></p>
<h3>{{if .DomainSendingSummary.Configured}}Change sending provider{{else}}Configure sending{{end}}</h3><form method="get" action="/ui/domains/{{.Domain.ID}}" class="provider-picker"><select name="sending"><option value="">Select a provider…</option>{{range .DomainSendingChoices}}<option value="{{.Name}}"{{if eq .Name $.DomainQuerySending}} selected{{end}}>{{.Description}}</option>{{end}}</select>{{if .DomainQueryReceiving}}<input type="hidden" name="receiving" value="{{.DomainQueryReceiving}}">{{end}}<button class="secondary" type="submit">Continue</button></form>
{{with .DomainSendingEditor}}{{$e := .}}<form method="post" action="/ui/domains/{{$.Domain.ID}}/sending" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="provider" value="{{.Provider}}">{{if .KeepSecrets}}<p class="muted">Saving {{.ProviderLabel}} updates this domain's sending configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted">Saving {{.ProviderLabel}} replaces this domain's sending configuration. Required secrets must be entered.</p>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}">{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}<div class="dialog-actions"><a class="btn secondary" href="/ui/domains/{{$.Domain.ID}}">Cancel</a><button>Save sending</button></div></form>{{end}}</section>
<section class="card"><h2>Receiving</h2>{{with .DomainReceivingSummary}}{{if .Configured}}<p><span class="pill">{{.ProviderLabel}}</span> <span class="muted">Updated {{.UpdatedAt.Format "2006-01-02 15:04"}} · Last received {{.LastActivity}}</span> · <a href="/ui/domains/{{$.Domain.ID}}?receiving={{.Provider}}">Edit</a></p>{{if .Fields}}<dl class="cfg-summary">{{range .Fields}}<dt>{{.Label}}</dt><dd>{{.Value}}</dd>{{end}}</dl>{{end}}{{if .Warning}}<div class="banner warn">{{.Warning}}</div>{{end}}{{if .WebhookURL}}<p class="muted small">Ingest URL: <code class="wrap">{{.WebhookURL}}</code></p>{{end}}<p class="muted small">Configured — inbound delivery is not verified until a message actually arrives.</p><p class="muted small">Inbound setup is managed by the provider; open Edit for the setup steps.</p><div class="actions-left">{{if .CanRegenerate}}<form method="post" action="/ui/domains/{{$.Domain.ID}}/receiving/regenerate" data-confirm="Regenerate the Worker secret? The current Worker stops working until you paste the new code."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="amber">Regenerate secret</button></form>{{end}}<form method="post" action="/ui/domains/{{$.Domain.ID}}/receiving/clear" data-confirm="Remove receiving configuration for this domain? It will stop accepting mail until a receive path is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove receiving</button></form></div>{{else}}<p class="muted">Not configured. This domain does not accept mail yet.</p>{{end}}{{end}}
<h3>{{if .DomainReceivingSummary.Configured}}Change receiving provider{{else}}Configure receiving{{end}}</h3><form method="get" action="/ui/domains/{{.Domain.ID}}" class="provider-picker"><select name="receiving"><option value="">Select a provider…</option>{{range .DomainReceivingChoices}}<option value="{{.Name}}"{{if eq .Name $.DomainQueryReceiving}} selected{{end}}>{{.Description}}</option>{{end}}</select>{{if .DomainQuerySending}}<input type="hidden" name="sending" value="{{.DomainQuerySending}}">{{end}}<button class="secondary" type="submit">Continue</button></form>
{{with .DomainReceivingEditor}}{{$e := .}}<form method="post" action="/ui/domains/{{$.Domain.ID}}/receiving" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><input type="hidden" name="provider" value="{{.Provider}}">{{if .WebhookURL}}{{if not .Generated}}<p class="muted">Register this webhook URL with {{.ProviderLabel}} before saving:</p><div class="secret"><pre id="setup-webhook-url">{{.WebhookURL}}</pre></div><p class="copy-note" id="setup-copy-note" hidden>Copying to the clipboard needs HTTPS. Select the URL above and copy it manually.</p><div class="dialog-actions"><button type="button" class="secondary" id="setup-copy">Copy webhook URL</button></div>{{end}}{{end}}{{if .Steps}}<ol class="steps">{{range .Steps}}<li>{{.}}</li>{{end}}</ol>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}">{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}{{if .KeepSecrets}}<p class="muted small">Saving updates this domain's receiving configuration. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted small">Saving replaces this domain's receiving configuration. Required secrets must be entered.</p>{{end}}<div class="dialog-actions"><a class="btn secondary" href="/ui/domains/{{$.Domain.ID}}">Cancel</a><button>Save receiving</button></div></form>{{end}}</section>
</div>`

const domainDeliveriesBody = `<div class="toolbar"><a href="/ui/domains/{{.Domain.ID}}">← {{.Domain.Name}}</a></div>
<section class="card"><h1>Delivery history</h1><p class="muted">Every send attempt for messages owned by {{.Domain.Name}}, regardless of which provider is configured now.</p>{{if .DeliveryAttempts}}<div class="table-wrap"><table style="font-size:12px"><thead><tr><th>When</th><th>Address</th><th>Message</th><th>Attempt</th><th>Status</th><th>Provider ID</th><th>Error</th></tr></thead><tbody>{{range .DeliveryAttempts}}<tr><td style="white-space:nowrap">{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if .MessageID}}<div>to: {{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</div><div>from: {{if .FromAddress}}{{.FromAddress}}{{else}}<span class="muted">—</span>{{end}}</div>{{else}}<span class="muted">message_deleted</span>{{end}}</td><td>{{if .MessageID}}<a href="/ui/messages/{{.MessageID}}">{{.MessageID}}</a>{{else}}<span class="muted">—</span>{{end}}</td><td>{{.Attempt}}</td><td>{{if eq .Status "sent"}}<span class="pill">Sent</span>{{else}}<span class="pill danger">Failed</span>{{end}}</td><td class="muted" style="font-size:10px;word-break:break-all">{{.ProviderMessageID}}</td><td class="muted" style="word-break:break-all">{{.ErrorText}}</td></tr>{{end}}</tbody></table></div>{{if .DeliveryHasMore}}<p><a href="/ui/domains/{{.Domain.ID}}/sending/deliveries?before={{.DeliveryBefore}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No delivery attempts yet.</p>{{end}}</section>`
