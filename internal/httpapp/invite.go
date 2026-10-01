package httpapp

import (
	"errors"
	"net/http"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/store"
)

const inviteBody = `<div class="card" style="max-width:460px;margin:60px auto"><h1>Set your password</h1>{{if .Error}}<div class="error">{{.Error}}</div>{{end}}<p class="muted">Finish setting up the account for <b>{{.Email}}</b>.</p><form method="post"><input type="hidden" name="_csrf" value="{{.CSRF}}"><label>Password</label><input type="password" name="password" minlength="10" required autocomplete="new-password"><label>Confirm password</label><input type="password" name="confirm_password" minlength="10" required autocomplete="new-password"><button>Set password &amp; sign in</button></form></div>`

// inviteInvalidBody is shown for an unknown, expired, revoked or already-used
// setup token. It never reveals which case applies.
const inviteInvalidBody = `<div class="card" style="max-width:460px;margin:60px auto"><h1>Invitation not available</h1><p>This invitation link is invalid, expired, or has already been used.</p><p><a href="/login">Log in</a></p></div>`

func (s *Server) inviteGet(w http.ResponseWriter, r *http.Request) {
	inv, err := s.Service.Store.GetInviteByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.render(w, r, inviteInvalidBody, pageData{Title: "Invitation"})
		return
	}
	s.render(w, r, inviteBody, pageData{Title: "Set your password", CSRF: s.setPreAuthCSRF(w, r), Email: inv.Email})
}

func (s *Server) invitePost(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	_ = r.ParseForm()
	password := r.Form.Get("password")
	renderErr := func(msg string) {
		inv, err := s.Service.Store.GetInviteByToken(r.Context(), token)
		if err != nil {
			s.render(w, r, inviteInvalidBody, pageData{Title: "Invitation"})
			return
		}
		s.render(w, r, inviteBody, pageData{Title: "Set your password", CSRF: s.setPreAuthCSRF(w, r), Email: inv.Email, Error: msg})
	}
	if password != r.Form.Get("confirm_password") {
		renderErr("Passwords do not match.")
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		renderErr(err.Error())
		return
	}
	u, err := s.Service.Store.RedeemInvite(r.Context(), token, password)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInviteExpired):
			s.render(w, r, inviteInvalidBody, pageData{Title: "Invitation"})
		case errors.Is(err, store.ErrConflict):
			renderErr("That email address is already in use.")
		default:
			renderErr("Could not complete setup: " + err.Error())
		}
		return
	}
	tok, _, err := s.Service.Store.CreateSession(r.Context(), u.ID, s.Service.Config.SessionTTL)
	if err != nil {
		http.Error(w, "session error", 500)
		return
	}
	s.setSessionCookie(w, r, tok)
	http.Redirect(w, r, "/", 303)
}
