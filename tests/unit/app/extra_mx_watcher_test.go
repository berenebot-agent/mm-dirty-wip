package app_test

import (
	"context"
	"net"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// switchMXResolver serves a settable MX set to the extra-MX watcher.
type switchMXResolver struct {
	hosts []string
}

func (s *switchMXResolver) set(hosts ...string) { s.hosts = hosts }

func (s *switchMXResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	out := make([]*net.MX, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, &net.MX{Host: h, Pref: 10})
	}
	return out, nil
}

// TestExtraMXWatcherPublishesChangesOnReadyDomain proves the watcher is the single
// owner of a domain's extra-MX verdict: it resolves once, publishes a transient
// mx.extra_mx_changed event only when the set changes, and shares that verdict via
// ExtraMXState so the dashboard light and status dialog read the same value.
func TestExtraMXWatcherPublishesChangesOnReadyDomain(t *testing.T) {
	core, _ := dialMXDomain(t, "example.test", "hermes")
	ctx := context.Background()
	core.svc.AntlerEndpoints = fixedAntlerReceivers{{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10}}
	if _, _, err := core.svc.SaveDomainReceivingConfig(ctx, core.user.AccountID, core.domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	// A ready receiver, or the domain is not a candidate and no verdict is made.
	m := mxdial.New(core.svc.DialMXBackend(), mxdial.Config{DataDir: core.svc.Config.DataDir, AllowPrivateDestinations: true})
	m.SetStatusForTest(core.domain.Name, []mxdial.Status{{
		ReceiverURL: "https://antler1.example.test", State: mxdial.StatusActive, SMTPHostname: "antler1.example.test",
	}})
	core.svc.DialMX = m

	res := &switchMXResolver{}
	res.set("antler1.example.test.")
	w := app.NewExtraMXWatcher(core.svc, res)
	core.svc.ExtraMX = w

	_, ch, cancel := core.svc.Hub.Subscribe(16)
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

	// First pass: only the Antler receiver is published, so no event (the set is
	// empty) and the shared verdict is empty.
	w.CheckOnce(ctx)
	if hosts, ok := core.svc.ExtraMXState(core.domain.ID); !ok || len(hosts) != 0 {
		t.Fatalf("initial verdict = %v (resolved=%v), want empty resolved", hosts, ok)
	}
	for _, ev := range drain() {
		if ev.Type == model.EventMXExtraMXChanged {
			t.Fatalf("no event expected when the verdict is unchanged/empty: %+v", ev)
		}
	}

	// A rogue record appears: amber, and one transient event names it.
	res.set("antler1.example.test.", "old.example.test.")
	w.CheckOnce(ctx)
	hosts, ok := core.svc.ExtraMXState(core.domain.ID)
	if !ok || len(hosts) != 1 || hosts[0] != "old.example.test" {
		t.Fatalf("verdict after rogue record = %v, want [old.example.test]", hosts)
	}
	events := drain()
	var found bool
	for _, ev := range events {
		if ev.Type != model.EventMXExtraMXChanged {
			continue
		}
		found = true
		if !ev.Transient {
			t.Fatalf("extra-MX event must be transient so the stream emits no id: line")
		}
		if ev.AccountID != core.user.AccountID {
			t.Fatalf("event account = %q, want %q", ev.AccountID, core.user.AccountID)
		}
		got, _ := ev.Payload["unexpected_mx"].([]string)
		if len(got) != 1 || got[0] != "old.example.test" {
			t.Fatalf("event payload unexpected_mx = %v, want [old.example.test]", ev.Payload["unexpected_mx"])
		}
	}
	if !found {
		t.Fatal("no mx.extra_mx_changed event published for a changed verdict")
	}

	// A repeat pass with the same DNS publishes nothing (diffed).
	w.CheckOnce(ctx)
	for _, ev := range drain() {
		if ev.Type == model.EventMXExtraMXChanged {
			t.Fatalf("unchanged verdict must not republish: %+v", ev)
		}
	}
}

// fixedAntlerReceivers is a deterministic Antler endpoint resolver for the watcher.
type fixedAntlerReceivers []mxdial.AntlerReceiver

func (f fixedAntlerReceivers) Receivers(context.Context) ([]mxdial.AntlerReceiver, error) {
	return f, nil
}
