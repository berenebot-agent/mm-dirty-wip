package httpapp

// This suite lives in package httpapp rather than under tests/unit because
// dialMXStatusLabel is an unexported rendering helper: the phrase an operator
// reads for a receiver that answered with its own status is worth pinning, and
// the black-box convention cannot reach it. It uses no database or server.

import "testing"

func TestDialMXStatusLabelNamesTheFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status mxdialStatusView
		want   string
	}{
		{"ready", mxdialStatusView{State: "ready"}, "ready"},
		{"refused", mxdialStatusView{State: "rejected", Reason: "session_rejected"}, "receiver refused the session"},
		// A receiver that answered with its own error status is retrying, but the
		// label must name the cause: "reconnecting" on its own hides it.
		{"server error", mxdialStatusView{State: "disconnected", Reason: "receiver_error"}, "receiver reported a server error"},
		{"http/1.1", mxdialStatusView{State: "rejected", Reason: "http2_required"}, "receiver speaks HTTP/1.1, not HTTP/2"},
		{"unreachable", mxdialStatusView{State: "unreachable"}, "receiver unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dialMXStatusLabel(tc.status); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}
