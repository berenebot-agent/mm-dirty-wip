package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestDialMXAntlerUnreachableReceiverKeepsConnectorHostname proves the receiving
// status keeps the configured connector hostname when a receiver the core cannot
// reach advertises none. The status wizard matches each connector row to a
// status row by smtp_hostname, so an empty hostname would strand the row on an
// amber "Waiting" instead of showing the real red failure.
func TestDialMXAntlerUnreachableReceiverKeepsConnectorHostname(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler-unreachable.example.test", SMTPHostname: "antler-unreachable.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}

	// A real manager dials the configured (unresolvable) Antler receiver, which
	// reports the domain as unreachable with no advertised SMTP hostname.
	m := mxdial.New(svc.DialMXBackend(), mxdial.Config{
		DataDir:                  svc.Config.DataDir,
		AllowPrivateDestinations: true,
		ReconcileInterval:        20 * time.Millisecond,
	})
	svc.DialMX = m
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go m.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rows := m.Status(domain.Name)
			if len(rows) > 0 && rows[0].State == mxdial.StatusUnreachable {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows := m.Status(domain.Name); len(rows) > 0 && rows[0].State == mxdial.StatusUnreachable {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows := m.Status(domain.Name); len(rows) == 0 || rows[0].State != mxdial.StatusUnreachable {
		t.Fatalf("receiver not observed unreachable: %#v", m.Status(domain.Name))
	}

	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	statuses, _ := body["status"].([]any)
	if len(statuses) != 1 {
		t.Fatalf("want one status row: %v", body["status"])
	}
	row := statuses[0].(map[string]any)
	if row["state"] != mxdial.StatusUnreachable {
		t.Fatalf("state = %v, want unreachable", row["state"])
	}
	if row["smtp_hostname"] != "antler-unreachable.example.test" {
		t.Fatalf("unreachable receiver lost its configured hostname: %v", row)
	}
}
