package app

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// extraMXResolver is the small resolver surface the extra-MX check needs.
// *net.Resolver satisfies it; tests substitute a deterministic implementation.
type extraMXResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

const (
	// extraMXCheckTimeout bounds a single domain's MX lookup so a slow resolver
	// cannot stall the watcher tick.
	extraMXCheckTimeout = 4 * time.Second
	// extraMXMaxHosts caps the published hostnames reported per domain.
	extraMXMaxHosts = 4
)

// ExtraMXWatcher is the single owner of every Dial MX domain's "extra MX"
// verdict: the published MX hostnames that are not one of the domain's
// configured Antler receivers. It re-checks each domain with a ready receiver on
// a fixed interval, diffs the result against the last one it published, and
// publishes a transient mx.extra_mx_changed event only when the set changes. The
// dashboard light and the status dialog both read the same verdict, so they can
// never disagree, and neither runs its own DNS lookup on a render.
type ExtraMXWatcher struct {
	svc      *Service
	resolver extraMXResolver

	mu       sync.Mutex
	last     map[string][]string // domainID -> last published verdict
	resolved map[string]bool     // domainID -> whether last is populated
}

// ExtraMXState returns the last verdict the watcher published for a domain, and
// whether the watcher has ever resolved one. A domain with no entry (never
// checked, or not Dial MX) reads as an empty verdict. It is the read side the
// HTTP layer uses so both the light and the dialog surface agree.
func (s *Service) ExtraMXState(domainID string) ([]string, bool) {
	if s.ExtraMX == nil {
		return nil, false
	}
	s.ExtraMX.mu.Lock()
	defer s.ExtraMX.mu.Unlock()
	if !s.ExtraMX.resolved[domainID] {
		return nil, false
	}
	return append([]string(nil), s.ExtraMX.last[domainID]...), true
}

// NewExtraMXWatcher builds the watcher. A nil resolver defaults to the system
// resolver; tests inject a deterministic one via SetExtraMXResolver.
func NewExtraMXWatcher(svc *Service, resolver extraMXResolver) *ExtraMXWatcher {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &ExtraMXWatcher{
		svc:      svc,
		resolver: resolver,
		last:     map[string][]string{},
		resolved: map[string]bool{},
	}
}

// SetExtraMXResolver replaces the resolver the check uses. It exists for tests;
// production always uses the system resolver.
func (w *ExtraMXWatcher) SetExtraMXResolver(r extraMXResolver) {
	if r == nil {
		return
	}
	w.mu.Lock()
	w.resolver = r
	w.mu.Unlock()
}

// Run re-checks every Dial MX domain's MX records on a fixed interval until ctx
// is cancelled. It runs one pass immediately so a light is correct on startup.
// The pass is bounded: each domain gets one resolver lookup with its own timeout,
// and a domain with no ready receiver is skipped (a red domain cannot be amber).
func (w *ExtraMXWatcher) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	w.checkAll(ctx)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkAll(ctx)
		}
	}
}

// CheckOnce runs a single pass. It is used on startup and by tests.
func (w *ExtraMXWatcher) CheckOnce(ctx context.Context) { w.checkAll(ctx) }

// checkAll evaluates every Dial MX domain and publishes changes.
func (w *ExtraMXWatcher) checkAll(ctx context.Context) {
	if w.svc.DialMX == nil {
		return
	}
	domains, err := w.svc.Store.ListDialMXDomains(ctx)
	if err != nil {
		return
	}
	// Only domains with a live, unexpired ready receiver are candidates: a
	// receiver reports ready only after proving both authority and routing, so a
	// red domain cannot be amber-on-extra-MX and needs no lookup.
	ready := map[string]bool{}
	for _, d := range domains {
		if !anyReadyStatus(w.svc.DialMX.Status(d.Name), time.Now()) {
			continue
		}
		ready[d.ID] = true
	}
	for _, d := range domains {
		if !ready[d.ID] {
			// A domain that is no longer eligible drops any stale verdict so the
			// light cannot stay amber from an old record set.
			w.setVerdict(d.ID, "", nil)
			continue
		}
		hosts := w.extraMXForDomain(ctx, d.AccountID, d.ID, d.Name)
		w.setVerdict(d.ID, d.AccountID, hosts)
	}
}

// extraMXForDomain resolves a domain's configured Antler receivers and returns
// the published MX hostnames that are not among them. A missing config or
// credential, or a resolver failure, yields no verdict (not an error).
func (w *ExtraMXWatcher) extraMXForDomain(ctx context.Context, accountID, domainID, domainName string) []string {
	cfg, err := w.svc.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, "dialmx")
	if err != nil {
		return nil
	}
	values, err := w.svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return nil
	}
	receivers := AntlerReceiversFromConfig(values)
	expected := make([]string, 0, len(receivers))
	for _, r := range receivers {
		expected = append(expected, r.SMTPHostname)
	}
	if len(expected) == 0 {
		return nil
	}
	return w.unexpectedMXLoop(ctx, domainName, expected)
}

// unexpectedMXLoop performs the bounded MX lookup and returns the published
// hostnames not in expected (lower-cased, trailing dot trimmed). A resolver
// error yields no verdict.
func (w *ExtraMXWatcher) unexpectedMXLoop(ctx context.Context, domain string, expected []string) []string {
	w.mu.Lock()
	resolver := w.resolver
	w.mu.Unlock()
	lctx, cancel := context.WithTimeout(ctx, extraMXCheckTimeout)
	defer cancel()
	records, err := resolver.LookupMX(lctx, domain)
	if err != nil || len(records) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, want := range expected {
		known[strings.ToLower(strings.TrimSuffix(want, "."))] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, mx := range records {
		host := strings.TrimSuffix(strings.ToLower(mx.Host), ".")
		if known[host] || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	sort.Strings(out)
	if len(out) > extraMXMaxHosts {
		out = out[:extraMXMaxHosts]
	}
	return out
}

// setVerdict stores a domain's verdict and, when it changed, publishes the
// transient event to the owning account. An empty accountID clears the record
// without publishing (the domain left the Dial MX set).
func (w *ExtraMXWatcher) setVerdict(domainID, accountID string, hosts []string) {
	w.mu.Lock()
	prev, had := w.last[domainID]
	changed := !had || !equalStrings(prev, hosts)
	w.last[domainID] = append([]string(nil), hosts...)
	w.resolved[domainID] = true
	w.mu.Unlock()
	// Publish only on a real change. The first time a domain is seen with an
	// empty verdict there is nothing to announce; only a non-empty first verdict
	// (a domain that was already amber) warrants an event.
	if !changed || accountID == "" {
		return
	}
	if !had && len(hosts) == 0 {
		return
	}
	w.svc.Hub.Publish(model.Event{
		Type:      model.EventMXExtraMXChanged,
		AccountID: accountID,
		Transient: true,
		Payload: map[string]any{
			"domain_id":     domainID,
			"unexpected_mx": hosts,
		},
	})
}

// anyReadyStatus mirrors the aggregate light's readiness test: at least one
// unexpired ready receiver row.
func anyReadyStatus(statuses []mxdial.Status, now time.Time) bool {
	for _, st := range statuses {
		if st.State == mxdial.StatusActive && (st.ExpiresAt.IsZero() || st.ExpiresAt.After(now)) {
			return true
		}
	}
	return false
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
