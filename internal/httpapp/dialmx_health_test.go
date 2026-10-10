package httpapp

// This suite lives in package httpapp rather than under tests/unit because
// dialMXHealth is an unexported rendering helper whose aggregate rule (any
// ready receiver => green) is exactly the traffic-light contract worth pinning.
// The black-box convention cannot reach it. It uses no database or server.

import (
	"testing"
	"time"
)

func TestDialMXHealthAggregatesReceiverStatus(t *testing.T) {
	now := time.Now()
	future := now.Add(5 * time.Minute)
	past := now.Add(-time.Minute)

	for _, tc := range []struct {
		name      string
		statuses  []mxdialStatusView
		wantLight string
	}{
		{"no receivers", nil, "danger"},
		{"all connecting", []mxdialStatusView{{State: "connecting"}, {State: "connecting"}}, "danger"},
		{"one ready", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "connecting"}}, "ok"},
		{"ready but expired", []mxdialStatusView{{State: "ready", ExpiresAt: &past}}, "danger"},
		{"ready without expiry", []mxdialStatusView{{State: "ready"}}, "ok"},
		{"connecting and failed", []mxdialStatusView{{State: "connecting"}, {State: "unreachable"}}, "danger"},
		{"all failed", []mxdialStatusView{{State: "unreachable"}, {State: "rejected"}}, "danger"},
		{"reconnecting and rejected", []mxdialStatusView{{State: "disconnected"}, {State: "rejected"}}, "danger"},
		{"not_mx is not ready", []mxdialStatusView{{State: "rejected", Reason: "not_mx", SMTPHostname: "mx1.test"}}, "danger"},
		{"one ready and one not_mx", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "rejected", Reason: "not_mx"}}, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light, title := dialMXHealth(tc.statuses, now)
			if light != tc.wantLight {
				t.Fatalf("light = %q, want %q", light, tc.wantLight)
			}
			if title == "" {
				t.Fatal("title must always explain the light")
			}
		})
	}
}
