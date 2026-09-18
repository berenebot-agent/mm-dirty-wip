package httpapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// externalAliasDialogView drives one external alias's connector popup. It mirrors
// the domain sending editor: a provider picker plus the selected provider's
// fields, with no alias identity repeated inside the dialog.
type externalAliasDialogView struct {
	InboxID    string
	AliasID    string
	Address    string
	Configured bool
	Editors    []*domainEditorView
	Selected   string
	Label      string
	Open       bool
}

// externalAliasAdmin authorizes an external-alias UI action and maps a
// non-admin/hosted principal to 403. External aliases are a self-hosted,
// Admin-only capability.
func (s *Server) externalAliasAdmin(w http.ResponseWriter, r *http.Request) (model.Principal, bool) {
	p := principal(r)
	if err := s.Service.ExternalAliasAdmin(p); err != nil {
		http.Error(w, "admin required", http.StatusForbidden)
		return p, false
	}
	return p, true
}

// externalAliasContext loads the inbox and external alias named in the path,
// rejecting a mismatched pairing with 404 so one inbox cannot address another
// inbox's alias.
func (s *Server) externalAliasContext(w http.ResponseWriter, r *http.Request, accountID string) (model.Inbox, store.ExternalAlias, bool) {
	ctx := r.Context()
	box, err := s.Service.Store.GetInboxInternal(ctx, accountID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "inbox not found", http.StatusNotFound)
		return model.Inbox{}, store.ExternalAlias{}, false
	}
	a, err := s.Service.Store.GetExternalAlias(ctx, accountID, box.ID, r.PathValue("aliasID"))
	if err != nil {
		http.Error(w, "external alias not found", http.StatusNotFound)
		return model.Inbox{}, store.ExternalAlias{}, false
	}
	return box, a, true
}

// externalAliasConfigureURL opens the alias's connector popup on the dashboard.
func externalAliasConfigureURL(aliasID string) string {
	return "/?" + url.Values{"alias": {aliasID}}.Encode()
}

// uiCreateExternalAlias adds a send-only external alias to a saved inbox. The
// address is immutable after creation; the connector is configured from the
// alias's popup on the dashboard.
func (s *Server) uiCreateExternalAlias(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	if _, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		http.Error(w, "inbox not found", http.StatusNotFound)
		return
	}
	a, err := s.Service.CreateExternalAlias(r.Context(), p, r.PathValue("id"), r.Form.Get("external_alias"), r.Form.Get("external_alias_name"))
	if err != nil {
		s.externalAliasError(w, err)
		return
	}
	// Return to the dashboard with the new alias's connector dialog open, since
	// the alias exists but cannot send until a connector is configured. The
	// inbox param reopens inbox settings behind it, so cancelling the popup
	// resumes there instead of dropping to the bare dashboard.
	q := url.Values{"alias": {a.ID}, "inbox": {a.InboxID}, "notice": {"External alias created — configure its sending connector"}}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// uiUpdateExternalAlias edits an external alias's display name from the
// dashboard. The address is intentionally not editable.
func (s *Server) uiUpdateExternalAlias(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	box, a, ok := s.externalAliasContext(w, r, p.AccountID)
	if !ok {
		return
	}
	if _, err := s.Service.UpdateExternalAlias(r.Context(), p, box.ID, a.ID, r.Form.Get("external_alias_name")); err != nil {
		s.externalAliasError(w, err)
		return
	}
	s.externalAliasBackToInbox(w, r, box.ID, "Alias updated")
}

func (s *Server) uiDeleteExternalAlias(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	box, a, ok := s.externalAliasContext(w, r, p.AccountID)
	if !ok {
		return
	}
	if err := s.Service.DeleteExternalAlias(r.Context(), p, box.ID, a.ID); err != nil {
		s.externalAliasError(w, err)
		return
	}
	s.externalAliasBackToInbox(w, r, box.ID, "External alias deleted")
}

// uiExternalAliasSending saves the alias's connector using the same schema,
// secret retention and validation as a domain sending config.
func (s *Server) uiExternalAliasSending(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	box, a, ok := s.externalAliasContext(w, r, p.AccountID)
	if !ok {
		return
	}
	provider := normalizeDomainProvider(r.Form.Get("provider"))
	fields, ok := outboundSchemaFields(provider)
	if !ok {
		s.externalAliasEditorError(w, r, box, a, provider, "Unknown sending provider")
		return
	}
	cfg, err := configFromForm(provider, fields, r)
	if err != nil {
		s.externalAliasEditorError(w, r, box, a, provider, err.Error())
		return
	}
	if _, err := s.Service.SaveExternalAliasSendingConfig(r.Context(), p, box.ID, a.ID, provider, cfg); err != nil {
		s.externalAliasSaveError(w, r, box, a, provider, err)
		return
	}
	s.externalAliasBackToInbox(w, r, box.ID, "Sending connector saved")
}

// uiExternalAliasSendingClear removes the alias's connector. Pending sends for
// the alias are requeued and then held until a connector is configured again.
func (s *Server) uiExternalAliasSendingClear(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	box, a, ok := s.externalAliasContext(w, r, p.AccountID)
	if !ok {
		return
	}
	if err := s.Service.DeleteExternalAliasSendingConfig(r.Context(), p, box.ID, a.ID); err != nil {
		s.externalAliasError(w, err)
		return
	}
	s.externalAliasBackToInbox(w, r, box.ID, "Sending connector removed")
}

// externalAliasBackToInbox redirects to the dashboard with the inbox's edit
// dialog reopened on the Aliases tab, so a connector save returns the operator
// to inbox settings rather than leaving the popup open.
func (s *Server) externalAliasBackToInbox(w http.ResponseWriter, r *http.Request, inboxID, notice string) {
	q := url.Values{"inbox": {inboxID}}
	if notice != "" {
		q.Set("notice", notice)
	}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// uiExternalAlias renders one alias's read-only outbound activity log. Name,
// connector and delete controls live on the dashboard.
func (s *Server) uiExternalAlias(w http.ResponseWriter, r *http.Request) {
	p, ok := s.externalAliasAdmin(w, r)
	if !ok {
		return
	}
	box, a, ok := s.externalAliasContext(w, r, p.AccountID)
	if !ok {
		return
	}
	ctx := r.Context()
	before, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("before")), 10, 64)
	attempts, err := s.Service.Store.ListExternalAliasDeliveryAttempts(ctx, p.AccountID, box.ID, a.ID, 51, before)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hasMore := len(attempts) > 50
	if hasMore {
		attempts = attempts[:50]
	}
	var nextBefore int64
	if len(attempts) > 0 {
		nextBefore = attempts[len(attempts)-1].ID
	}
	acc, _ := s.Service.Store.GetAccount(ctx, p.AccountID)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, externalAliasBody, pageData{
		Title:                    a.Address + " · External alias",
		Tab:                      "home",
		Principal:                p,
		CSRF:                     csrf(r),
		Account:                  acc,
		Inbox:                    &box,
		ExternalAlias:            &a,
		ExternalDeliveryAttempts: attempts,
		ExternalLogHasMore:       hasMore,
		ExternalLogBefore:        nextBefore,
	})
}

// externalAliasSendingEditors builds one prefilled outbound editor per provider
// for the alias's connector, mirroring the domain sending editor. A missing
// alias yields no editors.
func (s *Server) externalAliasSendingEditors(ctx context.Context, accountID, inboxID, aliasID string) []*domainEditorView {
	out := []*domainEditorView{}
	a, err := s.Service.Store.GetExternalAlias(ctx, accountID, inboxID, aliasID)
	if err != nil {
		return out
	}
	for _, t := range transport.ListOutbound() {
		e, ok := newDomainEditor("sending", t.Name(), s.Service.Config.BaseURL)
		if !ok {
			continue
		}
		e.Values = schemaDefaultValues(e.Fields)
		e.KeepSecrets = false
		if a.Configured && strings.EqualFold(a.Provider, t.Name()) {
			if dec, derr := s.Service.DecryptExternalAliasSendingConfig(a); derr == nil {
				overlayNonSecret(e.Values, e.Fields, dec)
			}
			e.KeepSecrets = true
		}
		out = append(out, e)
	}
	return out
}

// externalAliasDialog builds the connector popup state for one alias. flash, if
// for this alias, supplies the validation error and retained non-secret values.
func (s *Server) externalAliasDialog(ctx context.Context, accountID string, a model.ExternalAlias, requestedProvider, openAliasID string, flash *externalAliasNoticeFlash) externalAliasDialogView {
	v := externalAliasDialogView{InboxID: a.InboxID, AliasID: a.ID, Address: a.Address, Configured: a.Configured}
	v.Editors = s.externalAliasSendingEditors(ctx, accountID, a.InboxID, a.ID)
	selected := normalizeDomainProvider(requestedProvider)
	if selected == "" && a.Configured {
		selected = normalizeDomainProvider(a.Provider)
	}
	if flash != nil && flash.AliasID == a.ID {
		selected = normalizeDomainProvider(flash.Provider)
	}
	for _, e := range v.Editors {
		if e.Provider == selected {
			e.Selected = true
			v.Selected = e.Provider
			v.Label = e.ProviderLabel
		}
	}
	if flash != nil && flash.AliasID == a.ID {
		for _, e := range v.Editors {
			if strings.EqualFold(e.Provider, flash.Provider) {
				e.Error = flash.Error
				overlayValues(e.Values, flash.Values)
			}
		}
	}
	v.Open = openAliasID == a.ID || (flash != nil && flash.AliasID == a.ID)
	return v
}

// externalAliasDialogs builds the connector popups for every external alias in
// the given inboxes, so the dashboard can render them alongside domain dialogs.
func (s *Server) externalAliasDialogs(ctx context.Context, accountID string, boxes []model.Inbox, requestedProvider, openAliasID string, flash *externalAliasNoticeFlash) []externalAliasDialogView {
	out := []externalAliasDialogView{}
	for _, b := range boxes {
		for _, a := range b.ExternalAliases {
			out = append(out, s.externalAliasDialog(ctx, accountID, a, requestedProvider, openAliasID, flash))
		}
	}
	return out
}

// externalAliasNoticeFlash carries a user-safe connector validation error back
// to the dashboard popup, with the submitted non-secret values retained. Secret
// values are never stored.
type externalAliasNoticeFlash struct {
	AccountID string
	UserID    string
	InboxID   string
	AliasID   string
	AliasRev  int64
	Provider  string
	Error     string
	Values    map[string]string
}

// takeExternalAliasFlash validates and consumes a flash bound to this
// account/user/alias revision.
func (s *Server) takeExternalAliasFlash(tok string, p model.Principal, boxes []model.Inbox) *externalAliasNoticeFlash {
	v, ok := s.flashes.peek(tok)
	if !ok {
		return nil
	}
	f, ok := v.(externalAliasNoticeFlash)
	if !ok || f.AccountID != p.AccountID || f.UserID != p.UserID {
		return nil
	}
	// The alias must still exist at the same revision, so a stale flash from a
	// changed or deleted alias is never applied.
	found := false
	for _, b := range boxes {
		for _, a := range b.ExternalAliases {
			if a.ID == f.AliasID && a.Revision == f.AliasRev {
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	taken, ok := s.flashes.take(tok)
	if !ok {
		return nil
	}
	tf, ok := taken.(externalAliasNoticeFlash)
	if !ok || tf.AliasID != f.AliasID || tf.AliasRev != f.AliasRev || tf.Error != f.Error {
		return nil
	}
	return &tf
}

// externalAliasEditorError reopens the alias's connector popup on the dashboard
// with a user-safe validation message and the retained non-secret submitted
// values.
func (s *Server) externalAliasEditorError(w http.ResponseWriter, r *http.Request, box model.Inbox, a store.ExternalAlias, provider, msg string) {
	p := principal(r)
	values := submittedEditorValues("sending", provider, r)
	size := len(msg) + 32
	for k, v := range values {
		size += len(k) + len(v) + 8
	}
	if size > 4096 {
		values = nil
	}
	f := externalAliasNoticeFlash{AccountID: p.AccountID, UserID: p.UserID, InboxID: box.ID, AliasID: a.ID, AliasRev: a.Revision, Provider: provider, Error: msg, Values: values}
	// The inbox param reopens inbox settings behind the popup, so cancelling
	// it resumes there instead of dropping to the bare dashboard.
	q := url.Values{"alias": {a.ID}, "inbox": {box.ID}, "provider": {provider}}
	if tok := s.flashes.put(f, size); tok != "" {
		q.Set("_flash", tok)
	}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

// externalAliasSaveError maps a connector save failure to the popup. It mirrors
// domainSaveError but keeps the alias's dialog as the destination.
func (s *Server) externalAliasSaveError(w http.ResponseWriter, r *http.Request, box model.Inbox, a store.ExternalAlias, provider string, err error) {
	switch {
	case errors.Is(err, app.ErrInvalidConfig), errors.Is(err, transport.ErrUnknownProvider):
		s.externalAliasEditorError(w, r, box, a, provider, err.Error())
	case errors.Is(err, store.ErrConflict):
		s.externalAliasEditorError(w, r, box, a, provider, "This configuration changed in another session. Reload and try again.")
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "external alias not found", http.StatusNotFound)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		s.Log.Error("external alias sending config save failed", "provider", provider, "error", err)
		http.Error(w, "could not save configuration", http.StatusInternalServerError)
	}
}

// externalAliasError maps a generic alias operation failure to a UI response.
func (s *Server) externalAliasError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidAlias), errors.Is(err, store.ErrConflict):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		s.Log.Error("external alias operation failed", "error", err)
		http.Error(w, "could not complete the request", http.StatusInternalServerError)
	}
}

// externalAliasConnectorDialogs renders the domain-style connector popup for
// each external alias. It is embedded in the dashboard body (where the shared
// dialog JS runs), containing only the provider editor, never the identity.
const externalAliasConnectorDialogs = `{{range .ExternalAliasDialogs}}<dialog id="external-alias-sending-dialog-{{.AliasID}}" class="domain-dialog"{{if .Open}} data-open="1"{{end}}><h2>Sending · {{.Address}}</h2><form id="external-alias-sending-form-{{.AliasID}}" method="post" action="/ui/inboxes/{{.InboxID}}/external-aliases/{{.AliasID}}/sending" class="cfg-form" autocomplete="off"><input type="hidden" name="_csrf" value="{{$.CSRF}}"><label>Provider</label><select name="provider" class="provider-select"><option value="">Select a provider…</option>{{range .Editors}}<option value="{{.Provider}}"{{if .Selected}} selected{{end}}>{{.ProviderLabel}}</option>{{end}}</select><p class="muted provider-hint"{{if .Selected}} hidden{{end}}>Choose a provider to configure sending from this alias.</p>{{range .Editors}}{{$e := .}}<div class="provider-fields provider-box" data-provider="{{.Provider}}"{{if not .Selected}} hidden{{end}}>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}{{if .KeepSecrets}}<p class="muted">Saving {{.ProviderLabel}} updates this alias's connector. Leave a secret blank to keep the current one.</p>{{else}}<p class="muted">Saving {{.ProviderLabel}} replaces this alias's connector. Required secrets must be entered.</p>{{end}}{{range .Fields}}{{if not .Generated}}{{if .Options}}<label>{{.Label}}{{if .Required}} *{{end}}</label><select name="cfg_{{$e.Provider}}_{{.Name}}"{{if not $e.Selected}} disabled{{end}}>{{$f := .}}{{range .Options}}<option value="{{.Value}}"{{if eq .Value (index $e.Values $f.Name)}} selected{{end}}>{{.Label}}</option>{{end}}</select>{{else}}<label>{{.Label}}{{if .Required}} *{{end}}{{if and .Secret $e.KeepSecrets}} <span class="muted small">(leave blank to keep the current value)</span>{{end}}</label><input type="{{.Type}}" name="cfg_{{$e.Provider}}_{{.Name}}" placeholder="{{.Placeholder}}"{{if not $e.Selected}} disabled{{end}}{{if and .Required (or (not .Secret) (not $e.KeepSecrets))}} required{{end}}{{if .Secret}} autocomplete="off"{{else}} value="{{index $e.Values .Name}}"{{end}}>{{end}}{{end}}{{end}}</div>{{end}}</form><div class="dialog-actions">{{if .Configured}}<div class="dialog-danger"><form method="post" action="/ui/inboxes/{{.InboxID}}/external-aliases/{{.AliasID}}/sending/clear" data-confirm="Remove this alias's sending connector? Mail from this alias will queue until a provider is set."><input type="hidden" name="_csrf" value="{{$.CSRF}}"><button class="secondary danger">Remove sending</button></form></div>{{end}}<button type="button" class="secondary" data-close-dialog>Cancel</button><button type="submit" form="external-alias-sending-form-{{.AliasID}}" data-save-provider{{if not .Selected}} disabled{{end}}>Save</button></div></dialog>{{end}}`

// externalAliasBody is the read-only activity log for one external sending
// alias, mirroring the domain log page. Name, connector and delete controls
// live on the dashboard (inbox edit dialog Aliases tab and connector popup).
const externalAliasBody = `<div class="toolbar"><a href="/?inbox={{.Inbox.ID}}">← Back to inbox settings</a></div>
<section class="card"><div class="msghead"><div><h1>{{.ExternalAlias.Address}}</h1><p class="muted">External sending alias · sending only. Mail addressed here remains with its email provider.</p></div></div>
</section>
<section class="card"><h2>Activity</h2>
<p class="muted">Every outbound send attempt from this alias, newest first. Attempts are retained for about 30 days.</p>
{{if .ExternalDeliveryAttempts}}<div class="table-wrap"><table class="log-table"><thead><tr><th>When</th><th>Status</th><th>From</th><th>To</th><th>Subject</th><th>Detail</th><th></th></tr></thead><tbody>{{range .ExternalDeliveryAttempts}}<tr><td style="white-space:nowrap">{{.CreatedAt.Format "2006-01-02 15:04"}}</td><td>{{if eq .Status "sent"}}<span class="pill">Sent</span>{{else}}<span class="pill danger">Failed</span>{{end}}</td><td>{{if .FromAddress}}{{.FromAddress}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .To}}{{join .To ", "}}{{else}}<span class="muted">—</span>{{end}}</td><td>{{if .Subject}}{{.Subject}}{{else}}<span class="muted">—</span>{{end}}</td><td class="muted log-detail">attempt {{.Attempt}}{{if .Provider}} · {{.Provider}}{{end}}{{if .ProviderMessageID}} · {{.ProviderMessageID}}{{end}}{{if .ErrorText}} · {{.ErrorText}}{{end}}</td><td>{{if .MessageID}}<a href="/ui/messages/{{.MessageID}}">Open</a>{{else}}<span class="muted">—</span>{{end}}</td></tr>{{end}}</tbody></table></div>{{if .ExternalLogHasMore}}<p><a href="/ui/inboxes/{{.Inbox.ID}}/external-aliases/{{.ExternalAlias.ID}}?before={{.ExternalLogBefore}}">Load older →</a></p>{{end}}{{else}}<p class="muted">No sends yet.</p>{{end}}
</section>`
