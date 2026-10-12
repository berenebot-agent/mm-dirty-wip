package app

import (
	"context"
	"encoding/base64"
	"net"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// ExtraMXWatcher is the single owner of every Dial MX domain's published-record
// verdict: the `_mailmoose-mx` TXT check and the MX routing check (including the
// "extra MX" hosts that are not one of the domain's Antler receivers). It
// re-checks each Dial MX domain on a fixed interval, diffs the result against the
// last one it published, and publishes a transient mx.dns_changed event only when
// it changes. The dashboard light and the status dialog both read the same
// verdict, so they can never disagree, and neither runs a DNS lookup on a render.
//
// It checks a domain whenever it has at least one receiver configured (ready or
// not): a ready receiver needs the extra-MX verdict for the amber warning, and a
// non-ready receiver needs the TXT/MX checks to explain *why* it is red — the
// receiver's own single reason cannot, because it stops at the first failing gate.
type ExtraMXWatcher struct {
	svc      *Service
	resolver DNSResolver

	mu       sync.Mutex
	last     map[string]DNSVerdict // domainID -> last published verdict
	resolved map[string]bool       // domainID -> whether last is populated
}

// DNSState returns the last verdict the watcher published for a domain, and
// whether it has ever resolved one. It is the read side the HTTP layer uses so
// both the light and the dialog surface agree.
func (s *Service) DNSState(domainID string) (DNSVerdict, bool) {
	if s.ExtraMX == nil {
		return DNSVerdict{}, false
	}
	s.ExtraMX.mu.Lock()
	defer s.ExtraMX.mu.Unlock()
	if !s.ExtraMX.resolved[domainID] {
		return DNSVerdict{}, false
	}
	return s.ExtraMX.last[domainID], true
}

// ExtraMXState returns just the unexpected-MX host list from the shared verdict:
// published MX hostnames that are not one of the domain's Antler receivers. It is
// the projection the dashboard light turns amber on.
func (s *Service) ExtraMXState(domainID string) ([]string, bool) {
	v, ok := s.DNSState(domainID)
	if !ok || v.MX == nil {
		return nil, ok
	}
	return append([]string(nil), v.MX.Unexpected...), true
}

// NewExtraMXWatcher builds the watcher. A nil resolver defaults to the system
// resolver; tests inject a deterministic one via SetDNSResolver.
func NewExtraMXWatcher(svc *Service, resolver DNSResolver) *ExtraMXWatcher {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &ExtraMXWatcher{
		svc:      svc,
		resolver: resolver,
		last:     map[string]DNSVerdict{},
		resolved: map[string]bool{},
	}
}

// SetDNSResolver replaces the resolver the checks use. It exists for tests;
// production always uses the system resolver.
func (w *ExtraMXWatcher) SetDNSResolver(r DNSResolver) {
	if r == nil {
		return
	}
	w.mu.Lock()
	w.resolver = r
	w.mu.Unlock()
}

func (w *ExtraMXWatcher) currentResolver() DNSResolver {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resolver
}

// Run re-checks every Dial MX domain on a fixed interval until ctx is cancelled.
// It runs one pass immediately so a light is correct on startup.
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
	domains, err := w.svc.Store.ListDialMXDomains(ctx)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, d := range domains {
		seen[d.ID] = true
		verdict, ok := w.verdictForDomain(ctx, d.AccountID, d.ID, d.Name)
		if !ok {
			continue
		}
		w.setVerdict(d.ID, d.AccountID, verdict)
	}
	// A domain that is no longer Dial MX drops its stale verdict so the light
	// cannot stay amber or red from an old record set.
	w.mu.Lock()
	var stale []string
	for id := range w.resolved {
		if !seen[id] {
			stale = append(stale, id)
		}
	}
	w.mu.Unlock()
	for _, id := range stale {
		w.setVerdict(id, "", DNSVerdict{})
	}
}

// verdictForDomain resolves a domain's config and credential and computes the TXT
// and MX checks. ok is false when the domain has no resolvable config or
// credential (nothing to check).
func (w *ExtraMXWatcher) verdictForDomain(ctx context.Context, accountID, domainID, domainName string) (DNSVerdict, bool) {
	cfg, err := w.svc.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, mxdial.Provider)
	if err != nil {
		return DNSVerdict{}, false
	}
	values, err := w.svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return DNSVerdict{}, false
	}
	resolver := w.currentResolver()
	var verdict DNSVerdict

	// The TXT check needs the domain's exact key. A missing credential means the
	// domain has never minted one; skip rather than report a spurious mismatch.
	cred, err := w.svc.Store.GetDialMXCredential(ctx, accountID, domainID)
	if err == nil {
		pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
		if len(pub) > 0 {
			expectedValue := mxwire.DomainTXT(cred.KeyID, pub)
			txt := CheckTXT(ctx, resolver, domainName, cred.KeyID, pub, expectedValue)
			verdict.TXT = &txt
		}
	}

	// The MX check applies only to an Antler setup with configured receivers; a
	// custom-service domain leaves the MX hostnames to its own operator.
	receivers := AntlerReceiversFromConfig(values)
	expected := make([]string, 0, len(receivers))
	for _, r := range receivers {
		if r.SMTPHostname != "" {
			expected = append(expected, r.SMTPHostname)
		}
	}
	if len(expected) > 0 {
		mx := CheckMX(ctx, resolver, domainName, expected)
		// A lookup that could not be completed says nothing about the record set,
		// so it must not silently drop an already-known amber warning: the
		// unexpected hosts a previous pass observed are carried forward and the
		// light keeps its meaning until a real answer arrives. Only a completed
		// check may clear them.
		if mx.State == StateUnavailable {
			mx.Unexpected = w.carriedUnexpected(domainID)
		}
		verdict.MX = &mx
	}
	if verdict.TXT == nil && verdict.MX == nil {
		return DNSVerdict{}, false
	}
	return verdict, true
}

// carriedUnexpected returns the unexpected-MX host list from the domain's last
// published verdict, so an MX check that could not be completed keeps the warning
// a previous completed check established instead of clearing it.
func (w *ExtraMXWatcher) carriedUnexpected(domainID string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.last[domainID]
	if prev.MX == nil {
		return nil
	}
	return append([]string(nil), prev.MX.Unexpected...)
}

// setVerdict stores a domain's verdict and, when it changed, publishes the
// transient event to the owning account. An empty accountID clears the record
// without publishing (the domain left the Dial MX set).
func (w *ExtraMXWatcher) setVerdict(domainID, accountID string, verdict DNSVerdict) {
	w.mu.Lock()
	prev, had := w.last[domainID]
	changed := !had || !prev.Equal(verdict)
	w.last[domainID] = verdict
	w.resolved[domainID] = true
	w.mu.Unlock()
	// Publish only on a real change. The first verdict for a domain that is clean
	// (nothing published to fix) has nothing to announce; only a first verdict
	// that already flags a problem warrants an event.
	if !changed || accountID == "" {
		return
	}
	if !had && verdictClean(verdict) {
		return
	}
	w.svc.Hub.Publish(model.Event{
		Type:      model.EventMXDNSChanged,
		AccountID: accountID,
		Transient: true,
		Payload: map[string]any{
			"domain_id": domainID,
			"dns":       verdict,
		},
	})
}

// verdictClean reports whether a verdict flags no problem the operator must act
// on: both checks ok or absent. A check that could not be completed
// ("unavailable") is also treated as nothing to announce — there is no record to
// publish and no answer to act on, only a retry the watcher already owns.
func verdictClean(v DNSVerdict) bool {
	for _, r := range []*DNSRecordView{v.TXT, v.MX} {
		if r != nil && r.State != StateOK && r.State != StateUnavailable {
			return false
		}
	}
	return true
}
