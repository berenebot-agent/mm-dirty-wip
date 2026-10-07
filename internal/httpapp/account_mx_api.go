package httpapp

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/store"
)

// Account Remote MX receiver settings API. Unlike the installation MX routes,
// which are system-administrator and installation-wide, these edit the calling
// account's own single receiver and are available to any account Admin through
// the ordinary account-scoped bearer API (or the account-admin UI). One physical
// single-mode receiver belongs to exactly one account.
//
//	GET    /v1/admin/account/mx
//	PUT    /v1/admin/account/mx
//	DELETE /v1/admin/account/mx
//
// A blank bearer_key on PUT retains the stored credential. The revision is an
// optimistic-concurrency token: a save must echo the current revision, or zero
// to create the configuration when none exists.

type accountMXInputBody struct {
	URL          string `json:"url"`
	BearerKey    string `json:"bearer_key"`
	CA           string `json:"ca"`
	AllowPrivate bool   `json:"allow_private"`
	Revision     int64  `json:"revision"`
	// Read-only fields echoed by GET and ignored on write.
	KeyConfigured bool `json:"key_configured"`
}

type accountMXResponse struct {
	app.AccountMXReceiver
	Status app.AccountMXReceiverStatus `json:"status"`
}

// apiAccountMXReceiver serves the per-account Remote MX receiver settings.
func (s *Server) apiAccountMXReceiver(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.Service.GetAccountMXReceiver(r.Context(), p.AccountID)
		if err != nil {
			mapAccountMXError(w, err)
			return
		}
		writeAccountMXJSON(w, 200, s.accountMXResponse(r, p.AccountID, settings))
	case http.MethodPut:
		var in accountMXInputBody
		if !decodeJSON(w, r, &in) {
			return
		}
		settings, err := s.Service.SaveAccountMXReceiver(r.Context(), p, app.AccountMXReceiverInput{
			URL:          in.URL,
			BearerKey:    in.BearerKey,
			CA:           in.CA,
			AllowPrivate: in.AllowPrivate,
			Revision:     in.Revision,
		})
		if err != nil {
			mapAccountMXError(w, err)
			return
		}
		writeAccountMXJSON(w, 200, s.accountMXResponse(r, p.AccountID, settings))
	case http.MethodDelete:
		revision, err := accountMXRevisionQuery(r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := s.Service.ClearAccountMXReceiver(r.Context(), p, revision); err != nil {
			mapAccountMXError(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Server) accountMXResponse(r *http.Request, accountID string, settings app.AccountMXReceiver) accountMXResponse {
	resp := accountMXResponse{AccountMXReceiver: settings}
	if s.Service.RemoteMXRuntime != nil {
		resp.Status = s.Service.RemoteMXRuntime.RemoteMXStatus(r.Context(), accountID)
	} else {
		resp.Status = app.AccountMXReceiverStatus{Configured: settings.URL != "", Revision: settings.Revision, State: app.MXStateDisabled}
	}
	return resp
}

func accountMXRevisionQuery(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("revision"))
	if raw == "" {
		return 0, errors.New("revision is required")
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, errors.New("revision must be a positive integer")
	}
	return v, nil
}

// mapAccountMXError maps Remote MX settings failures to HTTP.
func mapAccountMXError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		writeError(w, 403, "account admin required")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "the Remote MX receiver configuration changed; reload and try again")
	case errors.Is(err, app.ErrAccountMXInvalidInput):
		writeError(w, 400, err.Error())
	default:
		writeError(w, 500, "internal error")
	}
}

func writeAccountMXJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}
