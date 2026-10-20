package app

import (
	"context"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mx"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// dnsDemandFloor bounds how often a demand read may force a re-check of the same
// domain. It is a coalescing guard, not a polling rate: it stops N open tabs (or
// a wizard's own timer plus a dashboard tick) from turning one domain into a
// burst of resolver lookups, while still letting an operator watching a repair
// see it land within seconds.
const dnsDemandFloor = 10 * time.Second

// DNSWatcher is the single owner of every domain's published-record verdict:
// the receiving checks for a Dial MX domain (the `_mailmoose-mx` TXT check and
// the MX routing check, including the "extra MX" hosts that are not one of the
// domain's Antler receivers) and the sending check for a Direct MX domain that
// signs outbound mail (its `_domainkey` public-key record). It diffs each
// verdict against the last one it published and publishes a transient
// mx.dns_changed event only when it changes. The dashboard lights and the setup
// dialogs all read the same verdict, so they can never disagree, and none runs
// a DNS lookup on a render.
//
// Checks are demand-driven with a slow background pass: an idle instance re-checks
// every domain every `Run` interval (15 minutes in production), while a read that
// needs to be current — opening the status dialog or the wizard, the dashboard's
// lights tick, a save or a key rotation — asks for a check through RequestCheck
// (force) or RequestCheckIfStale (subject to dnsDemandFloor). Requests are
// coalesced into the watcher's own loop, so no HTTP request ever runs a lookup.
//
// A Dial MX domain is checked whenever it has at least one receiver configured
// (ready or not): a ready receiver needs the extra-MX verdict for the amber
// warning, and a non-ready receiver needs the TXT/MX checks to explain *why* it
// is red — the receiver's own single reason cannot, because it stops at the
// first failing gate. A Direct MX sending domain is checked only when it signs
// outbound mail, so a non-signing sender costs no lookup and renders no light.
type DNSWatcher struct {
	svc      *Service
	resolver DNSResolver

	mu sync.Mutex
	// last is the most recent published verdict per domain; resolved records
	// whether one has ever been published, so a never-checked domain is
	// distinguishable from one whose verdict is empty.
	last     map[string]DNSVerdict
	resolved map[string]bool
	// checkedAt is when each domain's verdict was last computed, and demanded is
	// the set of domains a read has asked to re-check. Both are owned by mu.
	checkedAt map[string]time.Time
	demanded  map[string]bool
	// demand wakes the Run loop to service a targeted re-check.
	demand chan struct{}
}

// DNSState returns the last verdict the watcher published for a domain, and
// whether it has ever resolved one. It is the read side the HTTP layer uses so
// both the light and the dialog surface agree.
func (s *Service) DNSState(domainID string) (DNSVerdict, bool) {
	if s.DNS == nil {
		return DNSVerdict{}, false
	}
	s.DNS.mu.Lock()
	defer s.DNS.mu.Unlock()
	if !s.DNS.resolved[domainID] {
		return DNSVerdict{}, false
	}
	return s.DNS.last[domainID], true
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

// NewDNSWatcher builds the watcher. A nil resolver defaults to the system
// resolver; tests inject a deterministic one via SetDNSResolver.
func NewDNSWatcher(svc *Service, resolver DNSResolver) *DNSWatcher {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &DNSWatcher{
		svc:       svc,
		resolver:  resolver,
		last:      map[string]DNSVerdict{},
		resolved:  map[string]bool{},
		checkedAt: map[string]time.Time{},
		demanded:  map[string]bool{},
		demand:    make(chan struct{}, 1),
	}
}

// wake nudges the Run loop to service an outstanding demand request without
// blocking; a pending wake absorbs a concurrent one.
func (w *DNSWatcher) wake() {
	select {
	case w.demand <- struct{}{}:
	default:
	}
}

// SetDNSResolver replaces the resolver the checks use. It exists for tests;
// production always uses the system resolver.
func (w *DNSWatcher) SetDNSResolver(r DNSResolver) {
	if r == nil {
		return
	}
	w.mu.Lock()
	w.resolver = r
	w.mu.Unlock()
}

func (w *DNSWatcher) currentResolver() DNSResolver {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resolver
}

// Run re-checks domains until ctx is cancelled. It runs one full pass immediately
// so a light is correct on startup, then a full pass on `every` (the slow
// background refresh), and a targeted pass whenever a read requests one. A zero
// or negative interval selects a one-minute background pass.
func (w *DNSWatcher) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	w.checkDomains(ctx, nil)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkDomains(ctx, nil)
			w.wake()
		case <-w.demand:
			demanded := w.takeDemanded()
			if len(demanded) == 0 {
				continue
			}
			w.checkDomains(ctx, demanded)
		}
	}
}

// RequestCheck forces a re-check of one domain as soon as the watcher can run it.
// It is used by a save or a key rotation, where the verdict is known to be stale.
// It never blocks and coalesces with an outstanding request.
func (w *DNSWatcher) RequestCheck(domainID string) {
	if w == nil || domainID == "" {
		return
	}
	w.mu.Lock()
	w.demanded[domainID] = true
	w.mu.Unlock()
	w.wake()
}

// RequestCheckIfStale asks for a re-check of one domain unless its verdict was
// computed less than dnsDemandFloor ago. It is the read-side signal (a dashboard
// lights tick, an open status dialog, a wizard step): cheap when repeated, a real
// lookup when the cached verdict has gone stale. It never blocks and never runs a
// lookup on the caller's goroutine.
func (w *DNSWatcher) RequestCheckIfStale(domainID string) {
	if w == nil || domainID == "" {
		return
	}
	w.mu.Lock()
	fresh := !w.checkedAt[domainID].IsZero() && time.Since(w.checkedAt[domainID]) < dnsDemandFloor
	if !fresh {
		w.demanded[domainID] = true
	}
	w.mu.Unlock()
	if !fresh {
		w.wake()
	}
}

// Stale reports whether a domain's verdict was last computed longer ago than the
// demand floor, i.e. whether a read had to queue a re-check to serve it. A read
// that served a stale verdict knows its picture is superseded; asking once more
// after the check has run is what turns the fresh answer into pixels.
func (w *DNSWatcher) Stale(domainID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkedAt[domainID].IsZero() || time.Since(w.checkedAt[domainID]) >= dnsDemandFloor
}

// takeDemanded returns the domains a read asked to re-check and clears the set.
func (w *DNSWatcher) takeDemanded() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.demanded) == 0 {
		return nil
	}
	out := w.demanded
	w.demanded = map[string]bool{}
	return out
}

// CheckOnce runs a single full pass. It is used on startup and by tests. A full
// pass supersedes any outstanding demand requests, so it clears them.
func (w *DNSWatcher) CheckOnce(ctx context.Context) {
	w.takeDemanded()
	w.checkDomains(ctx, nil)
}

// CheckDemandedOnce runs the pending demand requests only, as the Run loop does on
// a demand wake. It exists for tests, which cannot easily drive Run's channels.
func (w *DNSWatcher) CheckDemandedOnce(ctx context.Context) {
	demanded := w.takeDemanded()
	if len(demanded) == 0 {
		return
	}
	w.checkDomains(ctx, demanded)
}

// checkDomains evaluates the domains the watcher tracks and publishes changes. A
// nil filter checks every domain; a non-nil filter checks only the domains it
// names. The stale-verdict sweep runs only on a full pass, so a targeted check
// cannot decide a domain has left the tracked set.
//
// Two sets are tracked: Dial MX receiving domains (TXT + MX checks) and Direct MX
// sending domains (the DKIM check, only when signing is on). They are unioned by
// id so a domain that is in both is checked once.
func (w *DNSWatcher) checkDomains(ctx context.Context, filter map[string]bool) {
	byID := map[string]model.Domain{}
	receiving, err := w.svc.Store.ListDialMXDomains(ctx)
	if err != nil {
		return
	}
	for _, d := range receiving {
		byID[d.ID] = d
	}
	sending, err := w.svc.Store.ListDirectMXSendingDomains(ctx)
	if err != nil {
		return
	}
	for _, d := range sending {
		byID[d.ID] = d
	}
	seen := map[string]bool{}
	for _, d := range byID {
		seen[d.ID] = true
		if filter != nil && !filter[d.ID] {
			continue
		}
		verdict, ok := w.verdictForDomain(ctx, d.AccountID, d.ID, d.Name)
		if !ok {
			continue
		}
		w.setVerdict(d.ID, d.AccountID, verdict)
	}
	if filter != nil {
		return
	}
	// A domain that is no longer receiving by Dial MX or sending by Direct MX
	// drops its stale verdict so a light cannot stay amber or red from an old
	// record set.
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

// verdictForDomain resolves a domain's receiving and sending configs and
// computes whichever checks apply: the receiving TXT and MX checks for a Dial MX
// domain, and the DKIM check for a Direct MX domain that signs. ok is false when
// no check applies (nothing to publish or watch).
func (w *DNSWatcher) verdictForDomain(ctx context.Context, accountID, domainID, domainName string) (DNSVerdict, bool) {
	resolver := w.currentResolver()
	var verdict DNSVerdict
	w.addReceivingChecks(ctx, &verdict, accountID, domainID, domainName, resolver)
	w.addDKIMCheck(ctx, &verdict, accountID, domainID, resolver)
	if verdict.TXT == nil && verdict.MX == nil && verdict.DKIM == nil {
		return DNSVerdict{}, false
	}
	return verdict, true
}

// addReceivingChecks fills in the receiving TXT and MX checks for a Dial MX
// domain. It leaves the verdict untouched for a domain that does not receive by
// Dial MX.
func (w *DNSWatcher) addReceivingChecks(ctx context.Context, verdict *DNSVerdict, accountID, domainID, domainName string, resolver DNSResolver) {
	cfg, err := w.svc.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, mxdial.Provider)
	if err != nil {
		return
	}
	values, err := w.svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return
	}

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
		if mx.State == StateDNSError {
			mx.Unexpected = w.carriedUnexpected(domainID)
		}
		verdict.MX = &mx
	}
}

// addDKIMCheck fills in the DKIM published-record check for a Direct MX sending
// domain that signs outbound mail. It leaves the verdict untouched for a domain
// that does not send via Direct MX or that has signing off, so a non-signing
// sender costs no lookup and renders no light.
//
// The signing key lives on the domain row, so it is read there rather than from
// the sending configuration's copy; the sending configuration only supplies the
// enabled flag (and is the reason a domain with no Direct MX config is skipped
// entirely).
func (w *DNSWatcher) addDKIMCheck(ctx context.Context, verdict *DNSVerdict, accountID, domainID string, resolver DNSResolver) {
	cfg, err := w.svc.Store.ResolveDomainSendingConfig(ctx, accountID, domainID)
	if err != nil || !strings.EqualFold(cfg.Provider, mx.Provider) {
		return
	}
	values, err := w.svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(dkimString(values[mx.FieldDKIMEnabled])), "on") {
		return
	}
	key, err := w.svc.ServiceDomainDKIM(ctx, accountID, domainID)
	if err != nil {
		return
	}
	selector := strings.TrimSpace(key.Selector)
	if selector == "" {
		selector = dkimsign.DefaultSelector
	}
	domain := strings.TrimSpace(key.Domain)
	if domain == "" {
		return
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	parsed, err := dkimsign.ParsePrivateKey(key.PrivateKey)
	if err != nil {
		return
	}
	pubPEM, err := dkimsign.PublicKeyPEM(parsed)
	if err != nil {
		return
	}
	pubB64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		return
	}
	check := CheckDKIM(ctx, resolver, dkimsign.RecordName(selector, domain), dkimsign.TXTRecordValue(pubB64), pubB64)
	verdict.DKIM = &check
}

// dkimString reads a config value as a string, tolerant of a missing or
// non-string value (which yields ""), matching the assertion style used for the
// other provider config reads in this package.
func dkimString(v any) string {
	s, _ := v.(string)
	return s
}

// carriedUnexpected returns the unexpected-MX host list from the domain's last
// published verdict, so an MX check that could not be completed keeps the warning
// a previous completed check established instead of clearing it.
func (w *DNSWatcher) carriedUnexpected(domainID string) []string {
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
func (w *DNSWatcher) setVerdict(domainID, accountID string, verdict DNSVerdict) {
	w.mu.Lock()
	prev, had := w.last[domainID]
	changed := !had || !prev.Equal(verdict)
	w.last[domainID] = verdict
	w.resolved[domainID] = true
	if accountID != "" {
		w.checkedAt[domainID] = time.Now()
	}
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
// ("dns_error") is also treated as nothing to announce — there is no record to
// publish and no answer to act on, only a retry the watcher already owns.
func verdictClean(v DNSVerdict) bool {
	for _, r := range []*DNSRecordView{v.TXT, v.MX} {
		if r != nil && r.State != StateOK && r.State != StateDNSError {
			return false
		}
	}
	return true
}
