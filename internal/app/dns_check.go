package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// DNSResolver is the small resolver surface the published-record checks need.
// *net.Resolver satisfies it; tests substitute a deterministic implementation.
type DNSResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// dnsCheckTimeout bounds a single lookup so a slow resolver cannot stall a render
// or a watcher tick indefinitely.
const dnsCheckTimeout = 4 * time.Second

// dnsMaxFound caps the published records reported per check.
const dnsMaxFound = 4

// DNSRecordView is one published-record check. State is "ok" (published and
// matching), "pending" (definitively not published yet), "mismatch" (published
// but wrong) or "unavailable" (the lookup could not be completed, so nothing is
// known). It is the shared shape the status view, the wizard and the API all
// render.
type DNSRecordView struct {
	Kind     string `json:"kind"` // "mx" | "txt"
	Name     string `json:"name"`
	Expected string `json:"expected"`
	// Found is the published record set shown to the operator (bounded).
	Found []string `json:"found,omitempty"`
	// Matched lists the expected receiver hostnames actually published (MX only).
	Matched []string `json:"matched,omitempty"`
	// Unexpected lists published hostnames that are not one of our receivers
	// (MX only); the aggregate light turns amber on them.
	Unexpected []string `json:"unexpected,omitempty"`
	State      string   `json:"state"`
	Reason     string   `json:"reason,omitempty"`
}

// Check states. "pending" and "unavailable" are deliberately distinct: a record
// that is definitely absent is something the operator must publish, while a
// lookup that could not be completed is something to retry. Presenting the
// second as the first tells the operator to create a record they may already
// have published correctly.
const (
	StateOK          = "ok"
	StatePending     = "pending"
	StateMismatch    = "mismatch"
	StateUnavailable = "unavailable"
)

// DNSVerdict is the combined published-record picture for one domain. Either
// check is nil when it does not apply (no Antler receivers → no MX check).
type DNSVerdict struct {
	TXT *DNSRecordView `json:"txt,omitempty"`
	MX  *DNSRecordView `json:"mx,omitempty"`
}

// Equal reports whether two verdicts are identical, so the watcher publishes only
// on a real change.
func (v DNSVerdict) Equal(o DNSVerdict) bool {
	return equalRecordViews(v.TXT, o.TXT) && equalRecordViews(v.MX, o.MX)
}

func equalRecordViews(a, b *DNSRecordView) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.Name == b.Name && a.Expected == b.Expected &&
		a.State == b.State && a.Reason == b.Reason &&
		equalStrings(a.Found, b.Found) && equalStrings(a.Matched, b.Matched) && equalStrings(a.Unexpected, b.Unexpected)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CheckMX returns the current check for a domain's MX records against the
// expected receiver hostnames.
//
// A domain's receiver set is redundancy, not an all-must-match set: the check is
// "ok" as long as at least one expected hostname is published, so an operator who
// points MX at a single Antler receiver is not dragged to a mismatch by the
// others. Matched lists which expected hostnames were found; Found is the
// published set; Unexpected lists published hostnames that are not our receivers.
// The state is "mismatch" only when MX records exist but none is one of ours,
// "pending" when the domain definitively has no MX records, and "unavailable"
// when the lookup itself could not be completed.
func CheckMX(parent context.Context, resolver DNSResolver, domain string, expected []string) DNSRecordView {
	view := DNSRecordView{Kind: "mx", Name: domain, Expected: strings.Join(expected, ", ")}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupMX(ctx, domain)
	if err != nil && !dnsNotFound(err) {
		view.State, view.Reason = StateUnavailable, "MX lookup failed; retrying"
		return view
	}
	// A definitive "no such records" answer and an empty record set are the same
	// fact to the operator: nothing is published yet.
	if len(records) == 0 {
		view.State, view.Reason = StatePending, "no MX records published yet"
		return view
	}
	known := map[string]bool{}
	for _, want := range expected {
		known[strings.ToLower(strings.TrimSuffix(want, "."))] = true
	}
	hosts := make([]string, 0, len(records))
	found := map[string]bool{}
	for _, mx := range records {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		hosts = append(hosts, host)
		found[host] = true
		if !known[host] {
			view.Unexpected = append(view.Unexpected, host)
		}
	}
	sort.Strings(view.Unexpected)
	if len(view.Unexpected) > dnsMaxFound {
		view.Unexpected = view.Unexpected[:dnsMaxFound]
	}
	sort.Strings(hosts)
	if len(hosts) > dnsMaxFound {
		hosts = hosts[:dnsMaxFound]
	}
	view.Found = hosts
	for _, want := range expected {
		if found[strings.ToLower(strings.TrimSuffix(want, "."))] {
			view.Matched = append(view.Matched, want)
		}
	}
	if len(view.Matched) == 0 {
		view.State, view.Reason = StateMismatch, "no expected MX hostname is published"
		return view
	}
	view.State = StateOK
	return view
}

// CheckTXT returns the current check for the domain's _mailmoose-mx TXT record
// against the exact key the receiver will require. expectedValue is the copy-ready
// record shown to the operator. The check is exact: exactly one MM1 record must be
// published for keyID with a public key equal to want.
//
// The record is parsed by the same mxwire.ParseDomainTXT the receiver applies, so
// the local check and the Antler receiver cannot disagree about whether a
// published record is valid: an extra field, a duplicate MM1 record or an
// oversized record fails here exactly as it fails there. A divergent second
// implementation is what let the dialog show a green "matching" record next to a
// receiver refusing with "key unavailable".
func CheckTXT(parent context.Context, resolver DNSResolver, domain, keyID string, want []byte, expectedValue string) DNSRecordView {
	name := "_mailmoose-mx." + domain
	view := DNSRecordView{Kind: "txt", Name: name, Expected: expectedValue}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupTXT(ctx, name)
	if err != nil && !dnsNotFound(err) {
		view.State, view.Reason = StateUnavailable, "TXT lookup failed; retrying"
		return view
	}
	if len(records) == 0 {
		view.State, view.Reason = StatePending, "TXT record not published yet"
		return view
	}
	kept := records
	if len(kept) > dnsMaxFound {
		kept = kept[:dnsMaxFound]
	}
	view.Found = kept
	pub, err := mxwire.ParseDomainTXT(records, keyID)
	if err != nil || len(want) == 0 || !bytes.Equal(pub, want) {
		view.State, view.Reason = StateMismatch, "the published TXT record does not match this key"
		return view
	}
	view.State = StateOK
	return view
}

func withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, dnsCheckTimeout)
}

// dnsNotFound reports whether a lookup error is a definitive "this name has no
// such records" answer (NODATA or NXDOMAIN) rather than a lookup that could not
// be completed. It mirrors the receiver's own mxNotFound test, so the local check
// and the receiver agree on when an absent record is a fact and when it is an
// unanswered question.
func dnsNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}
