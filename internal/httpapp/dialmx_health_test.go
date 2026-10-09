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
		mxChecked bool
		mxMatched []string
		wantLight string
	}{
		{"no receivers", nil, false, nil, "danger"},
		{"all connecting", []mxdialStatusView{{State: "connecting"}, {State: "connecting"}}, false, nil, "danger"},
		{"one ready", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "connecting"}}, false, nil, "ok"},
		{"ready but expired", []mxdialStatusView{{State: "ready", ExpiresAt: &past}}, false, nil, "danger"},
		{"ready without expiry", []mxdialStatusView{{State: "ready"}}, false, nil, "ok"},
		{"connecting and failed", []mxdialStatusView{{State: "connecting"}, {State: "unreachable"}}, false, nil, "danger"},
		{"all failed", []mxdialStatusView{{State: "unreachable"}, {State: "rejected"}}, false, nil, "danger"},
		{"reconnecting and rejected", []mxdialStatusView{{State: "disconnected"}, {State: "rejected"}}, false, nil, "danger"},
		{"ready and mx matched", []mxdialStatusView{{State: "ready", SMTPHostname: "mx1.test", ExpiresAt: &future}}, true, []string{"mx1.test"}, "ok"},
		{"ready but mx unmatched", []mxdialStatusView{{State: "ready", SMTPHostname: "mx1.test", ExpiresAt: &future}}, true, []string{"mx2.test"}, "danger"},
		{"ready but mx not published", []mxdialStatusView{{State: "ready", SMTPHostname: "mx1.test", ExpiresAt: &future}}, true, nil, "danger"},
		{"one routed one connecting", []mxdialStatusView{{State: "ready", SMTPHostname: "mx1.test", ExpiresAt: &future}, {State: "connecting"}}, true, []string{"mx1.test"}, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light, title := dialMXHealth(tc.statuses, tc.mxChecked, tc.mxMatched, now)
			if light != tc.wantLight {
				t.Fatalf("light = %q, want %q", light, tc.wantLight)
			}
			if title == "" {
				t.Fatal("title must always explain the light")
			}
		})
	}
}
