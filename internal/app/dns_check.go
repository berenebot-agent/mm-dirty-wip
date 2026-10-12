package app

import (
	"context"
	"encoding/base64"
	"net"
	"sort"
	"strings"
	"time"
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

// DNSRecordView is one published-record check. State is "ok", "pending" (nothing
// published yet) or "mismatch" (published but wrong). It is the shared shape the
// status view, the wizard and the API all render.
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
// The state is "mismatch" only when MX records exist but none is one of ours, and
// "pending" when none are published (or the lookup fails).
func CheckMX(parent context.Context, resolver DNSResolver, domain string, expected []string) DNSRecordView {
	view := DNSRecordView{Kind: "mx", Name: domain, Expected: strings.Join(expected, ", ")}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupMX(ctx, domain)
	if err != nil || len(records) == 0 {
		view.State, view.Reason = "pending", "no MX records published yet"
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
		view.State, view.Reason = "mismatch", "no expected MX hostname is published"
		return view
	}
	view.State = "ok"
	return view
}

// CheckTXT returns the current check for the domain's _mailmoose-mx TXT record
// against the exact key the receiver will require. expectedValue is the copy-ready
// record shown to the operator. The check is exact: exactly one MM1 record must be
// published for keyID with a public key equal to want.
func CheckTXT(parent context.Context, resolver DNSResolver, domain, keyID string, want []byte, expectedValue string) DNSRecordView {
	name := "_mailmoose-mx." + domain
	view := DNSRecordView{Kind: "txt", Name: name, Expected: expectedValue}
	ctx, cancel := withTimeout(parent)
	defer cancel()
	records, err := resolver.LookupTXT(ctx, name)
	if err != nil || len(records) == 0 {
		view.State, view.Reason = "pending", "TXT record not published yet"
		return view
	}
	kept := records
	if len(kept) > dnsMaxFound {
		kept = kept[:dnsMaxFound]
	}
	view.Found = kept
	if !txtMatches(records, keyID, want) {
		view.State, view.Reason = "mismatch", "the published TXT record does not match this key"
		return view
	}
	view.State = "ok"
	return view
}

func withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, dnsCheckTimeout)
}

// txtMatches reports whether exactly one MM1 key is published for keyID and its
// public key equals want. It is the same rule the receiver applies.
func txtMatches(records []string, keyID string, want []byte) bool {
	if keyID == "" || len(want) == 0 {
		return false
	}
	var found []byte
	matched := 0
	for _, raw := range records {
		fields := parseMM1(raw)
		if fields == nil || fields["v"] != "MM1" {
			continue
		}
		matched++
		if fields["id"] != keyID || fields["k"] != "ed25519" {
			return false
		}
		pub, err := base64.StdEncoding.Strict().DecodeString(fields["p"])
		if err != nil || len(pub) == 0 {
			return false
		}
		found = pub
	}
	return matched == 1 && string(found) == string(want)
}

// parseMM1 parses a single MM1 TXT record into its fields; nil when malformed.
func parseMM1(raw string) map[string]string {
	fields := map[string]string{}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" || fields[k] != "" {
			return nil
		}
		fields[k] = v
	}
	return fields
}
