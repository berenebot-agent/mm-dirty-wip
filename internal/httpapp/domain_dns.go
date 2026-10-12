package httpapp

import (
	"github.com/dellarb/mailmoose/internal/app"
)

// dnsViewsFromVerdict converts the watcher's shared DNS verdict into the API's
// published-record check shape. The checks are computed by the watcher (once per
// interval, on change pushed over SSE), so every render reads the same in-memory
// verdict and runs no resolver lookup of its own. A nil verdict (never checked,
// or no config) yields no checks.
func dnsViewsFromVerdict(v app.DNSVerdict) []domainDNSView {
	var out []domainDNSView
	if v.TXT != nil {
		out = append(out, dnsView(*v.TXT))
	}
	if v.MX != nil {
		out = append(out, dnsView(*v.MX))
	}
	if v.DKIM != nil {
		out = append(out, dnsView(*v.DKIM))
	}
	return out
}

// dkimSendingLight returns the published-DKIM-record light for a Direct MX
// signing domain, read from the watcher's shared cached verdict (never a
// resolver lookup on render). It asks for a re-check when that verdict has gone
// stale. It returns "" when there is no DKIM check yet (signing off, or the
// watcher has not resolved the domain), so no light renders.
func (s *Server) dkimSendingLight(domainID string) (string, string) {
	if s.Service.DNS != nil {
		s.Service.DNS.RequestCheckIfStale(domainID)
	}
	v, ok := s.Service.DNSState(domainID)
	if !ok || v.DKIM == nil {
		return "", ""
	}
	view := dnsView(*v.DKIM)
	title := "DKIM: record published and matching"
	switch view.State {
	case app.StateMismatch:
		title = "DKIM: the published record does not match this signing key"
	case app.StatePending:
		title = "DKIM: record not published yet"
	case app.StateUnavailable:
		title = "DKIM: check could not be completed"
	}
	return dkimLight(&view), title
}

func dnsView(r app.DNSRecordView) domainDNSView {
	return domainDNSView{
		Kind:       r.Kind,
		Name:       r.Name,
		Expected:   r.Expected,
		Found:      r.Found,
		Matched:    r.Matched,
		Unexpected: r.Unexpected,
		State:      r.State,
		Reason:     r.Reason,
	}
}
