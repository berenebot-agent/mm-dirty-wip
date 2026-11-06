package httpapp

// This test lives in-package because withRequestTiming, timingWriter and
// slowRequestExempt are unexported; limiter_test.go beside it documents the same
// exception. Every HTTP behaviour reachable from outside is covered under
// tests/unit/httpapp.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowEnough is comfortably above slowRequestThreshold so the record is
// deterministic without making the suite wait on a second.
const slowEnough = 450 * time.Millisecond

func timingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestSlowRequestIsLoggedOnTheOrdinaryLog asserts the line appears at the
// default level (no LOG_LEVEL needed), names the path and status, carries the
// duration, and never leaks the query string.
func TestSlowRequestIsLoggedOnTheOrdinaryLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	m := http.NewServeMux()
	m.HandleFunc("GET /ui/inboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowEnough)
		w.WriteHeader(http.StatusTeapot)
	})
	h := withRequestTiming(timingLogger(&buf), m)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/inboxes/inb_1?flash=one-time-secret", nil))

	out := buf.String()
	for _, want := range []string{"level=INFO", "slow request", "method=GET", "route=/ui/inboxes/{id}", "status=418", "duration_ms="} {
		if !strings.Contains(out, want) {
			t.Fatalf("record missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "flash=one-time-secret") {
		t.Fatalf("record leaked the query string: %q", out)
	}
	if rr.Code != http.StatusTeapot {
		t.Fatalf("response status = %d, want 418", rr.Code)
	}
}

func TestFastRequestIsNotLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := withRequestTiming(timingLogger(&buf), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/inboxes/inb_1", nil))
	if out := buf.String(); out != "" {
		t.Fatalf("fast request logged: %q", out)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response status = %d, want 200", rr.Code)
	}
}

// TestSlowStreamIsNotLogged proves an open stream is exempt even when it lasts
// past the threshold; every exemption is pinned by TestSlowRequestExemptPaths.
func TestSlowStreamIsNotLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := withRequestTiming(timingLogger(&buf), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowEnough)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ui/events/stream", nil))
	if out := buf.String(); out != "" {
		t.Fatalf("stream logged: %q", out)
	}
}

// TestSlowRequestExemptPaths pins the list: every long-lived endpoint is exempt
// and ordinary routes (including the Relay enrolment POST that shares its
// prefix) are timed.
func TestSlowRequestExemptPaths(t *testing.T) {
	t.Parallel()
	exempt := []string{"/relay", "/ui/events/stream", "/v1/events/stream", "/v1/events/wait", "/v1/messages/wait"}
	for _, p := range exempt {
		if !slowRequestExempt(p) {
			t.Errorf("%s must be exempt", p)
		}
	}
	timed := []string{"/", "/login", "/ui/inboxes/inb_1", "/v1/messages", "/relay/enroll", "/v1/events"}
	for _, p := range timed {
		if slowRequestExempt(p) {
			t.Errorf("%s must be timed", p)
		}
	}
}

// TestSlowRequestNeverLogsPathCredentials proves a slow request on a route whose
// path is itself a credential (the invitation token) logs the route pattern, not
// the concrete path, for both methods and for a request no route matched.
func TestSlowRequestNeverLogsPathCredentials(t *testing.T) {
	t.Parallel()
	const secret = "inv_s3cret-token"
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowEnough)
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /invite/{token}", slow)
	m.HandleFunc("POST /invite/{token}", slow)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var buf bytes.Buffer
		h := withRequestTiming(timingLogger(&buf), m)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/invite/"+secret+"?x=1", nil))
		out := buf.String()
		if !strings.Contains(out, "route=/invite/{token}") {
			t.Fatalf("%s: route pattern missing: %q", method, out)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("%s: record leaked the invitation token: %q", method, out)
		}
	}
	var buf bytes.Buffer
	h := withRequestTiming(timingLogger(&buf), http.HandlerFunc(slow))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/invite/"+secret, nil))
	if out := buf.String(); strings.Contains(out, secret) || !strings.Contains(out, "route=unmatched") {
		t.Fatalf("unmatched request logged wrongly: %q", out)
	}
}
