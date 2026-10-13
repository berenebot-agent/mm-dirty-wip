package app_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mx"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// switchDNSResolver serves a settable MX set and TXT set to the watcher.
type switchDNSResolver struct {
	hosts []string
	txt   []string
}

func (s *switchDNSResolver) setMX(hosts ...string) { s.hosts = hosts }
func (s *switchDNSResolver) setTXT(txt ...string)  { s.txt = txt }

func (s *switchDNSResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	out := make([]*net.MX, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, &net.MX{Host: h, Pref: 10})
	}
	return out, nil
}

func (s *switchDNSResolver) LookupTXT(context.Context, string) ([]string, error) {
	return s.txt, nil
}

// countingResolver counts lookups so a test can prove a read runs no resolver
// work itself and that a demand read coalesces.
type countingResolver struct {
	mu    sync.Mutex
	mx    int
	txt   int
	hosts []string
}

func (c *countingResolver) setMX(hosts ...string) {
	c.mu.Lock()
	c.hosts = hosts
	c.mu.Unlock()
}

func (c *countingResolver) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mx, c.txt
}

func (c *countingResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mx++
	out := make([]*net.MX, 0, len(c.hosts))
	for _, h := range c.hosts {
		out = append(out, &net.MX{Host: h, Pref: 10})
	}
	return out, nil
}

func (c *countingResolver) LookupTXT(context.Context, string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.txt++
	return nil, nil
}

// TestDNSWatcherDemandCoalesces proves a repeated demand read inside the
// floor does not force repeated lookups: the wizard's own 10s poll plus a
// dashboard tick must not turn one domain into a resolver burst, while a read
// past the floor does ask for a fresh check.
func TestDNSWatcherDemandCoalesces(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	core.svc.AntlerEndpoints = fixedAntlerReceivers{{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10}}
	if _, _, err := core.svc.SaveDomainReceivingConfig(ctx, core.user.AccountID, core.domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}

	res := &countingResolver{}
	res.setMX("antler1.example.test.")
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w
	// The save already requested a check; run it so checkedAt is set.
	w.CheckOnce(ctx)
	if mx, _ := res.counts(); mx == 0 {
		t.Fatal("no MX lookup on the first pass")
	}

	// Repeated demand reads immediately after settle must not add lookups: the
	// request is coalesced behind the floor and no domain is left demanded.
	beforeMX, _ := res.counts()
	for i := 0; i < 20; i++ {
		w.RequestCheckIfStale(core.domain.ID)
	}
	w.CheckDemandedOnce(ctx)
	if afterMX, _ := res.counts(); afterMX != beforeMX {
		t.Fatalf("a demand read inside the floor forced a lookup: %d -> %d", beforeMX, afterMX)
	}
}

// TestDNSWatcherRequestCheckForces proves a forced request re-checks a domain
// even inside the floor, so a save or a key rotation is reflected at once.
func TestDNSWatcherRequestCheckForces(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	core.svc.AntlerEndpoints = fixedAntlerReceivers{{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10}}
	if _, _, err := core.svc.SaveDomainReceivingConfig(ctx, core.user.AccountID, core.domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	res := &countingResolver{}
	res.setMX("antler1.example.test.")
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w
	w.CheckOnce(ctx)

	beforeMX, _ := res.counts()
	w.RequestCheck(core.domain.ID)
	w.CheckDemandedOnce(ctx)
	if afterMX, _ := res.counts(); afterMX == beforeMX {
		t.Fatalf("a forced request did not re-check: %d -> %d", beforeMX, afterMX)
	}
}

// TestDNSWatcherPublishesDNSChanges proves the watcher is the single owner of
// a domain's published-record verdict: it computes the TXT and MX checks, publishes
// a transient mx.dns_changed event only when the verdict changes, and shares it via
// DNSState so the dashboard light and the status dialog read the same value.
func TestDNSWatcherPublishesDNSChanges(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	core.svc.AntlerEndpoints = fixedAntlerReceivers{{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10}}
	if _, _, err := core.svc.SaveDomainReceivingConfig(ctx, core.user.AccountID, core.domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := core.svc.Store.GetDialMXCredential(ctx, core.user.AccountID, core.domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)

	res := &switchDNSResolver{}
	res.setMX("antler1.example.test.")
	res.setTXT(mxwire.DomainTXT(cred.KeyID, pub))
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w

	_, ch, cancel := core.svc.Hub.Subscribe(64)
	defer cancel()
	drain := func() []model.Event {
		var out []model.Event
		for {
			select {
			case ev := <-ch:
				out = append(out, ev)
			default:
				return out
			}
		}
	}

	// First pass: TXT and MX both clean, so the verdict is clean and no event is
	// published (there is nothing to announce).
	w.CheckOnce(ctx)
	if v, ok := core.svc.DNSState(core.domain.ID); !ok || v.TXT == nil || v.TXT.State != "ok" || v.MX == nil || v.MX.State != "ok" {
		t.Fatalf("initial verdict = %+v (ok=%v), want clean TXT+MX", v, ok)
	}
	for _, ev := range drain() {
		if ev.Type == model.EventMXDNSChanged {
			t.Fatalf("no event expected for a clean first verdict: %+v", ev)
		}
	}

	// A rogue MX record appears: the MX verdict gains an unexpected host and one
	// transient event carries it.
	res.setMX("antler1.example.test.", "old.example.test.")
	w.CheckOnce(ctx)
	hosts, ok := core.svc.ExtraMXState(core.domain.ID)
	if !ok || len(hosts) != 1 || hosts[0] != "old.example.test" {
		t.Fatalf("extra-MX after rogue record = %v, want [old.example.test]", hosts)
	}
	events := drain()
	var sawEvent bool
	for _, ev := range events {
		if ev.Type != model.EventMXDNSChanged {
			continue
		}
		sawEvent = true
		if !ev.Transient {
			t.Fatalf("dns event must be transient so the stream emits no id: line")
		}
		if ev.AccountID != core.user.AccountID {
			t.Fatalf("event account = %q, want %q", ev.AccountID, core.user.AccountID)
		}
	}
	if !sawEvent {
		t.Fatal("no mx.dns_changed event published for a changed verdict")
	}

	// A repeat pass with the same DNS publishes nothing (diffed).
	w.CheckOnce(ctx)
	for _, ev := range drain() {
		if ev.Type == model.EventMXDNSChanged {
			t.Fatalf("unchanged verdict must not republish: %+v", ev)
		}
	}

	// The TXT record breaks: the verdict now flags the TXT mismatch even though the
	// receiver's own single reason (if it reported one) could not show both.
	res.setTXT("v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub))
	w.CheckOnce(ctx)
	v, ok := core.svc.DNSState(core.domain.ID)
	if !ok || v.TXT == nil || v.TXT.State != "mismatch" {
		t.Fatalf("TXT verdict after break = %+v, want mismatch", v.TXT)
	}
	if v.MX == nil || v.MX.State != "ok" || len(v.MX.Unexpected) != 1 {
		t.Fatalf("MX verdict should still flag the rogue host: %+v", v.MX)
	}
}

// fixedAntlerReceivers is a deterministic Antler endpoint resolver for the watcher.
type fixedAntlerReceivers []mxdial.AntlerReceiver

func (f fixedAntlerReceivers) Receivers(context.Context) ([]mxdial.AntlerReceiver, error) {
	return f, nil
}

// errResolver fails every lookup with a non-notfound error, modelling a resolver
// that cannot answer (timeout, SERVFAIL) rather than one that says "no records".
type errResolver struct{ err error }

func (e errResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return nil, e.err }
func (e errResolver) LookupTXT(context.Context, string) ([]string, error) { return nil, e.err }

// TestDNSWatcherKeepsWarningWhenLookupFails proves a failed MX lookup does
// not silently clear a known amber warning: the unexpected hosts a previous pass
// observed are carried forward, so the light cannot turn green because the
// resolver stopped answering.
func TestDNSWatcherKeepsWarningWhenLookupFails(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	core.svc.AntlerEndpoints = fixedAntlerReceivers{{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10}}
	if _, _, err := core.svc.SaveDomainReceivingConfig(ctx, core.user.AccountID, core.domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := core.svc.Store.GetDialMXCredential(ctx, core.user.AccountID, core.domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)

	res := &switchDNSResolver{}
	res.setMX("antler1.example.test.", "old.example.test.")
	res.setTXT(mxwire.DomainTXT(cred.KeyID, pub))
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w
	w.CheckOnce(ctx)

	// A completed pass sees the rogue host.
	if hosts, ok := core.svc.ExtraMXState(core.domain.ID); !ok || len(hosts) != 1 || hosts[0] != "old.example.test" {
		t.Fatalf("extra-MX = %v, want [old.example.test]", hosts)
	}

	// The resolver now fails. The verdict must keep the known warning rather than
	// reporting a clean domain it cannot vouch for.
	w.SetDNSResolver(errResolver{err: errTestDNSFailure})
	w.CheckOnce(ctx)
	hosts, ok := core.svc.ExtraMXState(core.domain.ID)
	if !ok || len(hosts) != 1 || hosts[0] != "old.example.test" {
		t.Fatalf("extra-MX after a failed lookup = %v (ok=%v), want the carried [old.example.test]", hosts, ok)
	}
	v, _ := core.svc.DNSState(core.domain.ID)
	if v.MX == nil || v.MX.State != app.StateUnavailable {
		t.Fatalf("MX check after a failed lookup = %+v, want unavailable", v.MX)
	}
}

// errTestDNSFailure stands in for a resolver that cannot complete a lookup.
var errTestDNSFailure = errors.New("resolver unavailable")

// dkimExpectedTXT derives the copy-ready DKIM record value the watcher checks
// against, from the domain's stored signing key, so a test fixture matches
// exactly what the operator would publish.
func dkimExpectedTXT(t *testing.T, svc *app.Service, accountID, domainID string) string {
	t.Helper()
	key, err := svc.ServiceDomainDKIM(context.Background(), accountID, domainID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := dkimsign.ParsePrivateKey(key.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, err := dkimsign.PublicKeyPEM(parsed)
	if err != nil {
		t.Fatal(err)
	}
	pubB64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	return dkimsign.TXTRecordValue(pubB64)
}

// TestDNSWatcherChecksDKIMForSigningDomain proves the watcher extends its
// published-record verdict to the sending side: a Direct MX domain that signs
// gets a DKIM check (green when the published record matches, red when it does
// not), while a domain with signing off gets no DKIM check and costs no lookup.
func TestDNSWatcherChecksDKIMForSigningDomain(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	if _, err := core.svc.SaveDomainSendingConfig(ctx, core.user.AccountID, core.domain.ID, mx.Provider, map[string]any{
		"helo": "mail.example.test", mx.FieldDKIMEnabled: "on",
	}); err != nil {
		t.Fatalf("direct mx sending save: %v", err)
	}

	expected := dkimExpectedTXT(t, core.svc, core.user.AccountID, core.domain.ID)
	res := &switchDNSResolver{}
	res.setTXT(expected)
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w
	w.CheckOnce(ctx)

	v, ok := core.svc.DNSState(core.domain.ID)
	if !ok || v.DKIM == nil {
		t.Fatalf("no DKIM verdict for a signing domain: ok=%v verdict=%+v", ok, v)
	}
	if v.DKIM.State != app.StateOK {
		t.Fatalf("published matching record = %q, want ok", v.DKIM.State)
	}

	// A record for a different key is a mismatch (red), not green.
	res.setTXT("v=DKIM1; k=rsa; p=DIFFERENTKEY")
	w.CheckOnce(ctx)
	v, _ = core.svc.DNSState(core.domain.ID)
	if v.DKIM == nil || v.DKIM.State != app.StateMismatch {
		t.Fatalf("published wrong record = %+v, want mismatch", v.DKIM)
	}
}

// TestDNSWatcherSkipsDKIMWhenSigningOff proves a Direct MX domain with signing
// off is not checked for a DKIM record at all, so it renders no light and costs
// no lookup.
func TestDNSWatcherSkipsDKIMWhenSigningOff(t *testing.T) {
	t.Parallel()
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	if _, err := core.svc.SaveDomainSendingConfig(ctx, core.user.AccountID, core.domain.ID, mx.Provider, map[string]any{
		"helo": "mail.example.test", mx.FieldDKIMEnabled: "off",
	}); err != nil {
		t.Fatalf("direct mx sending save: %v", err)
	}
	res := &switchDNSResolver{}
	w := app.NewDNSWatcher(core.svc, res)
	core.svc.DNS = w
	w.CheckOnce(ctx)

	// Nothing is watchable for this domain (no receiving config, signing off),
	// so no verdict is stored.
	if v, ok := core.svc.DNSState(core.domain.ID); ok {
		t.Fatalf("expected no verdict for a non-receiving, non-signing domain, got %+v", v)
	}
}
