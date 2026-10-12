package httpapp

import "net/http"

// uiDomainSendingDKIMStatus returns the live published-DKIM-record light for a
// Direct MX sending domain, so an open sending dialog can refresh its record dot
// without a page reload. It reads the watcher's shared cached verdict and asks
// for a re-check when that verdict has gone stale, so it never blocks on a
// resolver lookup. A domain that does not sign returns an empty light, which the
// browser treats as "no light".
func (s *Server) uiDomainSendingDKIMStatus(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if _, err := s.Service.Store.GetDomain(r.Context(), p.AccountID, id); err != nil {
		writeError(w, 404, "not found")
		return
	}
	light, title := s.dkimSendingLight(id)
	writeJSON(w, 200, map[string]any{"light": light, "title": title})
}
