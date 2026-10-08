package receiver_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
)

// TestCoreLifecycleRecordsAreInfo asserts the operator-facing connect and
// disconnect records are emitted at INFO, so a working session is visible
// without DIALMX_LOG_LEVEL=debug. The DEBUG session/transport records remain,
// but the lifecycle is what an operator monitors.
func TestCoreLifecycleRecordsAreInfo(t *testing.T) {
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: rootTLS(t, srv), ForceAttemptHTTP2: true}}
	s := newRawSession(t, client, srv.URL)
	// The INFO record is written immediately after Ready, so it must appear
	// while the session is still open rather than only on close.
	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx core connected")
		return ok
	})

	opened, ok := findRecord(sink.records(t), "dialmx core connected")
	if !ok {
		t.Fatal("missing dialmx core connected record")
	}
	if opened["level"] != "INFO" {
		t.Fatalf("core connected level=%v want INFO", opened["level"])
	}
	for _, key := range []string{"core_connection_id", "peer", "transport", "protocol", "active_connections"} {
		if opened[key] == nil {
			t.Fatalf("core connected missing %s: %v", key, opened)
		}
	}
	if opened["protocol"] != "HTTP/2.0" {
		t.Fatalf("core connected protocol=%v want HTTP/2.0", opened["protocol"])
	}
	if opened["transport"] != "tls" {
		t.Fatalf("core connected transport=%v want tls for a TLS session", opened["transport"])
	}

	s.close()
	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx core disconnected")
		return ok
	})
	closed, ok := findRecord(sink.records(t), "dialmx core disconnected")
	if !ok {
		t.Fatal("missing dialmx core disconnected record")
	}
	if closed["level"] != "INFO" {
		t.Fatalf("core disconnected level=%v want INFO", closed["level"])
	}
	if _, ok := closed["uptime_ms"]; !ok {
		t.Fatalf("core disconnected missing uptime_ms: %v", closed)
	}
	if _, ok := closed["reason"]; !ok {
		t.Fatalf("core disconnected missing reason: %v", closed)
	}
	// The disconnect must be correlatable to the connection it closed.
	if closed["core_connection_id"] != opened["core_connection_id"] {
		t.Fatalf("disconnect id %v != connect id %v", closed["core_connection_id"], opened["core_connection_id"])
	}
}

// TestSessionRejectionIsInfoWithPeer asserts a refused session is INFO and
// carries the reason, since that line is the diagnostic for a misconfigured
// trusted-proxy allowlist. A cleartext HTTP/1.1 request to the session path is
// refused before any session exists.
func TestSessionRejectionIsInfoWithPeer(t *testing.T) {
	sink := &jsonSink{}
	r, _ := newLoggedServer(t, sink, receiver.Config{})

	req, err := http.NewRequest(http.MethodPost, "http://receiver.test"+"/mx/v2/session", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = "203.0.113.9:44444"
	req.ProtoMajor, req.ProtoMinor = 1, 1
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 426 {
		t.Fatalf("cleartext HTTP/1.1 session status=%d want 426", rec.Code)
	}

	// The handler runs synchronously, so the record is already captured.
	got, ok := findRecord(sink.records(t), "dialmx session rejected")
	if !ok {
		t.Fatalf("missing session rejected record; got %s", messages(sink.records(t)))
	}
	if got["level"] != "INFO" {
		t.Fatalf("session rejected level=%v want INFO", got["level"])
	}
	if got["peer"] != "203.0.113.9" {
		t.Fatalf("session rejected peer=%v want the offending address", got["peer"])
	}
}

// TestSummaryHeartbeat asserts the periodic INFO heartbeat reports totals, and
// that the first tick omits the delta because there is no previous window.
func TestSummaryHeartbeat(t *testing.T) {
	sink := &jsonSink{}
	log := receiver.NewLogger(slog.LevelInfo, sink)
	r := receiver.New(receiver.Config{
		Mode:            "single",
		CoreKey:         "0123456789abcdef0123456789abcdef",
		SMTP:            testSMTP(),
		SummaryInterval: 40 * time.Millisecond,
	}, log)
	r.Start()
	t.Cleanup(r.Stop)

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx summary")
		return ok
	})
	// findRecord returns the most recent match, and the ticker keeps firing, so
	// take the FIRST summary: that is the one with no previous window.
	summaries := allRecords(sink.records(t), "dialmx summary")
	if len(summaries) == 0 {
		t.Fatal("no summary records captured")
	}
	first := summaries[0]
	if first["level"] != "INFO" {
		t.Fatalf("summary level=%v want INFO", first["level"])
	}
	for _, key := range []string{"sessions_total", "proofs_total", "messages_total", "resolves_total", "active_connections", "interval_s"} {
		if first[key] == nil {
			t.Fatalf("summary missing %s: %v", key, first)
		}
	}
	if first["messages_total"] != "0" {
		t.Fatalf("a fresh receiver should report 0 messages, got %v", first["messages_total"])
	}
	// The first tick has no previous window, so it must not claim a rate.
	if first["messages"] != nil {
		t.Fatalf("first summary must omit the delta: %v", first)
	}
	// A later tick on an unchanged receiver may omit the delta, but must still
	// report the totals.
	if len(summaries) > 1 {
		later := summaries[len(summaries)-1]
		if later["messages_total"] == nil {
			t.Fatalf("a later summary must still report totals: %v", later)
		}
	}
}

// TestSummaryDisabledWhenNegative asserts a negative interval suppresses the
// heartbeat, so a test asserting an exact record set is not disturbed.
func TestSummaryDisabledWhenNegative(t *testing.T) {
	var buf strings.Builder
	log := receiver.NewLogger(slog.LevelInfo, &buf)
	r := receiver.New(receiver.Config{Mode: "single", CoreKey: "0123456789abcdef", SummaryInterval: -1}, log)
	r.Start()
	time.Sleep(80 * time.Millisecond)
	r.Stop()
	if strings.Contains(buf.String(), "dialmx summary") {
		t.Fatalf("summary emitted despite a negative interval: %q", buf.String())
	}
}
