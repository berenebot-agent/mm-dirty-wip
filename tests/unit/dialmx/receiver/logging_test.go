package receiver_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// jsonSink is a concurrency-safe io.Writer that captures the receiver's log
// bytes so a test can parse them into records. It is a plain writer because the
// production logger takes an io.Writer and emits compact [MX] text.
type jsonSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *jsonSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// records parses every complete captured line into a map. Each record carries
// "msg" and "level", plus one key per rendered attribute. Values are unquoted;
// list and quoted-space values are not needed by these assertions.
func (s *jsonSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(s.buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, " [MX] ") {
			continue
		}
		out = append(out, parseLogLine(line))
	}
	return out
}

// parseLogLine splits one "<time> [MX] <LEVEL> <msg words> key=value ..." line.
// The message is every word after the level up to the first key=value token;
// message words never contain '=', attributes always do.
func parseLogLine(line string) map[string]any {
	fields := strings.Fields(line)
	rec := map[string]any{}
	if len(fields) < 4 {
		return rec
	}
	rec["level"] = fields[2]
	idx := 3
	for idx < len(fields) && !strings.Contains(fields[idx], "=") {
		idx++
	}
	rec["msg"] = strings.Join(fields[3:idx], " ")
	for _, tok := range fields[idx:] {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		rec[k] = unquoteLogValue(v)
	}
	return rec
}

// unquoteLogValue strips the strconv-style quoting the handler applies to
// ambiguous values.
func unquoteLogValue(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
	}
	return v
}

// findRecord returns the most recent record whose msg equals want.
func findRecord(recs []map[string]any, want string) (map[string]any, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i]["msg"] == want {
			return recs[i], true
		}
	}
	return nil, false
}

// allRecords collects every record with the given msg.
func allRecords(recs []map[string]any, want string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == want {
			out = append(out, r)
		}
	}
	return out
}

// recordsWith returns every record whose msg and key value match.
func recordsWith(recs []map[string]any, msg, key string, val any) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["msg"] == msg && r[key] == val {
			out = append(out, r)
		}
	}
	return out
}

func waitForRecords(t *testing.T, sink *jsonSink, cond func([]map[string]any) bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond(sink.records(t)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition on captured records never satisfied")
}

// testSMTP is the shared SMTP-edge bound set for receiver-backed tests.
func testSMTP() mxagent.Config {
	return mxagent.Config{
		Hostname:        "mx.test",
		MaxMessageBytes: 1 << 20,
		MaxStagingBytes: 2 << 20,
		MaxRecipients:   10,
		MaxConnections:  16,
		DataTimeout:     5 * time.Second,
		DNSTimeout:      2 * time.Second,
	}
}

// newLoggedServer builds a receiver whose records are captured into sink, starts
// its HTTP/2 handler and registers it for the SMTP edge helper. It returns the
// receiver and the httptest server.
func newLoggedServer(t *testing.T, sink *jsonSink, cfg receiver.Config) (*receiver.Receiver, *httptest.Server) {
	t.Helper()
	r, srv, _ := newLoggedServerLogger(t, sink, cfg)
	return r, srv
}

// newLoggedServerLogger is newLoggedServer plus the shared logger, so a test can
// serve the SMTP edge through the same logger. The logger captures structured
// records directly (rather than parsing the production [MX] text), at DEBUG so
// the connect/session transport events are observable.
func newLoggedServerLogger(t *testing.T, sink *jsonSink, cfg receiver.Config) (*receiver.Receiver, *httptest.Server, *slog.Logger) {
	t.Helper()
	cfg.SMTP = testSMTP()
	log := newCaptureLogger(sink)
	cfg.Mode = "shared"
	r := receiver.New(cfg, log)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	registerReceiver(srv.URL, r)
	return r, srv, log
}

// captureHandler renders records in the production compact text shape into the
// sink, but always enables every level so a DEBUG logger's records are captured.
// It keeps the sink populated for assertions that inspect raw bytes.
type captureHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	attrs  []slog.Attr
	groups []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("2006-01-02T15:04:05"))
	b.WriteString(" [MX] ")
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	prefix := strings.Join(h.groups, ".")
	writeAttr := func(a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			return
		}
		key := a.Key
		if prefix != "" {
			key = prefix + "." + a.Key
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(fmt.Sprint(a.Value.Any()))
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

// newCaptureLogger returns a DEBUG-level logger writing into sink through the
// capture handler, so both structured and raw-byte assertions work.
func newCaptureLogger(sink *jsonSink) *slog.Logger {
	return slog.New(&captureHandler{mu: &sync.Mutex{}, w: sink})
}

// TestNewLoggerEmitsMatchedTextFormat asserts the production logger emits the
// compact [MX]-tagged text the core uses: no JSON braces, an "[MX]" tag, and a
// DEBUG level that is filtered at the default INFO level.
func TestNewLoggerEmitsMatchedTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := receiver.NewLogger(slog.LevelInfo, &buf)
	log.Debug("dialmx session opened", "stage", "opened")
	log.Info("dialmx handoff result", "recipient", "alice@example.test", "code", "ok")
	line := buf.String()
	if strings.Contains(line, "{") {
		t.Fatalf("log line is not compact text: %q", line)
	}
	if !strings.Contains(line, " [MX] ") {
		t.Fatalf("log line missing [MX] tag: %q", line)
	}
	if strings.Contains(line, "dialmx session opened") {
		t.Fatalf("DEBUG record emitted at INFO level: %q", line)
	}
	if !strings.Contains(line, "INFO dialmx handoff result recipient=alice@example.test code=ok") {
		t.Fatalf("INFO line not in expected shape: %q", line)
	}
	if strings.Contains(line, "schema_version") || strings.Contains(line, "boot_id") || strings.Contains(line, "service=") {
		t.Fatalf("legacy JSON envelope leaked into text line: %q", line)
	}
}

// TestSessionTransportRecordsAreDebug asserts the connect/session transport
// events are DEBUG (hidden by default) while mail receipt/transfer stay INFO.
func TestSessionTransportRecordsAreDebug(t *testing.T) {
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{})
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: rootTLS(t, srv), ForceAttemptHTTP2: true}}
	s := newRawSession(t, client, srv.URL)
	s.close()

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx session opened")
		return ok
	})
	recs := sink.records(t)
	if opened, ok := findRecord(recs, "dialmx session opened"); !ok {
		t.Fatal("missing session opened event")
	} else {
		if opened["level"] != "DEBUG" {
			t.Fatalf("session opened level=%v want DEBUG", opened["level"])
		}
		if opened["core_connection_id"] == nil {
			t.Fatalf("session opened missing core_connection_id: %v", opened)
		}
		if opened["stage"] != "opened" {
			t.Fatalf("stage=%v want opened", opened["stage"])
		}
	}
	if hello, ok := findRecord(recs, "dialmx session hello"); ok {
		if hello["version"] != mxwire.V2Protocol {
			t.Fatalf("hello version=%v want %s", hello["version"], mxwire.V2Protocol)
		}
		if hello["core_label"] == nil {
			t.Fatalf("hello missing core_label: %v", hello)
		}
	}
	if closed, ok := findRecord(recs, "dialmx session closed"); ok {
		if _, ok := closed["duration_ms"]; !ok {
			t.Fatalf("session closed missing duration_ms: %v", closed)
		}
	}
}

// TestSharedSMTPRecordsUseSameLogger proves the shared mxagent SMTP edge emits
// through the same [MX] logger as the receiver, so a shared edge record and a
// receiver record interleave in one format.
func TestSharedSMTPRecordsUseSameLogger(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	sink := &jsonSink{}
	_, srv, log := newLoggedServerLogger(t, sink, receiver.Config{LookupTXT: dns})

	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: rootTLS(t, srv), ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")
	send(t, startEdgeWithLogger(t, 2, srv.URL, log), []string{"alice@example.test"})

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "mx smtp transaction decision")
		return ok
	})
	for _, rec := range allRecords(sink.records(t), "mx smtp transaction decision") {
		if rec["level"] != "INFO" {
			t.Fatalf("shared edge decision level=%v want INFO: %v", rec["level"], rec)
		}
	}
}

// TestDomainAuthSummaryLogging drives a real mxdial manager against the receiver
// and asserts one domain proof attempt produces a single INFO "dialmx domain
// auth" summary (terminal phase, result and folded steps), that the per-step
// detail is DEBUG, and that no raw TXT record or public key material leaks.
func TestDomainAuthSummaryLogging(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	txt := mxwire.DomainTXT("key1", pub)
	dns := func(context.Context, string) ([]string, error) { return []string{txt}, nil }
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns, RevalidateInterval: 60 * time.Millisecond})

	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: rootTLS(t, srv), ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		for _, rec := range allRecords(recs, "dialmx domain auth") {
			if rec["phase"] == "renewal" && rec["result"] == "renewed" {
				return true
			}
		}
		return false
	})

	auths := allRecords(sink.records(t), "dialmx domain auth")
	seen := map[string]string{}
	for _, rec := range auths {
		phase, _ := rec["phase"].(string)
		result, _ := rec["result"].(string)
		if phase != "" {
			seen[phase] = result
		}
		if rec["domain"] != "example.test" || rec["key_id"] != "key1" {
			t.Fatalf("domain auth record missing identity: %v", rec)
		}
	}
	if seen["registration"] != "active" {
		t.Fatalf("registration result=%q want active", seen["registration"])
	}
	if seen["renewal"] != "renewed" {
		t.Fatalf("renewal result=%q want renewed", seen["renewal"])
	}

	// The summary folds the per-step outcomes into one "steps" string.
	for _, rec := range auths {
		steps, _ := rec["steps"].(string)
		if !strings.Contains(steps, "dns_lookup=ok") || !strings.Contains(steps, "parse_key=ok") || !strings.Contains(steps, "grant=ok") {
			t.Fatalf("domain auth summary missing folded steps: %v", rec)
		}
	}
	// The per-step records still exist, but at DEBUG.
	proofs := allRecords(sink.records(t), "dialmx domain proof")
	if len(proofs) == 0 {
		t.Fatal("expected per-step DEBUG proof records")
	}
	for _, rec := range proofs {
		if rec["level"] != slog.LevelDebug.String() {
			t.Fatalf("proof step level=%v want DEBUG: %v", rec["level"], rec)
		}
	}

	// No raw TXT proof or public key material may appear in any captured line.
	for _, rec := range sink.records(t) {
		blob, _ := json.Marshal(rec)
		if strings.Contains(string(blob), txt) {
			t.Fatalf("raw TXT proof leaked into log record: %s", blob)
		}
		if strings.Contains(string(blob), "; p=") {
			t.Fatalf("public key material leaked into log record: %s", blob)
		}
	}
}

// TestResolveNoBindingLogging proves an unknown domain is logged as a
// no_binding routing selection with the full recipient.
func TestResolveNoBindingLogging(t *testing.T) {
	sink := &jsonSink{}
	r, _ := newLoggedServer(t, sink, receiver.Config{})

	d := r.NewDelivery()
	defer d.Close()
	resp, err := d.Resolve(context.Background(), []string{"ghost@unregistered.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || !resp.Results[0].Temporary {
		t.Fatalf("expected temporary routing for unknown domain, got %#v", resp.Results)
	}
	rec, ok := findRecord(sink.records(t), "dialmx resolve")
	if !ok {
		t.Fatal("missing resolve event")
	}
	if rec["selected"] != "no_binding" {
		t.Fatalf("selected=%v want no_binding", rec["selected"])
	}
	if rec["domain"] != "unregistered.test" {
		t.Fatalf("domain=%v want unregistered.test", rec["domain"])
	}
	if rec["recipient"] != "ghost@unregistered.test" {
		t.Fatalf("recipient=%v want full address", rec["recipient"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Fatalf("resolve missing duration_ms: %v", rec)
	}
	// No core was selected, so no core identity must be claimed.
	if _, ok := rec["core_connection_id"]; ok {
		t.Fatalf("no_binding resolve must not claim a core: %v", rec)
	}
}

// TestResolveRecipientCoreCorrelation drives a real delivery and asserts the
// routing event names the authenticated core connection, key, channel and wire
// transaction for the accepted recipient.
func TestResolveRecipientCoreCorrelation(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns})

	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: rootTLS(t, srv), ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")
	send(t, startEdge(t, 2, srv.URL, nil), []string{"alice@example.test"})

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		return len(recordsWith(recs, "dialmx resolve", "recipient", "alice@example.test")) > 0
	})
	recs := sink.records(t)
	accepted := recordsWith(recs, "dialmx resolve", "recipient", "alice@example.test")
	if len(accepted) == 0 {
		t.Fatalf("missing resolve for alice: %s", messages(recs))
	}
	rec := accepted[len(accepted)-1]
	if rec["selected"] != "core" || rec["outcome"] != "accepted" {
		t.Fatalf("resolve outcome=%v selected=%v want core/accepted", rec["outcome"], rec["selected"])
	}
	coreID, _ := rec["core_connection_id"].(string)
	if coreID == "" {
		t.Fatalf("accepted resolve missing core_connection_id: %v", rec)
	}
	if rec["key_id"] != "key1" {
		t.Fatalf("resolve key_id=%v want key1", rec["key_id"])
	}
	if _, ok := rec["channel"]; !ok {
		t.Fatalf("resolve missing channel: %v", rec)
	}
	if _, ok := rec["wire_transaction_id"]; !ok {
		t.Fatalf("resolve missing wire_transaction_id: %v", rec)
	}
	// The same core connection id must appear on the handoff for this message.
	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx handoff result")
		return ok
	})
	handoff := recordsWith(sink.records(t), "dialmx handoff", "core_connection_id", coreID)
	if len(handoff) == 0 {
		t.Fatalf("no handoff correlated to resolve core id %q", coreID)
	}
}

// TestHandoffOutcomeLogging runs a real delivery and asserts the terminal
// handoff record carries the actual recipient/domain arrays, the handoff id, the
// core/wire ids, byte count and numeric duration, and that the per-recipient
// outcome names the full recipient. A valid response is "acknowledged", not
// "ok".
func TestHandoffOutcomeLogging(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
	sink := &jsonSink{}
	_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns})

	be := &backend{domains: []mxdial.Domain{{Name: "example.test", KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}}}}
	m := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: rootTLS(t, srv), ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	ready(t, m, "example.test")

	edge := startEdge(t, 2, srv.URL, nil)
	send(t, edge, []string{"alice@example.test"})

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx handoff result")
		return ok
	})
	recs := sink.records(t)

	handoffs := allRecords(recs, "dialmx handoff")
	if len(handoffs) != 2 || handoffs[0]["phase"] != "start" {
		t.Fatalf("expected start and terminal handoff records, got %v", handoffs)
	}
	rec := handoffs[1]
	if rec["outcome"] != "acknowledged" {
		t.Fatalf("handoff outcome=%v want acknowledged", rec["outcome"])
	}
	if rec["phase"] != "result" {
		t.Fatalf("handoff phase=%v want result", rec["phase"])
	}
	if rec["handoff_id"] == nil || rec["core_connection_id"] == nil || rec["wire_transaction_id"] == nil {
		t.Fatalf("handoff missing id fields: %v", rec)
	}
	if rec["recipients"] != "[alice@example.test]" {
		t.Fatalf("handoff recipients=%v want [alice@example.test]", rec["recipients"])
	}
	if rec["domains"] != "[example.test]" {
		t.Fatalf("handoff domains=%v want [example.test]", rec["domains"])
	}
	if _, ok := rec["bytes"]; !ok {
		t.Fatalf("handoff missing bytes: %v", rec)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Fatalf("handoff missing numeric duration_ms: %v", rec)
	}

	res, ok := findRecord(recs, "dialmx handoff result")
	if !ok {
		t.Fatal("missing handoff result event")
	}
	if res["code"] != string(mxwire.CodeOK) {
		t.Fatalf("result code=%v want %s", res["code"], mxwire.CodeOK)
	}
	if res["disposition"] != string(mxwire.DispositionStored) {
		t.Fatalf("result disposition=%v want %s", res["disposition"], mxwire.DispositionStored)
	}
	if res["recipient"] != "alice@example.test" {
		t.Fatalf("result recipient=%v want full address", res["recipient"])
	}
	if res["handoff_id"] != rec["handoff_id"] {
		t.Fatalf("handoff result id %v != handoff id %v", res["handoff_id"], rec["handoff_id"])
	}
	if _, ok := res["message_id"]; !ok {
		t.Fatalf("result missing message_id: %v", res)
	}
}

// TestHandoffPartialAndUnknownOutcomes proves a transient core outcome is logged
// as an acknowledged handoff with a per-recipient transient code, and that a
// post-IngestEnd wait failure is classified "unknown" (never "fail") because the
// core may have committed.
func TestHandoffPartialAndUnknownOutcomes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }

	t.Run("partial transient is acknowledged", func(t *testing.T) {
		sink := &jsonSink{}
		_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns})
		d := newFakeDialer(t, srv.URL, rootTLS(t, srv), priv, "example.test")
		d.transient = map[string]bool{"bob@example.test": true}
		defer d.close()
		d.waitAuth(t)

		delivery := lookupReceiver(t, srv.URL).NewDelivery()
		defer delivery.Close()
		ctx := context.Background()
		if _, e := delivery.Resolve(ctx, []string{"alice@example.test", "bob@example.test"}); e != nil {
			t.Fatal(e)
		}
		resp, e := delivery.Ingest(ctx, mxwire.IngestMetadata{Recipients: []string{"alice@example.test", "bob@example.test"}}, strings.NewReader("body"), 4, digestOf("body"))
		if e != nil {
			t.Fatal(e)
		}
		if len(resp.PerRecipient) != 2 {
			t.Fatalf("expected two per-recipient results, got %#v", resp.PerRecipient)
		}
		recs := sink.records(t)
		handoffs := allRecords(recs, "dialmx handoff")
		if len(handoffs) == 0 {
			t.Fatalf("missing handoff record: %s", messages(recs))
		}
		if got := handoffs[len(handoffs)-1]["outcome"]; got != "acknowledged" {
			t.Fatalf("partial handoff outcome=%v want acknowledged", got)
		}
		codes := map[string]string{}
		for _, rr := range allRecords(recs, "dialmx handoff result") {
			r, _ := rr["recipient"].(string)
			c, _ := rr["code"].(string)
			codes[r] = c
		}
		if codes["alice@example.test"] != string(mxwire.CodeOK) {
			t.Fatalf("alice code=%q want ok", codes["alice@example.test"])
		}
		if codes["bob@example.test"] != string(mxwire.CodeTempFail) {
			t.Fatalf("bob code=%q want temporary_failure", codes["bob@example.test"])
		}
	})

	t.Run("no response after ingest end is unknown", func(t *testing.T) {
		sink := &jsonSink{}
		_, srv := newLoggedServer(t, sink, receiver.Config{LookupTXT: dns, IngestTimeout: 400 * time.Millisecond})
		d := newFakeDialer(t, srv.URL, rootTLS(t, srv), priv, "example.test")
		d.dropIngestResult = true
		defer d.close()
		d.waitAuth(t)

		delivery := lookupReceiver(t, srv.URL).NewDelivery()
		defer delivery.Close()
		ctx := context.Background()
		if _, e := delivery.Resolve(ctx, []string{"alice@example.test"}); e != nil {
			t.Fatal(e)
		}
		if _, e := delivery.Ingest(ctx, mxwire.IngestMetadata{Recipients: []string{"alice@example.test"}}, strings.NewReader("body"), 4, digestOf("body")); e == nil {
			t.Fatal("expected a wait failure when the core never answers")
		}
		recs := sink.records(t)
		handoffs := allRecords(recs, "dialmx handoff")
		if len(handoffs) == 0 {
			t.Fatalf("missing handoff record: %s", messages(recs))
		}
		rec := handoffs[len(handoffs)-1]
		if rec["outcome"] != "unknown" {
			t.Fatalf("post-ingest-end outcome=%v want unknown (core may have committed)", rec["outcome"])
		}
		if rec["phase"] != "await" {
			t.Fatalf("post-ingest-end phase=%v want await", rec["phase"])
		}
	})
}

// TestTransportTrackerLifecycle exercises the transport tracker directly via a
// real Server+ConnState loop: a completed session logs accepted then closed with
// a transport id, peer port and numeric duration.
func TestTransportTrackerLifecycle(t *testing.T) {
	sink := &jsonSink{}
	log := receiver.NewLogger(slog.LevelDebug, sink)
	tracker := receiver.NewTransportTracker(log)

	r := receiver.New(receiver.Config{Mode: "shared", SMTP: testSMTP()}, log)
	certFile, keyFile, pool := selfSignedCA(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:     r.Handler(),
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}},
		ConnContext: tracker.ConnContext,
		ConnState:   tracker.ConnState,
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close(); <-done })

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
		DisableKeepAlives: true,
	}}

	// Open a real session so the receiver can correlate its core connection to
	// the transport connection assigned by ConnContext.
	base := "https://" + ln.Addr().String()
	s := newRawSession(t, client, base)
	s.close()

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx transport closed")
		return ok
	})
	recs := sink.records(t)
	accepted, _ := findRecord(recs, "dialmx transport accepted")
	if accepted["transport_id"] == nil || accepted["peer_port"] == nil {
		t.Fatalf("transport accepted missing id/peer_port: %v", accepted)
	}
	closed, _ := findRecord(recs, "dialmx transport closed")
	if closed["transport_id"] != accepted["transport_id"] {
		t.Fatalf("transport closed id %v != accepted %v", closed["transport_id"], accepted["transport_id"])
	}
	if _, ok := closed["duration_ms"]; !ok {
		t.Fatalf("transport closed missing numeric duration_ms: %v", closed)
	}
	if closed["reason"] != "closed" {
		t.Fatalf("transport closed reason=%v want closed", closed["reason"])
	}

	// The session must carry the same transport_id the tracker assigned.
	opened, ok := findRecord(recs, "dialmx session opened")
	if !ok {
		t.Fatalf("missing session opened record: %s", messages(recs))
	}
	if opened["transport_id"] != accepted["transport_id"] {
		t.Fatalf("session transport_id %v != transport accepted %v", opened["transport_id"], accepted["transport_id"])
	}
}

// TestTransportTLSFailureLogsFailureAndClose proves a handshake that never
// completes is logged as a TLS failure AND as a close with an aborted reason,
// never omitting the close.
func TestTransportTLSFailureLogsFailureAndClose(t *testing.T) {
	sink := &jsonSink{}
	log := receiver.NewLogger(slog.LevelDebug, sink)
	tracker := receiver.NewTransportTracker(log)

	// A server connection whose peer never negotiates TLS: accept on a plain
	// listener, wrap in *tls.Conn, and drive ConnContext/ConnState exactly as
	// http.Server does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			serverCh <- c
		}
	}()
	rawClient, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverRaw := <-serverCh
	_ = rawClient.Close()
	serverTLS := tls.Server(serverRaw, &tls.Config{})
	_ = tracker.ConnContext(context.Background(), serverTLS)
	tracker.ConnState(serverTLS, http.StateNew)
	tracker.ConnState(serverTLS, http.StateClosed)
	_ = serverTLS.Close()

	waitForRecords(t, sink, func(recs []map[string]any) bool {
		_, ok := findRecord(recs, "dialmx transport closed")
		return ok
	})
	recs := sink.records(t)
	fail, ok := findRecord(recs, "dialmx transport tls failure")
	if !ok {
		t.Fatalf("missing tls failure record: %s", messages(recs))
	}
	if fail["reason"] != "tls_handshake_failed" {
		t.Fatalf("tls failure reason=%v want tls_handshake_failed", fail["reason"])
	}
	if fail["level"] != "DEBUG" {
		t.Fatalf("tls failure level=%v want DEBUG", fail["level"])
	}
	closed, ok := findRecord(recs, "dialmx transport closed")
	if !ok {
		t.Fatalf("tls failure omitted the close record: %s", messages(recs))
	}
	if closed["reason"] != "tls_handshake_failed" {
		t.Fatalf("close reason=%v want tls_handshake_failed (aborted)", closed["reason"])
	}
	if closed["transport_id"] != fail["transport_id"] {
		t.Fatalf("close id %v != failure id %v", closed["transport_id"], fail["transport_id"])
	}
}

// digestOf returns the hex SHA-256 of s, matching what the edge supplies.
func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// messages renders the captured record messages for failure diagnostics.
func messages(recs []map[string]any) string {
	var b strings.Builder
	for _, r := range recs {
		m, _ := r["msg"].(string)
		b.WriteString(m)
		b.WriteByte(' ')
	}
	return b.String()
}
