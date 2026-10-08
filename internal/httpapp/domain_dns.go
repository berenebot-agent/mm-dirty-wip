package httpapp

import (
	"context"
	"encoding/base64"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// dnsChecker performs the bounded published-record checks behind the Dial MX
// traffic lights. Every check is a fresh resolver lookup: results are never
// cached, so a record change is reflected on the next poll or an explicit
// "Check now". Each lookup is bounded by dnsCheckTimeout so a slow resolver
// cannot stall a render indefinitely, and a resolver failure is reported as a
// check state, never as an API error.
type dnsChecker struct {
	mu       sync.Mutex
	resolver dnsResolver
}

// DNSResolver is the small resolver surface the checker needs. *net.Resolver
// satisfies it; tests substitute a deterministic implementation.
type DNSResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type dnsResolver = DNSResolver

const (
	dnsCheckTimeout = 4 * time.Second
	dnsMaxFound     = 4
)

func newDNSChecker() *dnsChecker {
	return &dnsChecker{resolver: net.DefaultResolver}
}

// SetDNSResolver replaces the resolver used for published-record checks. It is
// for tests; production always uses the system resolver.
func (s *Server) SetDNSResolver(r DNSResolver) {
	if r == nil {
		return
	}
	s.dns.mu.Lock()
	s.dns.resolver = r
	s.dns.mu.Unlock()
}

// checkMX returns the current check for the domain's MX records against the
// expected hostnames, resolved fresh on every call.
//
// A domain's receiver set is redundancy, not an all-must-match set: the setup is
// "ok" as long as at least one expected hostname is published, so an operator who
// points MX at a single Antler receiver is not dragged to a mismatch by the
// other advertised receivers. Matched lists which expected hostnames were found
// (it is what the per-connector status lights read); Found is the published set
// shown to the operator. The state is "mismatch" only when MX records exist but
// none of them is one of our receivers.
func (c *dnsChecker) checkMX(domain string, expected []dialMXMXInstruction) domainDNSView {
	return c.view(func(resolver DNSResolver, ctx context.Context) domainDNSView {
		view := domainDNSView{Kind: "mx", Name: domain, Expected: formatMXExpected(expected)}
		records, err := resolver.LookupMX(ctx, domain)
		if err != nil || len(records) == 0 {
			view.State, view.Reason = "pending", "no MX records published yet"
			return view
		}
		hosts := make([]string, 0, len(records))
		found := map[string]bool{}
		for _, mx := range records {
			host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
			hosts = append(hosts, host)
			found[host] = true
		}
		sort.Strings(hosts)
		if len(hosts) > dnsMaxFound {
			hosts = hosts[:dnsMaxFound]
		}
		view.Found = hosts
		for _, want := range expected {
			if found[strings.ToLower(want.Hostname)] {
				view.Matched = append(view.Matched, want.Hostname)
			}
		}
		if len(view.Matched) == 0 {
			view.State, view.Reason = "mismatch", "no expected MX hostname is published"
			return view
		}
		view.State = "ok"
		return view
	})
}

// checkTXT returns the current check for the domain's _mailmoose-mx TXT record
// against the exact key the receiver will require, resolved fresh on every call.
func (c *dnsChecker) checkTXT(domain, keyID string, want []byte, expectedValue string) domainDNSView {
	name := "_mailmoose-mx." + domain
	return c.view(func(resolver DNSResolver, ctx context.Context) domainDNSView {
		view := domainDNSView{Kind: "txt", Name: name, Expected: expectedValue}
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
	})
}

// view runs one lookup against the current resolver, bounded by dnsCheckTimeout.
// Nothing is cached: the resolver is consulted afresh on every call so the
// traffic lights always reflect current DNS.
func (c *dnsChecker) view(lookup func(DNSResolver, context.Context) domainDNSView) domainDNSView {
	c.mu.Lock()
	resolver := c.resolver
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), dnsCheckTimeout)
	defer cancel()
	return lookup(resolver, ctx)
}

// txtMatches reports whether exactly one MM1 key is published for keyID and
// its public key equals want. It is the same rule the receiver applies.
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

func formatMXExpected(expected []dialMXMXInstruction) string {
	parts := make([]string, 0, len(expected))
	for _, mx := range expected {
		parts = append(parts, mx.Hostname)
	}
	return strings.Join(parts, ", ")
}
