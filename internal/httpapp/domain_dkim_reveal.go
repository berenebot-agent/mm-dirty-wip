package httpapp

import (
	"errors"
	"net/http"
	"strings"
)

// dkimKeyFlash carries the DKIM private key from the reveal action back to the
// sending dialog, where it is shown in a pop-up. Like the other flashes it is
// bound to the account, user and domain, so a stale or foreign flash can never
// be displayed, and it is consumed on first read.
type dkimKeyFlash struct {
	AccountID  string
	UserID     string
	DomainID   string
	PrivateKey string
}

// uiDomainSendingDKIMReveal re-issues the domain's stored private key as a
// one-time flash and returns to the sending dialog, where it is shown as a
// pop-up. It is a POST behind CSRF so a cross-site request cannot pull the
// secret out through a navigation, and the value is served no-store and only
// once.
func (s *Server) uiDomainSendingDKIMReveal(w http.ResponseWriter, r *http.Request) {
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
	key, err := s.Service.ServiceDomainDKIM(ctx, p.AccountID, d.ID)
	if err != nil {
		s.uiError(w, err, 400)
		return
	}
	keyPEM := key.PrivateKey
	if strings.TrimSpace(keyPEM) == "" {
		s.uiError(w, errors.New("no DKIM signing key is stored for this domain"), 400)
		return
	}
	f := dkimKeyFlash{AccountID: p.AccountID, UserID: p.UserID, DomainID: d.ID, PrivateKey: keyPEM}
	tok := s.flashes.put(f, len(keyPEM)+64)
	if tok == "" {
		s.uiError(w, errors.New("could not prepare the key for display; try again"), 500)
		return
	}
	dest := "/?domain=" + d.ID + "&kind=sending&provider=mx&_flash=" + tok
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
