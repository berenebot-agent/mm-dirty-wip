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

// uiAccountMXSave handles the account Remote MX receiver editor submission. It
// validates through the same service path the API uses, so the form and the API
// cannot diverge. Only an account Admin may change it.
func (s *Server) uiAccountMXSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if s.Service.RemoteMXRuntime != nil {
		defer s.Service.RemoteMXRuntime.WakeRemoteMX()
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("rx_revision")), 10, 64)
	in := app.AccountMXReceiverInput{
		URL:          r.Form.Get("rx_url"),
		BearerKey:    r.Form.Get("rx_bearer_key"),
		CA:           r.Form.Get("rx_ca"),
		AllowPrivate: r.Form.Get("rx_allow_private") == "1" || strings.EqualFold(r.Form.Get("rx_allow_private"), "true"),
		Revision:     rev,
	}
	if _, err := s.Service.SaveAccountMXReceiver(r.Context(), p, in); err != nil {
		s.accountMXUIError(w, r, err)
		return
	}
	s.accountMXRedirect(w, r, "Remote MX receiver saved", "")
}

// uiAccountMXClear removes the account's Remote MX receiver. It is refused while
// a domain still routes to it, so a clear never silently breaks receiving.
func (s *Server) uiAccountMXClear(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.Admin {
		http.Error(w, "admin required", 403)
		return
	}
	if s.Service.RemoteMXRuntime != nil {
		defer s.Service.RemoteMXRuntime.WakeRemoteMX()
	}
	rev, _ := strconv.ParseInt(strings.TrimSpace(r.Form.Get("rx_revision")), 10, 64)
	if err := s.Service.ClearAccountMXReceiver(r.Context(), p, rev); err != nil {
		s.accountMXUIError(w, r, err)
		return
	}
	s.accountMXRedirect(w, r, "Remote MX receiver cleared", "")
}

// accountMXRedirect returns to the domain receiving dialog the account-level
// editor was opened from, so a save, clear or validation error lands back on the
// form instead of closing it. The domain is only honoured when it belongs to the
// caller's account; otherwise the dashboard is the destination.
func (s *Server) accountMXRedirect(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	values := url.Values{}
	if id := strings.TrimSpace(r.Form.Get("rx_domain")); id != "" {
		if _, err := s.Service.Store.GetDomain(r.Context(), principal(r).AccountID, id); err == nil {
			values.Set("domain", id)
			values.Set("kind", "receiving")
			values.Set("provider", "remotemx")
		}
	}
	if notice != "" {
		values.Set("notice", notice)
	}
	if errMsg != "" {
		values.Set("rx_error", errMsg)
	}
	dest := "/"
	if len(values) > 0 {
		dest += "?" + values.Encode()
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// accountMXUIError maps a Remote MX save/clear failure back to the originating
// domain dialog (or the dashboard) with a user-safe query message. The bearer key
// and private CA are never echoed.
func (s *Server) accountMXUIError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrAccountMXInvalidInput):
		s.accountMXRedirect(w, r, "", accountMXUserMessage(err))
	case errors.Is(err, store.ErrConflict):
		s.accountMXRedirect(w, r, "", "The Remote MX configuration changed in another session. Reload and try again.")
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, "admin required", 403)
	default:
		s.Log.Error("Remote MX receiver save failed", "error", err)
		http.Error(w, "could not save Remote MX receiver", 500)
	}
}

// accountMXUserMessage strips the sentinel prefix from a validation error so the
// form shows the operator-facing reason only.
func accountMXUserMessage(err error) string {
	msg := err.Error()
	return strings.TrimSpace(strings.TrimPrefix(msg, app.ErrAccountMXInvalidInput.Error()+":"))
}
