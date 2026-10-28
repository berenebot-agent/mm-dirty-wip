package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/dkimsign"
	"github.com/dellarb/mammothmail/internal/mxwire"
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
// matching), "not_found" (definitively not published yet), "mismatch" (published
// but wrong) or "dns_error" (the lookup could not be completed, so nothing is
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

// Check states. "not_found" and "dns_error" are deliberately distinct: a record
// that is definitely absent is something the operator must publish, while a
// lookup that could not be completed says nothing about the record. Presenting
// the second as the first tells the operator to create a record they may already
// have published correctly.
//
// A settled record state is a hard verdict — for the receiving checks, correct
// or not correct, never "wait and see". "not_found" and "dns_error" are both
// red there because the operator must not proceed on either. The amber used on
// the dashboard belongs to the receiver's extra-MX warning, which is a *correct*
// DNS config with ambiguous routing, not to any per-record verdict.
const (
	StateOK       = "ok"
	StateNotFound = "not_found"
	StateMismatch = "mismatch"
	StateDNSError = "dns_error"
)

// DNSVerdict is the combined published-record picture for one domain. Either
// receiving check is nil when it does not apply (no Tusk receivers → no MX
// check); DKIM is nil when the domain does not sign outbound mail.
type DNSVerdict struct {
	TXT  *DNSRecordView `json:"txt,omitempty"`
	MX   *DNSRecordView `json:"mx,omitempty"`
	DKIM *DNSRecordView `json:"dkim,omitempty"`
}

// Equal reports whether two verdicts are identical, so the watcher publishes only
// on a real change.
func (v DNSVerdict) Equal(o DNSVerdict) bool {
	return equalRecordViews(v.TXT, o.TXT) && equalRecordViews(v.MX, o.MX) && equalRecordViews(v.DKIM, o.DKIM)
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
// points MX at a single Tusk receiver is not dragged to a mismatch by the
// others. Matched lists which expected hostnames were found; Found is the
// published set; Unexpected lists published hostnames that are not our receivers.
// The state is "mismatch" only when MX records exist but none is one of ours,
// "pending" when the domain definitively has no MX records, and "unavailable"
// when the lookup itself could not be completed.
func CheckMX(parent context.Context, resolver DNSResolver, domain string, expected []string) DNSRecordView {
	view := DNSRecordView{Kind: "mx", Name: domain, Expected: strings.Join(expected, ", ")}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupMX(ctx, absoluteName(domain))
	if err != nil && !dnsNotFound(err) {
		view.State, view.Reason = StateDNSError, "MX lookup failed; retrying"
		return view
	}
	// A definitive "no such records" answer and an empty record set are the same
	// fact to the operator: nothing is published yet.
	if len(records) == 0 {
		view.State, view.Reason = StateNotFound, "no MX records published yet"
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

// CheckTXT returns the current check for the domain's _mail-proof TXT record
// against the exact key the receiver will require. expectedValue is the copy-ready
// record shown to the operator. The check is exact: exactly one MM1 record must be
// published for keyID with a public key equal to want.
//
// The record is parsed by the same mxwire.ParseDomainTXT the receiver applies, so
// the local check and the Tusk receiver cannot disagree about whether a
// published record is valid: an extra field, a duplicate MM1 record or an
// oversized record fails here exactly as it fails there. A divergent second
// implementation is what let the dialog show a green "matching" record next to a
// receiver refusing with "key unavailable".
func CheckTXT(parent context.Context, resolver DNSResolver, domain, keyID string, want []byte, expectedValue string) DNSRecordView {
	name := "_mail-proof." + domain
	view := DNSRecordView{Kind: "txt", Name: name, Expected: expectedValue}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupTXT(ctx, absoluteName(name))
	if err != nil && !dnsNotFound(err) {
		view.State, view.Reason = StateDNSError, "TXT lookup failed; retrying"
		return view
	}
	if len(records) == 0 {
		view.State, view.Reason = StateNotFound, "TXT record not published yet"
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

// CheckDKIM returns the current check for a Direct MX sending domain's DKIM
// public-key TXT record against the key Mammoth Mail signs with. expectedValue is
// the copy-ready record shown to the operator. The check mirrors CheckMX's
// shape: "ok" when a record is published and its p= value equals wantBase64,
// "pending" when the name definitively carries no record yet, "mismatch" when a
// record is published but its key differs (a stale record from a rotated key, or
// another operator's record on the same name), and "unavailable" when the lookup
// could not be completed. It runs only for a signing domain, so a non-signing
// domain costs no resolver lookup.
func CheckDKIM(parent context.Context, resolver DNSResolver, recordName, expectedValue, wantBase64 string) DNSRecordView {
	view := DNSRecordView{Kind: "dkim", Name: recordName, Expected: expectedValue}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupTXT(ctx, absoluteName(recordName))
	if err != nil && !dnsNotFound(err) {
		view.State, view.Reason = StateDNSError, "DKIM TXT lookup failed; retrying"
		return view
	}
	if len(records) == 0 {
		view.State, view.Reason = StateNotFound, "DKIM record not published yet"
		return view
	}
	kept := records
	if len(kept) > dnsMaxFound {
		kept = kept[:dnsMaxFound]
	}
	view.Found = kept
	published, ok := dkimsign.ParseTXTRecordValue(records)
	if !ok {
		view.State, view.Reason = StateNotFound, "DKIM record not published yet"
		return view
	}
	if published != strings.TrimSpace(wantBase64) {
		view.State, view.Reason = StateMismatch, "the published DKIM record does not match this signing key"
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

// absoluteName makes a DNS name fully qualified by ensuring the root label.
//
// Every published-record lookup must query an absolute name. A relative name is
// subject to the resolver's search list (/etc/resolv.conf `search`), so Go tries
// the name and then <name>.<suffix> for each entry. When the bare name
// legitimately has no such record but a search suffix fails, the search-list
// error is what survives: a clean NODATA comes back as IsTemporary rather than
// IsNotFound, so dnsNotFound is false and an absent record reads as dns_error
// instead of not_found. That is exactly the "a brand new domain's TXT check
// never settles" symptom. The trailing dot skips search expansion and yields
// the real answer. The receiver's own proofs use the same rule
// (dialmx/receiver.absoluteName), so the local check and the receiver agree.
func absoluteName(d string) string {
	if d == "" || strings.HasSuffix(d, ".") {
		return d
	}
	return d + "."
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
