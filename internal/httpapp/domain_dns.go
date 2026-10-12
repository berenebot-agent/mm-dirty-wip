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
	return out
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
