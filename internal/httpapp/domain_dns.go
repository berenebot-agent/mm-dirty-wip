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

// dnsChecker performs the bounded, cached published-record checks behind the
// Dial MX traffic lights. Checks are asynchronous: a render returns the last
// known result (or a "checking" placeholder) and kicks a background refresh, so
// an open dialog or a dashboard never blocks on DNS. A resolver failure is
// reported as a check state, never as an API error.
type dnsChecker struct {
	resolver dnsResolver
	ttl      time.Duration
	now      func() time.Time

	mu         sync.Mutex
	entries    map[string]domainDNSView
	times      map[string]time.Time
	refreshing map[string]bool
	generation int
}

// DNSResolver is the small resolver surface the checker needs. *net.Resolver
// satisfies it; tests substitute a deterministic implementation.
type DNSResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

type dnsResolver = DNSResolver

const (
	dnsCheckTTL     = time.Minute
	dnsCheckTimeout = 4 * time.Second
	dnsMaxFound     = 4
)

func newDNSChecker() *dnsChecker {
	return &dnsChecker{
		resolver:   net.DefaultResolver,
		ttl:        dnsCheckTTL,
		now:        time.Now,
		entries:    map[string]domainDNSView{},
		times:      map[string]time.Time{},
		refreshing: map[string]bool{},
	}
}

// SetDNSResolver replaces the resolver used for published-record checks and
// drops the cache. It is for tests; production always uses the system resolver.
func (s *Server) SetDNSResolver(r DNSResolver) {
	if r == nil {
		return
	}
	s.dns.mu.Lock()
	s.dns.resolver = r
	s.dns.entries = map[string]domainDNSView{}
	s.dns.times = map[string]time.Time{}
	s.dns.refreshing = map[string]bool{}
	s.dns.generation++
	s.dns.mu.Unlock()
}

// checkMX returns the current check for the domain's MX records against the
// expected hostnames, starting a background refresh when the cached result is
// stale.
func (c *dnsChecker) checkMX(domain string, expected []dialMXMXInstruction) domainDNSView {
	key := "mx|" + domain + "|" + formatMXExpected(expected)
	pending := domainDNSView{Kind: "mx", Name: domain, Expected: formatMXExpected(expected), State: "pending", Reason: "checking published records"}
	return c.view(key, pending, func(resolver DNSResolver, ctx context.Context) domainDNSView {
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
			if !found[strings.ToLower(want.Hostname)] {
				view.State, view.Reason = "mismatch", "an expected MX hostname is not published"
				return view
			}
		}
		view.State = "ok"
		return view
	})
}

// checkTXT returns the current check for the domain's _mailmoose-mx TXT record
// against the exact key the receiver will require.
func (c *dnsChecker) checkTXT(domain, keyID string, want []byte, expectedValue string) domainDNSView {
	name := "_mailmoose-mx." + domain
	key := "txt|" + name + "|" + keyID
	pending := domainDNSView{Kind: "txt", Name: name, Expected: expectedValue, State: "pending", Reason: "checking published records"}
	return c.view(key, pending, func(resolver DNSResolver, ctx context.Context) domainDNSView {
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

// view returns the cached result when it is fresh; otherwise it starts one
// background refresh per key and returns the last known result, or the pending
// placeholder when no result has ever been computed. The resolver is snapshotted
// under the lock and handed to the refresh goroutine, so a concurrent resolver
// swap never races with an in-flight lookup.
func (c *dnsChecker) view(key string, pending domainDNSView, lookup func(DNSResolver, context.Context) domainDNSView) domainDNSView {
	now := c.now()
	c.mu.Lock()
	cached, ok := c.entries[key]
	if ok && now.Sub(c.times[key]) < c.ttl {
		c.mu.Unlock()
		return cached
	}
	if c.refreshing[key] {
		c.mu.Unlock()
		if ok {
			return cached
		}
		return pending
	}
	c.refreshing[key] = true
	resolver := c.resolver
	gen := c.generation
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), dnsCheckTimeout)
		defer cancel()
		result := lookup(resolver, ctx)
		c.mu.Lock()
		// A resolver swap (test hook) invalidates results from the old resolver.
		if c.generation == gen {
			c.entries[key] = result
			c.times[key] = c.now()
		}
		delete(c.refreshing, key)
		c.mu.Unlock()
	}()
	if ok {
		return cached
	}
	return pending
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
