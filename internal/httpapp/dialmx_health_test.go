package httpapp

// This suite lives in package httpapp rather than under tests/unit because
// dialMXHealth is an unexported rendering helper whose aggregate rule (any
// ready receiver and no extra MX => green, ready with extra MX => amber,
// otherwise red) is exactly the traffic-light contract worth pinning. The
// black-box convention cannot reach it. It uses no database or server.

import (
	"strings"
	"testing"
	"time"
)

func TestDialMXHealthAggregatesReceiverStatus(t *testing.T) {
	now := time.Now()
	future := now.Add(5 * time.Minute)
	past := now.Add(-time.Minute)

	for _, tc := range []struct {
		name         string
		statuses     []mxdialStatusView
		unexpectedMX []string
		wantLight    string
		wantTitle    string
	}{
		{"no receivers", nil, nil, "danger", ""},
		{"all connecting", []mxdialStatusView{{State: "connecting"}, {State: "connecting"}}, nil, "danger", ""},
		{"one ready", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "connecting"}}, nil, "ok", ""},
		{"ready but expired", []mxdialStatusView{{State: "ready", ExpiresAt: &past}}, nil, "danger", ""},
		{"ready without expiry", []mxdialStatusView{{State: "ready"}}, nil, "ok", ""},
		{"connecting and failed", []mxdialStatusView{{State: "connecting"}, {State: "unreachable"}}, nil, "danger", ""},
		{"all failed", []mxdialStatusView{{State: "unreachable"}, {State: "rejected"}}, nil, "danger", ""},
		{"reconnecting and rejected", []mxdialStatusView{{State: "disconnected"}, {State: "rejected"}}, nil, "danger", ""},
		{"not_mx is not ready", []mxdialStatusView{{State: "rejected", Reason: "not_mx", SMTPHostname: "mx1.test"}}, nil, "danger", ""},
		{"one ready and one not_mx", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "rejected", Reason: "not_mx"}}, nil, "ok", ""},
		// Replaced is the one failure a ready sibling must not hide: another
		// connection owns this domain's receiver binding, so this instance is
		// not receiving on it and cannot reclaim it without a restart.
		{"replaced overrides a ready sibling", []mxdialStatusView{{State: "ready", ExpiresAt: &future}, {State: "unavailable", Reason: "replaced"}}, nil, "danger", "another connection"},
		{"ready with extra MX", []mxdialStatusView{{State: "ready", ExpiresAt: &future}}, []string{"old.example.test"}, "amber", ""},
		{"no ready with extra MX stays red", []mxdialStatusView{{State: "connecting"}}, []string{"old.example.test"}, "danger", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light, title := dialMXHealth(tc.statuses, tc.unexpectedMX, now)
			if light != tc.wantLight {
				t.Fatalf("light = %q, want %q", light, tc.wantLight)
			}
			if title == "" {
				t.Fatal("title must always explain the light")
			}
			if tc.wantTitle != "" && !strings.Contains(strings.ToLower(title), tc.wantTitle) {
				t.Fatalf("title must name the cause, got %q", title)
			}
			if tc.wantLight == "amber" && !strings.Contains(title, tc.unexpectedMX[0]) {
				t.Fatalf("amber title must name the extra records, got %q", title)
			}
		})
	}
}
