package httpapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// domainEditorView drives the save form for a chosen provider. Non-secret
// values are prefilled from the stored same-provider config or the schema
// defaults; secret inputs are always truly empty and generated fields are not
// rendered at all.
type domainEditorView struct {
	Kind          string // "sending" | "receiving"
	Provider      string
	ProviderLabel string
	// SelectLabel is the longer label shown in the provider <select>. It falls
	// back to ProviderLabel when empty, so only providers that distinguish a
	// short label from a descriptive choice set it (Antler MX).
	SelectLabel string
	Fields      []transport.ConfigField
	WebhookURL  string
	Steps       []string
	Generated   bool
	// KeepSecrets is true when the stored config already uses this provider, so
	// a blank secret retains the stored value ("leave blank to keep"). It is
	// false for a create or provider switch, where required secrets must be
	// supplied.
	KeepSecrets bool
	// Values holds only non-secret field values to prefill the inputs/selects.
	Values map[string]string
	// Error is a user-safe validation message shown inside the dialog.
	Error string
	// Selected marks the provider whose field group is shown and enabled.
	Selected bool
}

// domainSendingEditor builds the prefilled sending editor for one provider,
// returning false for an unknown provider.
func (s *Server) domainSendingEditor(ctx context.Context, accountID, domainID, provider string) (*domainEditorView, bool) {
	if _, ok := transport.LookupOutbound(provider); !ok {
		return nil, false
	}
	e, ok := newDomainEditor("sending", provider, s.Service.Config.BaseURL)
	if !ok {
		return nil, false
	}
	e.Values, e.KeepSecrets = s.sendingEditorState(ctx, accountID, domainID, provider, e.Fields)
	return e, true
}

// domainReceivingEditor is the receiving counterpart of domainSendingEditor.
func (s *Server) domainReceivingEditor(ctx context.Context, accountID, domainID, provider string) (*domainEditorView, bool) {
	e, ok := newDomainEditor("receiving", provider, s.Service.Config.ReceiverURL())
	if !ok {
		return nil, false
	}
	// Direct MX is an admin-only receiving option: it binds the domain to the
	// installation-wide receiver, which an account admin must explicitly opt
	// into. Non-admin operators never see it.
	if provider == "mx" && !principalFromContext(ctx).Admin {
		return nil, false
	}
	e.Values, e.KeepSecrets = s.receivingEditorState(ctx, accountID, domainID, provider, e.Fields)
	// The Direct MX status panel rendered with the dialog reports whether the
	// installation receiver is configured, so the editor steps stay generic.
	return e, true
}

// domainSendingEditors builds one editor per outbound provider so the dialog can
// switch between them client-side without a round trip. The primary providers
// lead in a fixed order; the rest follow alphabetically by display label.
func (s *Server) domainSendingEditors(ctx context.Context, accountID, domainID string) []*domainEditorView {
	out := []*domainEditorView{}
	for _, t := range transport.ListOutbound() {
		if e, ok := s.domainSendingEditor(ctx, accountID, domainID, t.Name()); ok {
			out = append(out, e)
		}
	}
	sortDomainEditors(out, map[string]int{"smtp": 0, "mx": 1})
	return out
}

// domainReceivingEditors is the receiving counterpart of domainSendingEditors.
// Antler MX, Direct MX and Remote MX lead in that order; the rest follow
// alphabetically by display label.
func (s *Server) domainReceivingEditors(ctx context.Context, accountID, domainID string) []*domainEditorView {
	out := []*domainEditorView{}
	for _, t := range transport.ListInbound() {
		if e, ok := s.domainReceivingEditor(ctx, accountID, domainID, t.Name()); ok {
			out = append(out, e)
		}
	}
	sortDomainEditors(out, map[string]int{"dialmx": 0, "mx": 1, "remotemx": 2})
	return out
}

// sortDomainEditors orders provider editors by an explicit priority map, then
// alphabetically by the label shown in the <select>. Providers absent from
// priorities sort after every listed provider.
func sortDomainEditors(editors []*domainEditorView, priorities map[string]int) {
	priority := func(p string) int {
		if n, ok := priorities[p]; ok {
			return n
		}
		return len(priorities)
	}
	label := func(e *domainEditorView) string {
		if e.SelectLabel != "" {
			return e.SelectLabel
		}
		return e.ProviderLabel
	}
	sort.SliceStable(editors, func(i, j int) bool {
		pi, pj := priority(editors[i].Provider), priority(editors[j].Provider)
		if pi != pj {
			return pi < pj
		}
		return label(editors[i]) < label(editors[j])
	})
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
		s.uiError(w, err, 400)
		return
	}
	s.domainNotice(w, r, "Catch-all inbox updated")
}

// providerInherited is the sentinel provider value used by the sending and
// receiving editors for a subdomain that should reuse an ancestor's connector
// instead of its own. It is a UI-only value, never persisted as a provider.
const providerInherited = "inherited"

// uiDomainSending creates, replaces or updates the domain's single sending
// configuration. The provider is always taken from the submitted form, so any
// provider (including a switch from another provider) can replace it. The
// sentinel provider "inherited" instead removes the domain's own sending config
// and switches it to reuse its parent's.
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
	if provider == providerInherited {
		if err := s.setSendingInherited(ctx, p.AccountID, d); err != nil {
			s.domainSaveError(w, r, d.ID, "sending", provider, err)
			return
		}
		s.domainNotice(w, r, "Sending configuration now inherits from the parent domain")
		return
	}
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
	// An explicit provider is the domain's own; stop it falling back to the
	// parent if this config is later removed.
	if d.InheritSending {
		if err := s.Service.Store.SetDomainInheritFlag(ctx, p.AccountID, d.ID, true, false); err != nil {
			s.domainSaveError(w, r, d.ID, "sending", provider, err)
			return
		}
	}
	s.domainNotice(w, r, "Sending configuration saved")
}

// setSendingInherited makes a subdomain reuse its parent's sending connector:
// its own sending config is removed and inherit_sending is switched on. When the
// domain was added before its parent and is not linked yet, it is linked to the
// nearest existing ancestor first. It errors when no parent can serve it or the
// parent has no sending provider to inherit.
func (s *Server) setSendingInherited(ctx context.Context, accountID string, d model.Domain) error {
	if d.ParentDomainID == "" {
		linked, err := s.linkNearestAncestor(ctx, accountID, d)
		if err != nil {
			return err
		}
		d = linked
	}
	parent, err := s.Service.Store.GetDomain(ctx, accountID, d.ParentDomainID)
	if err != nil {
		return err
	}
	if parent.SendingProvider == "" {
		return fmt.Errorf("%w: the parent domain has no sending configuration to inherit", app.ErrInvalidConfig)
	}
	if err := s.Service.Store.DeleteDomainSendingConfig(ctx, accountID, d.ID); err != nil && !errors.Is(err, store.ErrNoProvider) {
		return err
	}
	return s.Service.Store.SetDomainInheritFlag(ctx, accountID, d.ID, true, true)
}

// linkNearestAncestor links a domain that has no parent yet to the nearest
// existing ancestor in the same account (the parent added after it). It errors
// when no ancestor exists.
func (s *Server) linkNearestAncestor(ctx context.Context, accountID string, d model.Domain) (model.Domain, error) {
	candidates, err := s.Service.Store.InheritableAncestors(ctx, accountID, d.ID)
	if err != nil {
		return d, err
	}
	if len(candidates) == 0 {
		return d, fmt.Errorf("%w: only a subdomain can inherit a configuration", app.ErrInvalidConfig)
	}
	if err := s.Service.Store.SetDomainParent(ctx, accountID, d.ID, candidates[0].ID); err != nil {
		return d, err
	}
	return s.Service.Store.GetDomain(ctx, accountID, d.ID)
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
		s.uiError(w, err, 400)
		return
	}
	s.domainNotice(w, r, "Sending configuration removed")
}

// uiAntlerContactEmail updates only the hosted setup's contact metadata.
func (s *Server) uiAntlerContactEmail(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Config   struct {
			ContactEmail string `json:"contact_email"`
		} `json:"config"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	cfg, err := s.Service.UpdateAntlerContactEmail(r.Context(), p.AccountID, id, in.Config.ContactEmail)
	if err != nil {
		mapDomainConfigError(w, err)
		return
	}
	response, err := s.domainReceivingResponse(r.Context(), id, cfg)
	if err != nil {
		mapDomainConfigError(w, err)
		return
	}
	writeDomainConfigJSON(w, http.StatusOK, response)
}

// uiDomainReceiving creates, replaces or updates the domain's single receiving
// configuration. A generated provider secret (Cloudflare) is minted only when
// missing and returned once as Worker code.
func (s *Server) uiDomainReceiving(w http.ResponseWriter, r *http.Request) {
	if s.Service.MXRuntime != nil {
		defer s.Service.MXRuntime.Wake()
	}
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
	if provider == providerInherited {
		if err := s.setReceivingInherited(ctx, p.AccountID, d); err != nil {
			s.domainSaveError(w, r, d.ID, "receiving", provider, err)
			return
		}
		s.domainNotice(w, r, "Receiving configuration now inherits from the parent domain")
		return
	}
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
	if provider == "mx" && p.SystemAdmin {
		if !s.saveIncludedMXForm(w, r) {
			return
		}
	}
	saved, generated, err := s.Service.SaveDomainReceivingConfig(ctx, p.AccountID, d.ID, provider, cfg, false)
	if err != nil {
		s.domainSaveError(w, r, d.ID, "receiving", provider, err)
		return
	}
	// An explicit provider is the domain's own; stop it falling back to the
	// parent if this config is later removed.
	if d.InheritReceiving {
		if err := s.Service.Store.SetDomainInheritFlag(ctx, p.AccountID, d.ID, false, false); err != nil {
			s.domainSaveError(w, r, d.ID, "receiving", provider, err)
			return
		}
	}
	if provider == "dialmx" {
		http.Redirect(w, r, "/?"+url.Values{"domain": {d.ID}, "kind": {"receiving"}, "provider": {"dialmx"}}.Encode(), http.StatusSeeOther)
		return
	}
	if secret := generated["webhook_secret"]; secret != "" {
		s.flashDomainWorker(w, r, p, d.ID, saved, secret)
		return
	}
	s.domainNotice(w, r, "Receiving configuration saved")
}

// setReceivingInherited makes a subdomain reuse its parent's receiving
// connector: its own receiving config is removed and inherit_receiving is
// switched on. When the domain was added before its parent and is not linked
// yet, it is linked to the nearest existing ancestor first. It errors when no
// parent can serve it or the parent has no receiving provider to inherit.
func (s *Server) setReceivingInherited(ctx context.Context, accountID string, d model.Domain) error {
	if d.ParentDomainID == "" {
		linked, err := s.linkNearestAncestor(ctx, accountID, d)
		if err != nil {
			return err
		}
		d = linked
	}
	parent, err := s.Service.Store.GetDomain(ctx, accountID, d.ParentDomainID)
	if err != nil {
		return err
	}
	if parent.ReceivingProvider == "" {
		return fmt.Errorf("%w: the parent domain has no receiving configuration to inherit", app.ErrInvalidConfig)
	}
	if err := s.Service.Store.DeleteDomainReceivingConfig(ctx, accountID, d.ID); err != nil && !errors.Is(err, store.ErrNoProvider) {
		return err
	}
	return s.Service.Store.SetDomainInheritFlag(ctx, accountID, d.ID, false, true)
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
		s.uiError(w, err, 400)
		return
	}
	s.domainNotice(w, r, "Receiving configuration removed")
	if s.Service.MXRuntime != nil {
		s.Service.MXRuntime.Wake()
	}
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
	if d.ReceivingProvider == "dialmx" {
		if _, err := s.Service.RotateDialMXCredential(ctx, p.AccountID, d.ID); err != nil {
			s.domainSaveError(w, r, d.ID, "receiving", "dialmx", err)
			return
		}
		http.Redirect(w, r, "/?"+url.Values{"domain": {d.ID}, "kind": {"receiving"}, "provider": {"dialmx"}}.Encode(), http.StatusSeeOther)
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
	s.domainNotice(w, r, "Receiving secret regenerated")
}

// domainDeliveries shows the two-way activity log owned by one domain:
// outbound delivery attempts plus delivered and blocked inbound mail, merged
// newest first. It is scoped by domain, independent of whichever provider
// config currently exists.
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
	var before time.Time
	if v := strings.TrimSpace(r.URL.Query().Get("before")); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			before = t
		}
	}
	entries, err := s.Service.Store.ListDomainLog(ctx, p.AccountID, d.ID, 51, before)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	hasMore := len(entries) > 50
	if hasMore {
		entries = entries[:50]
	}
	nextBefore := ""
	if len(entries) > 0 {
		nextBefore = entries[len(entries)-1].At.UTC().Format(time.RFC3339Nano)
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, domainDeliveriesBody, pageData{
		Title:      d.Name + " · Domain log",
		Tab:        "home",
		Principal:  p,
		CSRF:       csrf(r),
		Account:    acc,
		Domain:     &d,
		LogEntries: entries,
		LogHasMore: hasMore,
		LogBefore:  nextBefore,
	})
}

// domainNotice stores a success notice and redirects back to the dashboard.
func (s *Server) domainNotice(w http.ResponseWriter, r *http.Request, notice string) {
	dest := "/?" + url.Values{"notice": {notice}}.Encode()
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// domainEditorError reopens the relevant provider editor with a user-safe
// validation message. The flash is bound to this account/user/domain and keeps
// only non-secret submitted values, so no secret value is ever included and a
// foreign viewer cannot consume it.
func (s *Server) domainEditorError(w http.ResponseWriter, r *http.Request, domainID, kind, provider, msg string) {
	p := principal(r)
	q := url.Values{}
	q.Set("domain", domainID)
	q.Set("kind", kind)
	q.Set("provider", provider)
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
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
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
	base := s.Service.Config.ReceiverURL()
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
	dest := "/?domain=" + url.PathEscape(domainID)
	if tok := s.flashes.put(f, len(code)+128); tok != "" {
		dest += "&_flash=" + tok
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
	// A provider may supply a longer, descriptive picker label than its short
	// Description(): Antler MX states its zero-config nature, Remote MX states
	// that the receiver is the account's own.
	e.SelectLabel = transport.InboundSelectLabel(t)
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
		// A pre-Antler Dial MX config has receiver URLs but no service choice.
		// Prefill it as custom so saving the form cannot silently migrate it.
		if provider == "dialmx" {
			if _, ok := dec["service"]; !ok {
				if urls, _ := dec["receiver_urls"].(string); strings.TrimSpace(urls) != "" {
					values["service"] = "custom"
				}
			}
		}
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
			"Paste the signing secret and a full-access Resend key below, then save.",
		}
	case "cloudflare":
		return []string{
			"Save this form to generate a shared secret and Worker code.",
			"In Cloudflare, create a Worker (Workers & Pages, Create application, Start with Hello World, Deploy).",
			"Open the Worker, choose Edit code, replace the stub with the generated snippet, then Deploy.",
			"Onboard Email Routing for this domain, edit the catch-all rule to Send to a Worker, select this Worker and enable it.",
		}
	case "mailgun":
		return []string{
			"In Mailgun, add this URL as the inbound route for raw MIME delivery.",
			"Paste the HTTP webhook signing key below, then save.",
		}
	case "dialmx":
		return []string{
			"Choose Antler MX for the zero-config free relay, or Custom receiver URLs to point at your own receiver.",
			"For Antler MX, enter a contact email and save; the MX and TXT records to publish are then shown in this dialog.",
			"For a custom service, add each receiver base URL as an HTTPS origin; multiple receivers can be comma separated.",
			"Save to generate the domain key, then publish the shown MM1 TXT record at _mailmoose-mx.<this-domain> and the shown MX records.",
			"After regenerating the key, replace this domain's TXT record; the core will re-authenticate with each receiver.",
			"Choose moderate or hard authentication enforcement, then save.",
		}
	case "mx":
		return []string{
			"Point this domain's MX record at the receiver's advertised SMTP hostname (shown below).",
			"Publish an SPF record for the domain; DKIM and DMARC are computed at the receiver.",
			"Choose an enforcement mode below (moderate is the default), then save.",
		}
	default:
		return []string{"Register the webhook URL above with the provider, then fill in the fields below and save."}
	}
}

const clientDeliveriesBody = `<div class="toolbar"><a href="/">← Clients</a></div>
<section class="card"><h1>{{.ClientLogClient.Name}} · Log</h1><p class="muted">{{if eq .ClientLogClient.Kind "webhook"}}Events delivered to this Webhook client, newest first. “Acknowledged” means the endpoint returned 2xx; queued and retrying rows are still being attempted. Completed history is retained for about 30 days.{{else}}Events delivered to this Hermes relay, newest first. “Acknowledged” means the gateway accepted the event — not that the agent finished processing it. Completed history is retained for about 30 days.{{end}}</p>{{if .ClientLogEntries}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Status</th><th>Event</th><th>Message</th><th>Detail</th><th></th></tr></thead><tbody>{{range .ClientLogEntries}}<tr><td style="white-space:nowrap">{{localDateTime .At}}</td><td><span class="pill{{if eq .Status "failed"}} danger{{else if or (eq .Status "pending") (eq .Status "skipped")}} amber{{end}}">{{deliveryStatus .Status .Attempts .LastError}}</span></td><td>{{if .EventType}}{{.EventType}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Detail}}{{.Detail}}{{else}}<span class="muted">—</span>{{end}}</td><td class="muted log-detail">attempt {{.Attempts}}{{if .NextAttemptAt}} · next {{.NextAttemptAt}}{{end}}{{if .LastError}} · {{.LastError}}{{end}}</td><td>{{if .MessageID}}<a href="/ui/messages/{{.MessageID}}">Open</a>{{else}}<span class="muted">—</span>{{end}}</td></tr>{{end}}</tbody></table></div>{{if .ClientLogHasMore}}<p><a href="/ui/clients/{{.ClientLogClient.ID}}/log?before={{.ClientLogBefore}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No deliveries yet.</p>{{end}}</section>`

const domainDeliveriesBody = `<div class="toolbar"><a href="/?domain={{.Domain.ID}}">← {{.Domain.Name}}</a></div>
<section class="card"><h1>Domain log</h1><p class="muted">Two-way activity for {{.Domain.Name}}: delivered, blocked and approval-control inbound mail, plus every outbound send attempt, newest first. Outbound attempts are retained for about 30 days; received and blocked mail follows normal message retention.</p>{{if .LogEntries}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Direction</th><th>From</th><th>To</th><th>Subject</th><th>Client</th><th>Detail</th><th></th></tr></thead><tbody>{{range .LogEntries}}<tr><td style="white-space:nowrap">{{localDateTime .At}}</td><td>{{if eq .Kind "sent"}}<span class="pill">Sent</span>{{else if eq .Kind "sending"}}<span class="pill amber">Sending…</span>{{else if eq .Kind "interrupted"}}<span class="pill danger">Interrupted</span>{{else if eq .Kind "failed"}}<span class="pill danger">Failed</span>{{else if or (eq .Kind "received") (eq .Kind "approval")}}<span class="pill">Received</span>{{else}}<span class="pill amber">Blocked</span>{{end}}</td><td>{{if .FromAddress}}{{.FromAddress}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Subject}}{{.Subject}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if eq .Client "Control"}}<span class="pill">Control</span>{{else if .Client}}{{.Client}}{{else}}<span class="muted">—</span>{{end}}</td><td class="muted log-detail">{{if eq .Kind "sent"}}attempt {{.Attempt}}{{if .ProviderMessageID}} · {{.ProviderMessageID}}{{end}}{{else if eq .Kind "sending"}}attempt {{.Attempt}} in progress{{if .Provider}} · {{.Provider}}{{end}}{{else if eq .Kind "interrupted"}}attempt {{.Attempt}} interrupted before an outcome was recorded{{if .Provider}} · {{.Provider}}{{end}}{{else if eq .Kind "failed"}}attempt {{.Attempt}}{{if .ErrorText}} · {{.ErrorText}}{{end}}{{else if eq .Kind "blocked"}}{{if .Reason}}{{.Reason}}{{else}}blocked{{end}}{{else if eq .Kind "approval"}}{{.Status}}{{if .Reason}} · {{.Reason}}{{end}}{{else}}{{if .Provider}}{{.Provider}}{{end}}{{if .SizeBytes}} · {{bytes .SizeBytes}}{{end}}{{end}}</td><td>{{if .MessageID}}<a href="/ui/messages/{{.MessageID}}">Open</a>{{else}}<span class="muted">—</span>{{end}}</td></tr>{{end}}</tbody></table></div>{{if .LogHasMore}}<p><a href="/ui/domains/{{.Domain.ID}}/sending/deliveries?before={{.LogBefore}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No activity yet.</p>{{end}}</section>`
