package httpapp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// challengeHeader carries the one-use ceremony token between the begin and
// finish steps. A header keeps the finish request body reserved for the raw
// PublicKeyCredential JSON that the WebAuthn library parses.
const challengeHeader = "X-WebAuthn-Challenge"

// webAuthnUser builds the credential-bearing view of a user for a ceremony.
// The WebAuthnID is the user's row id as bytes, which is stable across logins
// and unique across the installation.
func (s *Server) webAuthnUser(r *http.Request, u model.User) (*auth.WebAuthnUser, error) {
	creds, err := s.Service.Store.WebAuthnCredentialsForUser(r.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	return &auth.WebAuthnUser{
		ID:          []byte(u.ID),
		Name:        u.Email,
		DisplayName: u.Email,
		Credentials: toWebAuthnCredentials(creds),
	}, nil
}

// toWebAuthnCredentials converts stored rows into the library's credential
// representation used for exclusion lists and login verification. Backup
// eligibility and state must be reconstructed: the library hard-fails an
// assertion when the stored BackupEligible does not match the authenticator's
// flag, which is fixed at registration and set for synced passkeys.
func toWebAuthnCredentials(creds []model.WebAuthnCredential) []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(creds))
	for _, c := range creds {
		transports := make([]protocol.AuthenticatorTransport, 0, len(c.Transports))
		for _, t := range c.Transports {
			transports = append(transports, protocol.AuthenticatorTransport(t))
		}
		out = append(out, webauthn.Credential{
			ID:        c.CredentialID,
			PublicKey: c.PublicKey,
			Transport: transports,
			Flags: webauthn.CredentialFlags{
				BackupEligible: c.BackupEligible,
				BackupState:    c.BackupState,
			},
			Authenticator: webauthn.Authenticator{
				SignCount: c.SignCount,
			},
		})
	}
	return out
}

// webauthnUnavailable reports whether passkey support is configured. Handlers
// return a clear 501 rather than panicking when a deployment has no usable RP
// id.
func (s *Server) webauthnUnavailable(w http.ResponseWriter) bool {
	if s.webauthn == nil {
		writeError(w, http.StatusNotImplemented, "passkeys are not configured for this deployment")
		return true
	}
	return false
}

// uiPasskeyRegisterBegin starts a registration ceremony for the signed-in user.
func (s *Server) uiPasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.webauthnUnavailable(w) {
		return
	}
	p := principal(r)
	u, err := s.Service.Store.GetUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user not found")
		return
	}
	waUser, err := s.webAuthnUser(r, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load passkeys")
		return
	}
	opts, err := s.webauthn.BeginRegistration(waUser)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start passkey registration")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// uiPasskeyRegisterFinish validates the registration response and stores the
// new passkey. The human-readable name comes from the Name header so the body
// stays reserved for the credential JSON.
func (s *Server) uiPasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.webauthnUnavailable(w) {
		return
	}
	p := principal(r)
	u, err := s.Service.Store.GetUser(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user not found")
		return
	}
	waUser, err := s.webAuthnUser(r, u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load passkeys")
		return
	}
	token := r.Header.Get(challengeHeader)
	reg, err := s.webauthn.FinishRegistration(waUser, token, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "passkey registration failed")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	err = s.Service.Store.AddWebAuthnCredential(r.Context(), p.UserID, model.WebAuthnCredential{
		CredentialID: reg.Credential.ID,
		PublicKey:    reg.Credential.PublicKey,
		SignCount:    reg.Credential.Authenticator.SignCount,
		Transports:   transportsToStrings(reg.Credential.Transport),
		Name:         name,
	}, reg.AttestationType, aaguidString(reg.AAGUID), reg.BackupEligible, reg.BackupState)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "this passkey is already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not save passkey")
		return
	}
	// When the user chose to make this passkey their only sign-in method, disable
	// password authentication now that a passkey exists. The add above already
	// succeeded, so the account always has at least one working method. The
	// system administrator keeps password sign-in as a break-glass recovery
	// path, so the request is ignored for them.
	passwordOnly := false
	warning := ""
	if r.URL.Query().Get("only") == "1" && !u.SystemAdmin {
		if err := s.Service.Store.SetPasswordAuth(r.Context(), p.UserID, p.AccountID, false); err != nil {
			// The passkey is saved and the account still has a working method
			// (password), so this is not a failure: report success with a
			// warning rather than an error that invites a retry which would
			// then say "already registered".
			warning = "Passkey added, but password sign-in could not be turned off. Both sign-in methods remain available."
		} else {
			passwordOnly = true
		}
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.passkey_add", name)
	resp := map[string]any{"ok": true, "password_only": passwordOnly}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// uiPasskeyEnablePassword re-enables password sign-in for a passkey-only user
// who wants to remove the "only sign-in method" state. It refuses for the
// system administrator, whose password is config-owned.
func (s *Server) uiPasskeyEnablePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.SystemAdmin {
		s.settingsRedirect(w, r, "", "The system administrator's password is managed by the deployment configuration.")
		return
	}
	if err := s.Service.Store.SetPasswordAuth(r.Context(), p.UserID, p.AccountID, true); err != nil {
		s.settingsRedirect(w, r, "", "Could not re-enable password sign-in.")
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.password_enabled", "")
	s.settingsRedirect(w, r, "Password sign-in re-enabled. Set a new password below.", "")
}

// uiPasskeyRename changes a passkey's label.
func (s *Server) uiPasskeyRename(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_ = r.ParseForm()
	if err := s.Service.Store.RenameWebAuthnCredential(r.Context(), p.UserID, r.Form.Get("id"), r.Form.Get("name")); err != nil {
		s.settingsRedirect(w, r, "", "Could not rename passkey.")
		return
	}
	s.Service.Store.Audit(r.Context(), p.AccountID, "user.passkey_rename", "")
	s.settingsRedirect(w, r, "Passkey renamed.", "")
}

// uiPasskeyDelete removes a passkey, refusing when it is the user's only
// remaining sign-in method.
func (s *Server) uiPasskeyDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_ = r.ParseForm()
	err := s.Service.Store.DeleteWebAuthnCredential(r.Context(), p.UserID, r.Form.Get("id"))
	switch {
	case errors.Is(err, store.ErrLastAuthMethod):
		s.settingsRedirect(w, r, "", "This is your only sign-in method. Add a password or another passkey before removing it.")
	case err != nil:
		s.settingsRedirect(w, r, "", "Could not remove passkey.")
	default:
		s.Service.Store.Audit(r.Context(), p.AccountID, "user.passkey_remove", "")
		s.settingsRedirect(w, r, "Passkey removed.", "")
	}
}

// webauthnLoginBegin starts a usernameless passkey login. It is rate-limited by
// source address; the ceremony itself proves the user, so no email is required.
// The one-use ceremony token is returned in the JSON body and echoed back by the
// client in the X-WebAuthn-Challenge header on the finish step.
func (s *Server) webauthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.webauthnUnavailable(w) {
		return
	}
	if s.webauthnLoginLimiter != nil && !s.webauthnLoginLimiter.Allow(clientIP(r, s.Service.Config)) {
		writeError(w, http.StatusTooManyRequests, "too many passkey attempts, try again later")
		return
	}
	opts, err := s.webauthn.BeginDiscoverableLogin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start passkey login")
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

// webauthnLoginFinish validates the assertion, resolves the owning user, records
// the credential use, and mints a normal session.
func (s *Server) webauthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	if s.webauthnUnavailable(w) {
		return
	}
	ip := clientIP(r, s.Service.Config)
	if s.webauthnLoginLimiter != nil && !s.webauthnLoginLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many passkey attempts, try again later")
		return
	}
	token := r.Header.Get(challengeHeader)
	discovered, err := s.webauthn.FinishDiscoverableLogin(token, r, func(credentialID, _ []byte) (*auth.WebAuthnUser, error) {
		row, err := s.Service.Store.WebAuthnCredentialByCredentialID(r.Context(), credentialID)
		if err != nil {
			return nil, err
		}
		u, err := s.Service.Store.GetUser(r.Context(), row.UserID)
		if err != nil {
			return nil, err
		}
		return s.webAuthnUser(r, u)
	})
	if err != nil {
		writeError(w, http.StatusUnauthorized, "passkey login failed")
		return
	}
	// Resolve the user from the user handle the ceremony returned, which the
	// library verified against the stored credential.
	u, err := s.Service.Store.GetUser(r.Context(), string(discovered.UserHandle))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "passkey login failed")
		return
	}
	if cloneWarning, uerr := s.Service.Store.UpdateWebAuthnCredentialUse(r.Context(), discovered.Credential.ID, discovered.Credential.Authenticator.SignCount, discovered.Credential.Flags.BackupState); uerr != nil {
		// A failure to record use is not fatal to the login, but a clone
		// warning is worth a server-side note.
		s.Log.Warn("passkey use not recorded", "user_id", u.ID, "error", uerr)
	} else if cloneWarning {
		s.Log.Warn("passkey signature counter did not advance", "user_id", u.ID)
	}
	sess, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	s.setSessionCookie(w, r, sess)
	s.Service.Store.Audit(r.Context(), u.AccountID, "user.login", "passkey")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": safeNextPath(r.URL.Query().Get("next"))})
}

func transportsToStrings(ts []protocol.AuthenticatorTransport) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}

// aaguidString renders the authenticator AAGUID as a canonical UUID string for
// display/diagnostics; an all-zero or empty AAGUID yields "".
func aaguidString(aaguid []byte) string {
	if len(aaguid) != 16 {
		return ""
	}
	allZero := true
	for _, b := range aaguid {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	return formatUUID(aaguid)
}

func formatUUID(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexdigits[v>>4], hexdigits[v&0x0f])
	}
	return string(out)
}
