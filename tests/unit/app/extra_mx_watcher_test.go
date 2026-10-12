package app_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
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

// TestExtraMXWatcherPublishesDNSChanges proves the watcher is the single owner of
// a domain's published-record verdict: it computes the TXT and MX checks, publishes
// a transient mx.dns_changed event only when the verdict changes, and shares it via
// DNSState so the dashboard light and the status dialog read the same value.
func TestExtraMXWatcherPublishesDNSChanges(t *testing.T) {
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
	w := app.NewExtraMXWatcher(core.svc, res)
	core.svc.ExtraMX = w

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

// TestExtraMXWatcherKeepsWarningWhenLookupFails proves a failed MX lookup does
// not silently clear a known amber warning: the unexpected hosts a previous pass
// observed are carried forward, so the light cannot turn green because the
// resolver stopped answering.
func TestExtraMXWatcherKeepsWarningWhenLookupFails(t *testing.T) {
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
	w := app.NewExtraMXWatcher(core.svc, res)
	core.svc.ExtraMX = w
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
