package httpapp_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestDialMXStatusObserverPublishesHealthEvent proves a receiver status change
// on the shared dial manager is published to the in-process hub as a transient
// mx.health_changed notification scoped to the domain's owning account, so the
// browser can refresh a traffic light the instant readiness changes (rather
// than waiting for a poll). The notification carries no durable cursor.
func TestDialMXStatusObserverPublishesHealthEvent(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler-health.example.test", SMTPHostname: "antler-health.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}

	m := mxdial.New(svc.DialMXBackend(), mxdial.Config{
		DataDir:                  svc.Config.DataDir,
		AllowPrivateDestinations: true,
		ReconcileInterval:        20 * time.Millisecond,
		AuthRetryInterval:        50 * time.Millisecond,
	})
	svc.DialMX = m
	// Install after the manager is assigned so the observer sees it.
	svc.InstallDialMXStatusObserver()

	_, ch, cancelSub := svc.Hub.Subscribe(64)
	defer cancelSub()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go m.Run(runCtx)

	deadline := time.After(8 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != model.EventMXHealthChanged {
				continue
			}
			if ev.AccountID != u.AccountID {
				t.Fatalf("health event account = %q, want %q", ev.AccountID, u.AccountID)
			}
			if ev.Payload["domain_id"] != domain.ID {
				t.Fatalf("health event domain = %#v, want %q", ev.Payload["domain_id"], domain.ID)
			}
			if ev.Cursor != "" {
				t.Fatalf("transient health event must not carry a durable cursor, got %q", ev.Cursor)
			}
			if !ev.Transient {
				t.Fatalf("health event must be marked Transient so the stream never emits an id: line")
			}
			return
		case <-deadline:
			t.Fatal("no mx.health_changed event observed for the domain's account")
		}
	}
}
