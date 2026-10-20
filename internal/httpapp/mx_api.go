package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/store"
)

// withInstallSession guards the installation-management API routes. Unlike the
// UI's withSession it answers with JSON rather than redirecting to the login
// page, because these are API operations: an unauthenticated request gets 401,
// an authenticated non-system-admin (including any account bearer request, which
// carries no session cookie) gets 403. Writes are then wrapped in the shared
// withCSRF middleware, so the token comes from the same session context the
// rest of the app uses.
func (s *Server) withInstallSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("mmm_session")
		if err != nil {
			writeError(w, 401, "system administrator session required")
			return
		}
		p, cval, err := s.Service.Store.SessionPrincipal(r.Context(), c.Value)
		if err != nil {
			writeError(w, 401, "system administrator session required")
			return
		}
		if !p.SystemAdmin {
			writeError(w, 403, "system administrator required")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, p)
		ctx = context.WithValue(ctx, csrfKey, cval)
		r = r.WithContext(ctx)
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		s.withCSRF(next)(w, r)
	}
}

// System-administrator MX receiver settings API. These routes edit the single
// installation-wide receiver configuration (included or remote) and report its
// live status. They are deliberately separate from the per-domain receiving
// provider config: a domain selects whether it receives by MX at all, while this
// installation setting decides how the core reaches the receiver. Only a system
// administrator may read or change them, because one receiver serves every
// account on the instance.
//
//	GET    /v1/admin/mx
//	PUT    /v1/admin/mx
//	DELETE /v1/admin/mx
//
// They authenticate with the system administrator's cookie session, not a
// bearer API key. An account-scoped bearer key (the /v1 default) must never
// reach installation state, and the store never marks an API key as a system
// administrator. Writes additionally require a CSRF token, matching the UI.
//
// A blank bearer_key on PUT retains the stored credential (included mode never
// accepts a caller-supplied key; the core generates and retains one). The
// revision is an optimistic-concurrency token: a save must echo the current
// revision, or zero to create the configuration when none exists.

// mxReceiverInputBody is the PUT body. It mirrors app.MXReceiverInput so a
// caller can round-trip a GET response into a save; the read-only fields the
// GET adds (key_configured, smtp_tls_key_configured, status, updated_at) are
// accepted and ignored, and a blank bearer_key or smtp_tls_key retains the
// stored secret. The private STARTTLS key is write-only: the GET never returns
// smtp_tls_key, so a round-trip cannot resend it.
type mxReceiverInputBody struct {
	Mode            string `json:"mode"`
	URL             string `json:"url"`
	BearerKey       string `json:"bearer_key"`
	CA              string `json:"ca"`
	Hostname        string `json:"hostname"`
	MaxMessageBytes int64  `json:"max_message_bytes"`
	MaxStagingBytes int64  `json:"max_staging_bytes"`
	MaxRecipients   int    `json:"max_recipients"`
	MaxConnections  int    `json:"max_connections"`
	RequireTLS      *bool  `json:"require_tls"`
	VerifySPF       *bool  `json:"verify_spf"`
	VerifyDKIM      *bool  `json:"verify_dkim"`
	VerifyDMARC     *bool  `json:"verify_dmarc"`
	// DNSResolver and the timeouts (seconds) override the included child
	// defaults.
	DNSResolver         string `json:"dns_resolver"`
	DNSTimeoutSeconds   int    `json:"dns_timeout_seconds"`
	ReadTimeoutSeconds  int    `json:"read_timeout_seconds"`
	WriteTimeoutSeconds int    `json:"write_timeout_seconds"`
	DataTimeoutSeconds  int    `json:"data_timeout_seconds"`
	// SMTPTLSCert is the public STARTTLS certificate (PEM) and SMTPTLSKey the
	// private key (PEM, write-only). A blank smtp_tls_key retains the stored key;
	// an empty smtp_tls_cert with a blank key clears both.
	SMTPTLSCert string `json:"smtp_tls_cert"`
	SMTPTLSKey  string `json:"smtp_tls_key"`
	Revision    int64  `json:"revision"`
	// Read-only fields echoed by GET and ignored on write.
	KeyConfigured        bool            `json:"key_configured"`
	SMTPTLSKeyConfigured bool            `json:"smtp_tls_key_configured"`
	UpdatedAt            json.RawMessage `json:"updated_at"`
	Status               json.RawMessage `json:"status"`
}

// apiMXReceiver serves the installation MX receiver settings:
//
//	GET    /v1/admin/mx   redacted settings plus live status
//	PUT    /v1/admin/mx   save (CAS)
//	DELETE /v1/admin/mx   clear
func (s *Server) apiMXReceiver(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.SystemAdmin {
		writeError(w, 403, "system administrator required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.Service.GetMXReceiverSettings(r.Context())
		if err != nil {
			mapMXError(w, err)
			return
		}
		status, err := s.Service.MXReceiverStatus(r.Context())
		if err != nil {
			mapMXError(w, err)
			return
		}
		writeMXJSON(w, 200, newMXSettingsResponse(settings, status))
	case http.MethodPut:
		var in mxReceiverInputBody
		if !decodeJSON(w, r, &in) {
			return
		}
		settings, err := s.Service.SaveMXReceiverSettings(r.Context(), p, app.MXReceiverInput{
			Mode:                in.Mode,
			URL:                 in.URL,
			BearerKey:           in.BearerKey,
			CA:                  in.CA,
			Hostname:            in.Hostname,
			MaxMessageBytes:     in.MaxMessageBytes,
			MaxStagingBytes:     in.MaxStagingBytes,
			MaxRecipients:       in.MaxRecipients,
			MaxConnections:      in.MaxConnections,
			RequireTLS:          in.RequireTLS,
			VerifySPF:           in.VerifySPF,
			VerifyDKIM:          in.VerifyDKIM,
			VerifyDMARC:         in.VerifyDMARC,
			DNSResolver:         in.DNSResolver,
			DNSTimeoutSeconds:   in.DNSTimeoutSeconds,
			ReadTimeoutSeconds:  in.ReadTimeoutSeconds,
			WriteTimeoutSeconds: in.WriteTimeoutSeconds,
			DataTimeoutSeconds:  in.DataTimeoutSeconds,
			SMTPTLSCert:         in.SMTPTLSCert,
			SMTPTLSKey:          in.SMTPTLSKey,
			Revision:            in.Revision,
		})
		if err != nil {
			mapMXError(w, err)
			return
		}
		status, err := s.Service.MXReceiverStatus(r.Context())
		if err != nil {
			mapMXError(w, err)
			return
		}
		writeMXJSON(w, 200, newMXSettingsResponse(settings, status))
	case http.MethodDelete:
		revision, err := mxRevisionQuery(r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := s.Service.ClearMXReceiverSettings(r.Context(), p, revision); err != nil {
			mapMXError(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// mxSettingsResponse is the flat JSON view: the redacted settings plus the
// observable runtime status. The bearer key is never present; key_configured
// reports whether one is stored and the status reports whether the receiver is
// actually live.
type mxSettingsResponse struct {
	app.MXReceiverSettings
	Status app.MXReceiverStatus `json:"status"`
}

func newMXSettingsResponse(settings app.MXReceiverSettings, status app.MXReceiverStatus) mxSettingsResponse {
	// The settings struct already omits the secret (BearerKey is always empty
	// on a read); Status is a separate object so a caller reads "configured"
	// from the settings and "ready" from the status.
	return mxSettingsResponse{MXReceiverSettings: settings, Status: status}
}

// mxRevisionQuery reads the required revision query parameter for DELETE. A
// missing or non-integer value is a client error; zero is rejected because
// clearing an unconfigured receiver is not a meaningful admin action.
func mxRevisionQuery(r *http.Request) (int64, error) {
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

// mapMXError maps MX settings failures to HTTP. Validation faults carry a
// user-safe message; a missing principal or internal fault is redacted.
func mapMXError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		writeError(w, 403, "system administrator required")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "the MX receiver configuration changed; reload and try again")
	case errors.Is(err, app.ErrMXInvalidInput):
		writeError(w, 400, err.Error())
	default:
		writeError(w, 500, "internal error")
	}
}

// writeMXJSON marks MX settings responses non-cacheable; the body carries
// installation-wide receiver state and a freshly generated credential is never
// echoed, but the response must still not be cached by an intermediary.
func writeMXJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}
