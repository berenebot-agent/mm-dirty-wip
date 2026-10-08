package mxagent_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// captureHandler is a slog.Handler that records every record's message and
// attributes (including those supplied by WithAttrs) so tests can assert the
// structured lifecycle events without parsing text.
type captureHandler struct {
	mu      *sync.Mutex
	records *[]capturedRecord
	attrs   []slog.Attr
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func newCapture() (*captureHandler, *[]capturedRecord) {
	mu := &sync.Mutex{}
	recs := &[]capturedRecord{}
	return &captureHandler{mu: mu, records: recs}, recs
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	*h.records = append(*h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// recordFor returns the most recent record with the given message.
func recordFor(recs []capturedRecord, msg string) (capturedRecord, bool) {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].msg == msg {
			return recs[i], true
		}
	}
	return capturedRecord{}, false
}

func hasRecord(recs []capturedRecord, msg string) bool {
	_, ok := recordFor(recs, msg)
	return ok
}

// ctxDelivery is a fake Delivery that records the TransactionAttrs the edge
// attached to each Resolve/Ingest context, so the test can verify correlation
// flows through the public helper contract.
type ctxDelivery struct {
	mu       sync.Mutex
	resolve  []mxagent.TransactionAttrs
	ingest   []mxagent.TransactionAttrs
	ingested int
}

func (d *ctxDelivery) Resolve(ctx context.Context, addresses []string) (mxwire.ResolveResponse, error) {
	if a, ok := mxagent.TransactionAttrsFromContext(ctx); ok {
		d.mu.Lock()
		d.resolve = append(d.resolve, a)
		d.mu.Unlock()
	}
	resp := mxwire.ResolveResponse{Version: mxwire.V2Protocol}
	for _, a := range addresses {
		resp.Results = append(resp.Results, mxwire.ResolveRecipient{Recipient: a, Accept: true, Domain: "example.test"})
	}
	return resp, nil
}

func (d *ctxDelivery) Ingest(ctx context.Context, meta mxwire.IngestMetadata, _ io.Reader, _ int64, _ string) (mxwire.IngestResponse, error) {
	if a, ok := mxagent.TransactionAttrsFromContext(ctx); ok {
		d.mu.Lock()
		d.ingest = append(d.ingest, a)
		d.mu.Unlock()
	}
	d.mu.Lock()
	d.ingested++
	d.mu.Unlock()
	resp := mxwire.IngestResponse{Version: mxwire.V2Protocol}
	for _, r := range meta.Recipients {
		resp.PerRecipient = append(resp.PerRecipient, mxwire.RecipientIngestResult{
			Recipient: r, Disposition: mxwire.DispositionStored, MachineCode: mxwire.CodeOK,
		})
	}
	return resp, nil
}

func (d *ctxDelivery) Close() error { return nil }

func startLoggingEdge(t *testing.T, log *slog.Logger, delivery mxagent.Delivery) (addr string, stop func()) {
	t.Helper()
	return startEdgeWithConfig(t, log, delivery, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
		VerifySPF: false, VerifyDKIM: false, VerifyDMARC: false,
	})
}

// startEdgeWithConfig starts an edge with the given config, filling the network
// address if unset.
func startEdgeWithConfig(t *testing.T, log *slog.Logger, delivery mxagent.Delivery, cfg mxagent.Config) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := mxagent.NewServerWithDelivery(cfg, log, func() mxagent.Delivery { return delivery })
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	return ln.Addr().String(), func() { cancel(); <-done }
}

// TestEdgeLifecycleLogging drives one successful SMTP transaction and asserts
// the structured lifecycle events exist with correlation ids, and that the
// connection open/close events bracket them.
func TestEdgeLifecycleLogging(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	sendMessage(t, addr, "sender@outside.test", []string{"box@example.test"},
		"From: Sender <sender@outside.test>\r\nTo: box@example.test\r\nSubject: lifecycle\r\n\r\nhello")

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx connection closed")
	})

	handler.mu.Lock()
	got := append([]capturedRecord(nil), *recs...)
	handler.mu.Unlock()

	for _, msg := range []string{
		"mx connection opened",
		"mx session started",
		"mx mail transaction started",
		"mx recipient routing",
		"mx data staging",
		"mx auth evidence",
		"mx core ingest result",
		"mx smtp transaction decision",
		"mx smtp reply transport write",
		"mx connection closed",
	} {
		if !hasRecord(got, msg) {
			t.Fatalf("missing lifecycle event %q; records: %s", msg, messages(got))
		}
	}
	// Mail receipt, transfer and the single per-message auth evidence record are
	// INFO; the per-connection and per-session transport chatter is DEBUG, hidden
	// at the default level.
	levels := map[string]slog.Level{
		"mx connection opened":          slog.LevelDebug,
		"mx session started":            slog.LevelDebug,
		"mx mail transaction started":   slog.LevelInfo,
		"mx recipient routing":          slog.LevelInfo,
		"mx data staging":               slog.LevelDebug,
		"mx auth evidence":              slog.LevelInfo,
		"mx core ingest result":         slog.LevelInfo,
		"mx smtp transaction decision":  slog.LevelInfo,
		"mx smtp reply transport write": slog.LevelDebug,
		"mx connection closed":          slog.LevelDebug,
	}
	for msg, want := range levels {
		r, ok := recordFor(got, msg)
		if !ok {
			t.Fatalf("missing lifecycle event %q; records: %s", msg, messages(got))
		}
		if r.level != want {
			t.Fatalf("event %q at %v, want %v", msg, r.level, want)
		}
	}

	open, _ := recordFor(got, "mx connection opened")
	closeRec, _ := recordFor(got, "mx connection closed")
	connID, _ := open.attrs[mxagent.AttrConnectionID].(string)
	if connID == "" {
		t.Fatalf("connection open missing %s: %v", mxagent.AttrConnectionID, open.attrs)
	}
	if closeRec.attrs[mxagent.AttrConnectionID] != connID {
		t.Fatalf("connection close id %v != open id %q", closeRec.attrs[mxagent.AttrConnectionID], connID)
	}

	start, _ := recordFor(got, "mx mail transaction started")
	if start.attrs[mxagent.AttrConnectionID] != connID {
		t.Fatalf("transaction start has wrong connection id: %v", start.attrs)
	}
	txID, _ := start.attrs[mxagent.AttrTransactionID].(string)
	if txID == "" {
		t.Fatalf("transaction start missing %s: %v", mxagent.AttrTransactionID, start.attrs)
	}

	// The decision and the final reply-write event must share the transaction id.
	decision, _ := recordFor(got, "mx smtp transaction decision")
	if decision.attrs[mxagent.AttrTransactionID] != txID {
		t.Fatalf("decision tx id %v != %q", decision.attrs[mxagent.AttrTransactionID], txID)
	}
	if decision.attrs["smtp_code"] != "250" {
		t.Fatalf("decision smtp_code=%v want 250", decision.attrs["smtp_code"])
	}
	write, _ := recordFor(got, "mx smtp reply transport write")
	if write.attrs[mxagent.AttrTransactionID] != txID {
		t.Fatalf("reply transport write tx id %v != %q", write.attrs[mxagent.AttrTransactionID], txID)
	}
	if write.attrs["ok"] != true {
		t.Fatalf("reply transport write ok=%v want true", write.attrs["ok"])
	}

	// Numeric durations accompany the decision, staging and connection events.
	if _, ok := decision.attrs["duration_ms"].(int64); !ok {
		t.Fatalf("decision duration_ms=%v (%T) want int64", decision.attrs["duration_ms"], decision.attrs["duration_ms"])
	}
	if _, ok := closeRec.attrs["duration_ms"].(int64); !ok {
		t.Fatalf("close duration_ms=%v (%T) want int64", closeRec.attrs["duration_ms"], closeRec.attrs["duration_ms"])
	}

	// The connection close event carries endpoint, byte counters and the
	// session count.
	for _, k := range []string{"peer", "local", "bytes_read", "bytes_written", "sessions", "close_reason"} {
		if _, ok := closeRec.attrs[k]; !ok {
			t.Fatalf("connection close missing %q: %v", k, closeRec.attrs)
		}
	}
	if read, _ := closeRec.attrs["bytes_read"].(int64); read <= 0 {
		t.Fatalf("bytes_read=%v want > 0", closeRec.attrs["bytes_read"])
	}
	if wrote, _ := closeRec.attrs["bytes_written"].(int64); wrote <= 0 {
		t.Fatalf("bytes_written=%v want > 0", closeRec.attrs["bytes_written"])
	}
	if sess, _ := closeRec.attrs["sessions"].(int64); sess != 1 {
		t.Fatalf("sessions=%v want 1", closeRec.attrs["sessions"])
	}

	// The receiver saw the same ids in its Resolve and Ingest contexts.
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.resolve) == 0 || len(d.ingest) == 0 {
		t.Fatalf("receiver did not receive context attrs: resolve=%d ingest=%d", len(d.resolve), len(d.ingest))
	}
	rid := d.resolve[len(d.resolve)-1]
	iid := d.ingest[len(d.ingest)-1]
	if rid.ConnectionID != connID || iid.ConnectionID != connID {
		t.Fatalf("receiver connection ids: resolve=%q ingest=%q want %q", rid.ConnectionID, iid.ConnectionID, connID)
	}
	if rid.TransactionID != txID || iid.TransactionID != txID {
		t.Fatalf("receiver transaction ids: resolve=%q ingest=%q want %q", rid.TransactionID, iid.TransactionID, txID)
	}
}

// TestEdgeAuthEvidenceFullArrays verifies the auth evidence event carries the
// whole normalized mxwire.AuthResults value (not just summary counts) alongside
// the enabled toggles, so a reader can tell "verification disabled" from
// "enabled but no evidence".
func TestEdgeAuthEvidenceFullArrays(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	addr, stop := startEdgeWithConfig(t, log, d, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
		VerifySPF: true, VerifyDKIM: false, VerifyDMARC: true,
	})
	defer stop()

	sendMessage(t, addr, "sender@outside.test", []string{"box@example.test"},
		"From: a@outside.test\r\nTo: box@example.test\r\nSubject: auth\r\n\r\nx")

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx auth evidence")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx auth evidence")
	if rec.attrs["spf_enabled"] != true || rec.attrs["dkim_enabled"] != false || rec.attrs["dmarc_enabled"] != true {
		t.Fatalf("enabled toggles=%v/%v/%v want true/false/true",
			rec.attrs["spf_enabled"], rec.attrs["dkim_enabled"], rec.attrs["dmarc_enabled"])
	}
	evidence, ok := rec.attrs["auth_results"].(mxwire.AuthResults)
	if !ok {
		t.Fatalf("auth_results type=%T want mxwire.AuthResults", rec.attrs["auth_results"])
	}
	// The full value is exposed, not a truncated summary: SPF evidence is
	// present as a typed pointer even when it is a "none" verdict.
	if evidence.SPF == nil {
		t.Fatalf("auth_results.SPF is nil; full evidence not logged: %+v", evidence)
	}
	// The verification duration is folded into the single auth evidence record
	// rather than emitted as a separate completion line.
	if _, ok := rec.attrs["duration_ms"].(int64); !ok {
		t.Fatalf("auth evidence duration_ms=%v (%T) want int64", rec.attrs["duration_ms"], rec.attrs["duration_ms"])
	}
}

// TestEdgeTransactionIdsDistinctPerMail verifies each MAIL transaction gets its
// own id on one connection, and the connection id is stable across both.
func TestEdgeTransactionIdsDistinctPerMail(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	// Two transactions on one TCP connection: the connection id must stay
	// stable while each MAIL gets its own transaction id.
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	for _, subj := range []string{"one", "two"} {
		if err := c.Mail("sender@outside.test", nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Rcpt("box@example.test", nil); err != nil {
			t.Fatal(err)
		}
		w, err := c.Data()
		if err != nil {
			t.Fatal(err)
		}
		body := "From: a@outside.test\r\nTo: box@example.test\r\nSubject: " + subj + "\r\n\r\n" + subj
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Quit()
	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		n := 0
		for _, r := range *recs {
			if r.msg == "mx mail transaction started" {
				n++
			}
		}
		return n == 2
	})

	handler.mu.Lock()
	defer handler.mu.Unlock()
	var starts []capturedRecord
	for _, r := range *recs {
		if r.msg == "mx mail transaction started" {
			starts = append(starts, r)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("transaction starts=%d want 2: %s", len(starts), messages(*recs))
	}
	id1, _ := starts[0].attrs[mxagent.AttrTransactionID].(string)
	id2, _ := starts[1].attrs[mxagent.AttrTransactionID].(string)
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("transaction ids must be non-empty and distinct: %q %q", id1, id2)
	}
	if starts[0].attrs[mxagent.AttrConnectionID] != starts[1].attrs[mxagent.AttrConnectionID] {
		t.Fatalf("connection id changed between MAILs: %v %v",
			starts[0].attrs[mxagent.AttrConnectionID], starts[1].attrs[mxagent.AttrConnectionID])
	}
}

// TestEdgeAbandonedTransactionLogging verifies a MAIL/RSET with no DATA emits a
// terminal abandoned event rather than leaving the transaction open.
func TestEdgeAbandonedTransactionLogging(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("box@example.test", nil); err != nil {
		t.Fatal(err)
	}
	// RSET abandons the open transaction.
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx transaction abandoned")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	abandoned, _ := recordFor(*recs, "mx transaction abandoned")
	if abandoned.attrs["reason"] != "reset" {
		t.Fatalf("abandoned reason=%v want reset", abandoned.attrs["reason"])
	}
	if n, _ := abandoned.attrs["recipients"].(int64); n != 1 {
		t.Fatalf("abandoned recipients=%v want 1", abandoned.attrs["recipients"])
	}
	if _, ok := abandoned.attrs["duration_ms"].(int64); !ok {
		t.Fatalf("abandoned duration_ms=%v (%T) want int64", abandoned.attrs["duration_ms"], abandoned.attrs["duration_ms"])
	}
}

// TestEdgeRejectedRecipientLogging verifies the routing outcome for an
// unknown recipient is recorded as a rejected outcome.
func TestEdgeRejectedRecipientLogging(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &unknownRcptDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("nobody@example.test", nil); err == nil {
		t.Fatal("expected rejection")
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx recipient routing")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx recipient routing")
	if rec.attrs["outcome"] != "rejected" {
		t.Fatalf("outcome=%v want rejected", rec.attrs["outcome"])
	}
}

// TestEdgeCoreAckSeparateFromDecision verifies a transient core outcome is
// recorded on the core-ack event, while the SMTP decision is recorded
// separately and the reply write is observed independently.
func TestEdgeCoreAckSeparateFromDecision(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &transientDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	// The DATA reply is a 451, so the transaction helper that fatals on error
	// is not usable here.
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("box@example.test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "From: a@outside.test\r\nTo: box@example.test\r\nSubject: t\r\n\r\nx")
	if err := w.Close(); err == nil {
		t.Fatal("expected transient DATA failure")
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx smtp transaction decision")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	ack, ok := recordFor(*recs, "mx core ingest result")
	if !ok {
		t.Fatalf("missing core ack: %s", messages(*recs))
	}
	if ack.attrs["outcome"] != "acknowledged" || ack.attrs["transient_failed"] != int64(1) {
		t.Fatalf("core ack attrs=%v want outcome=ok transient_failed=1", ack.attrs)
	}
	decision, _ := recordFor(*recs, "mx smtp transaction decision")
	if decision.attrs["smtp_code"] != "451" || decision.attrs["reason"] != "transient" {
		t.Fatalf("decision attrs=%v want 451/transient", decision.attrs)
	}
	if !hasRecord(*recs, "mx smtp reply transport write") {
		t.Fatalf("missing reply transport write: %s", messages(*recs))
	}
}

// transientDelivery returns a per-recipient transient failure, which the core
// would report when it could not durably accept a recipient.
type transientDelivery struct{}

func (d *transientDelivery) Resolve(_ context.Context, addresses []string) (mxwire.ResolveResponse, error) {
	resp := mxwire.ResolveResponse{Version: mxwire.V2Protocol}
	for _, a := range addresses {
		resp.Results = append(resp.Results, mxwire.ResolveRecipient{Recipient: a, Accept: true, Domain: "example.test"})
	}
	return resp, nil
}

func (d *transientDelivery) Ingest(_ context.Context, meta mxwire.IngestMetadata, _ io.Reader, _ int64, _ string) (mxwire.IngestResponse, error) {
	resp := mxwire.IngestResponse{Version: mxwire.V2Protocol}
	for _, r := range meta.Recipients {
		resp.PerRecipient = append(resp.PerRecipient, mxwire.RecipientIngestResult{
			Recipient: r, MachineCode: mxwire.CodeTempFail,
		})
	}
	return resp, nil
}

func (d *transientDelivery) Close() error { return nil }

// errorResolveDelivery fails every routing lookup, exercising the temporary
// routing path.
type errorResolveDelivery struct{}

func (d *errorResolveDelivery) Resolve(context.Context, []string) (mxwire.ResolveResponse, error) {
	return mxwire.ResolveResponse{}, errors.New("routing backend unavailable")
}

func (d *errorResolveDelivery) Ingest(context.Context, mxwire.IngestMetadata, io.Reader, int64, string) (mxwire.IngestResponse, error) {
	return mxwire.IngestResponse{}, nil
}

func (d *errorResolveDelivery) Close() error { return nil }

// unknownRcptDelivery rejects every recipient as unknown.
type unknownRcptDelivery struct{}

func (d *unknownRcptDelivery) Resolve(context.Context, []string) (mxwire.ResolveResponse, error) {
	return mxwire.ResolveResponse{Version: mxwire.V2Protocol, Results: []mxwire.ResolveRecipient{{Recipient: "nobody@example.test"}}}, nil
}

func (d *unknownRcptDelivery) Ingest(context.Context, mxwire.IngestMetadata, io.Reader, int64, string) (mxwire.IngestResponse, error) {
	return mxwire.IngestResponse{}, nil
}

func (d *unknownRcptDelivery) Close() error { return nil }

// sendMessage performs one full SMTP transaction against addr.
func sendMessage(t *testing.T, addr, from string, rcpts []string, body string) {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := c.Mail(from, nil); err != nil {
		t.Fatalf("mail: %v", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r, nil); err != nil {
			t.Fatalf("rcpt %s: %v", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close data: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("quit: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition never satisfied")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEdgeOversizeStagingOutcome verifies an oversize DATA is logged as a
// staging failure with the too_large classifier and a 552 decision, while the
// SMTP error remains a permanent 552.
func TestEdgeOversizeStagingOutcome(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mxagent.NewServerWithDelivery(mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 2048, MaxStagingBytes: 1 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
	}, log, func() mxagent.Delivery { return d })
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	defer func() { cancel(); <-done }()

	c, err := smtp.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("box@example.test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	// Many short lines: exceeds the byte cap without tripping the line-length
	// limit, so the oversize path (not line-too-long) is exercised.
	body := strings.Repeat("AAAA\r\n", 1024)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("expected oversize DATA to fail")
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx smtp transaction decision")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	failed, _ := recordFor(*recs, "mx data staging")
	if failed.attrs["stage"] != "failed" || failed.attrs["outcome"] != "too_large" {
		t.Fatalf("staging outcome=%v stage=%v attrs=%v want failed/too_large", failed.attrs["outcome"], failed.attrs["stage"], failed.attrs)
	}
	decision, _ := recordFor(*recs, "mx smtp transaction decision")
	if decision.attrs["smtp_code"] != "552" || decision.attrs["reason"] != "too_large" {
		t.Fatalf("decision code=%v reason=%v want 552/too_large", decision.attrs["smtp_code"], decision.attrs["reason"])
	}
}

// TestEdgeStagingBusyOutcome verifies the 451 busy path is logged as a staging
// decision when the aggregate staging budget is full.
func TestEdgeStagingBusyOutcome(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	// One staging slot equal to the message cap: the first transaction holds
	// the whole budget while blocked in Ingest, so the second DATA is refused.
	d := &blockingDelivery{blockFirst: true, firstGate: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const maxBytes = 4096
	srv := mxagent.NewServerWithDelivery(mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: maxBytes, MaxStagingBytes: maxBytes + 1,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 3 * time.Second, DNSTimeout: 2 * time.Second,
	}, log, func() mxagent.Delivery { return d })
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	defer func() { cancel(); <-done }()

	body := "From: a@outside.test\r\nTo: box@example.test\r\nSubject: busy\r\n\r\nhello"
	firstErr := make(chan error, 1)
	go func() { firstErr <- sendOnce(t, ln.Addr().String(), body) }()
	waitFor(t, func() bool { return atomic.LoadInt64(&d.ingests) > 0 })

	if err := sendOnce(t, ln.Addr().String(), body); smtpCode(err) != 451 {
		t.Fatalf("second DATA want 451 busy, got %v", err)
	}
	d.releaseFirst()
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatalf("first DATA: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("first transaction never completed")
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	found := false
	for _, r := range *recs {
		if r.msg == "mx smtp transaction decision" && r.attrs["reason"] == "staging_busy" && r.attrs["smtp_code"] == "451" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing staging_busy decision: %s", messages(*recs))
	}
	// The refused transaction must still have its own transaction id, distinct
	// from the accepted one.
	ids := map[string]bool{}
	for _, r := range *recs {
		if r.msg == "mx smtp transaction decision" {
			if id, _ := r.attrs[mxagent.AttrTransactionID].(string); id != "" {
				ids[id] = true
			}
		}
	}
	if len(ids) != 2 {
		t.Fatalf("decision transaction ids=%d want 2 distinct: %v", len(ids), ids)
	}
}

// TestEdgeConnectionIDRetainedAcrossStartTLS verifies the smtp_connection_id
// observed after STARTTLS is the same one assigned to the TCP connection before
// the upgrade, exercising the wrapper-unwrap path through *tls.Conn.
func TestEdgeConnectionIDRetainedAcrossStartTLS(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	certFile, keyFile := writeSelfSignedCert(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mxagent.NewServerWithDelivery(mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
		TLSCertFile: certFile, TLSKeyFile: keyFile,
	}, log, func() mxagent.Delivery { return d })
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	defer func() { cancel(); <-done }()

	c, err := smtp.DialStartTLS(ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("starttls: %v", err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("box@example.test", nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "From: a@outside.test\r\nTo: box@example.test\r\nSubject: tls\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx mail transaction started")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()

	// The pre-upgrade open event carries the connection id; the post-STARTTLS
	// session must reuse it. There must be exactly one open event.
	var opens []capturedRecord
	for _, r := range *recs {
		if r.msg == "mx connection opened" {
			opens = append(opens, r)
		}
	}
	if len(opens) != 1 {
		t.Fatalf("connection opened events=%d want 1: %s", len(opens), messages(*recs))
	}
	openID, _ := opens[0].attrs[mxagent.AttrConnectionID].(string)
	if openID == "" {
		t.Fatal("open event missing connection id")
	}
	start, _ := recordFor(*recs, "mx mail transaction started")
	if start.attrs[mxagent.AttrConnectionID] != openID {
		t.Fatalf("post-STARTTLS connection id %v != pre-upgrade %q", start.attrs[mxagent.AttrConnectionID], openID)
	}

	// A successful STARTTLS emits a distinct event with the negotiated TLS
	// version and cipher, on the same connection id.
	established, ok := recordFor(*recs, "mx starttls established")
	if !ok {
		t.Fatalf("missing starttls established event: %s", messages(*recs))
	}
	if established.attrs[mxagent.AttrConnectionID] != openID {
		t.Fatalf("starttls connection id %v != %q", established.attrs[mxagent.AttrConnectionID], openID)
	}
	if v, _ := established.attrs["tls_version"].(string); v == "" {
		t.Fatalf("starttls missing tls_version: %v", established.attrs)
	}
	if cph, _ := established.attrs["tls_cipher"].(string); cph == "" {
		t.Fatalf("starttls missing tls_cipher: %v", established.attrs)
	}
	// The post-STARTTLS session event reports tls=true.
	var tlsSession bool
	for _, r := range *recs {
		if r.msg == "mx session started" && r.attrs["tls"] == true {
			tlsSession = true
		}
	}
	if !tlsSession {
		t.Fatalf("no encrypted mx session started event: %s", messages(*recs))
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ingest) == 0 {
		t.Fatal("receiver did not receive the post-STARTTLS ingest")
	}
	if got := d.ingest[len(d.ingest)-1].ConnectionID; got != openID {
		t.Fatalf("receiver ingest connection id %q != %q", got, openID)
	}
}

// TestEdgeGlobalCapRejectionLogged verifies the global connection-cap rejection
// is logged distinctly from the per-source rejection.
func TestEdgeGlobalCapRejectionLogged(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	// A cap of one, and the first connection held open, forces the second
	// accept to be rejected by the global cap.
	addr, stop := startEdgeWithConfig(t, log, d, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 1,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
	})
	defer stop()

	first, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Hello("client.test"); err != nil {
		t.Fatal(err)
	}

	second, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// The second greeting must be refused with 421 from the global cap.
	if err := second.Hello("client.test"); err == nil {
		t.Fatal("expected global cap rejection on second connection")
	}

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx connection rejected: too many connections")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx connection rejected: too many connections")
	if rec.attrs["reason"] != "global_cap" {
		t.Fatalf("global cap reason=%v want global_cap", rec.attrs["reason"])
	}
	if rec.attrs["limit"] != int64(1) {
		t.Fatalf("global cap limit=%v want 1", rec.attrs["limit"])
	}
	if hasRecord(*recs, "mx connection rejected: too many from source") {
		t.Fatal("global cap must not be logged as a source rejection")
	}
}

// TestEdgeMailRejectedByTLSStillCorrelated verifies a MAIL rejected for missing
// TLS still carries an assigned transaction id, and is not later reported as an
// abandoned transaction (behaviour is unchanged: still a 530).
func TestEdgeMailRejectedByTLSStillCorrelated(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	// RequireTLS needs a certificate so STARTTLS can be offered; the client
	// deliberately connects in plaintext and is rejected.
	certFile, keyFile := writeSelfSignedCert(t)
	addr, stop := startEdgeWithConfig(t, log, d, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
		TLSCertFile: certFile, TLSKeyFile: keyFile, RequireTLS: true,
	})
	defer stop()

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err == nil {
		t.Fatal("expected MAIL rejection without TLS")
	}
	_ = c.Quit()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx mail transaction rejected")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx mail transaction rejected")
	if rec.attrs["reason"] != "tls_required" {
		t.Fatalf("reason=%v want tls_required", rec.attrs["reason"])
	}
	txID, _ := rec.attrs[mxagent.AttrTransactionID].(string)
	if txID == "" {
		t.Fatalf("rejected MAIL missing transaction id: %v", rec.attrs)
	}
	if hasRecord(*recs, "mx transaction abandoned") {
		t.Fatal("a rejected MAIL must be terminal, not abandoned")
	}
}

// TestEdgeRcptTemporaryRoutingLogged verifies a Resolve failure is recorded as
// a temporary routing outcome with a bounded reason. It drives the raw SMTP
// protocol, because the routing decision is a server-side path.
//
// Malformed RCPT syntax and a RCPT arriving before MAIL never reach the session:
// go-smtp answers them itself (501 and 502), so the defensive empty/need_mail
// branches are not observable end to end.
func TestEdgeRcptTemporaryRoutingLogged(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &errorResolveDelivery{}
	addr, stop := startLoggingEdge(t, log, d)
	defer stop()

	conn, br := rawSMTP(t, addr)
	defer conn.Close()
	expectCode(t, br, 220)
	writeSMTP(t, conn, "EHLO client.test\r\n")
	drainMulti(t, br, 250)
	writeSMTP(t, conn, "MAIL FROM:<sender@outside.test>\r\n")
	expectCode(t, br, 250)
	writeSMTP(t, conn, "RCPT TO:<box@example.test>\r\n")
	if code := expectCode(t, br, 0); code != 451 {
		t.Fatalf("RCPT on resolve error got %d want 451", code)
	}
	writeSMTP(t, conn, "QUIT\r\n")
	expectCode(t, br, 221)

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx recipient routing")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx recipient routing")
	if rec.attrs["outcome"] != "temporary" || rec.attrs["reason"] != "resolve_error" {
		t.Fatalf("routing attrs=%v want temporary/resolve_error", rec.attrs)
	}
}

// TestEdgeConnectionReadTimeoutClassified verifies an idle connection that hits
// the read timeout records a classified timeout on the close event, with
// numeric byte counters.
func TestEdgeConnectionReadTimeoutClassified(t *testing.T) {
	handler, recs := newCapture()
	log := slog.New(handler)
	d := &ctxDelivery{}
	addr, stop := startEdgeWithConfig(t, log, d, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 4 << 20,
		MaxRecipients: 10, MaxConnections: 10,
		ReadTimeout: 200 * time.Millisecond, WriteTimeout: 2 * time.Second,
		DataTimeout: 2 * time.Second, DNSTimeout: 2 * time.Second,
	})
	defer stop()

	conn, br := rawSMTP(t, addr)
	defer conn.Close()
	expectCode(t, br, 220)
	writeSMTP(t, conn, "EHLO client.test\r\n")
	drainMulti(t, br, 250)
	// Stall without sending another command; the server read timeout fires and
	// closes the connection.
	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return hasRecord(*recs, "mx connection closed")
	})
	handler.mu.Lock()
	defer handler.mu.Unlock()
	rec, _ := recordFor(*recs, "mx connection closed")
	if rec.attrs["close_reason"] != "timeout" {
		t.Fatalf("close_reason=%v want timeout", rec.attrs["close_reason"])
	}
	if rec.attrs["last_read_error"] != "timeout" {
		t.Fatalf("last_read_error=%v want timeout", rec.attrs["last_read_error"])
	}
	for _, k := range []string{"bytes_read", "bytes_written", "duration_ms", "sessions"} {
		if _, ok := rec.attrs[k].(int64); !ok {
			t.Fatalf("%s=%v (%T) want int64", k, rec.attrs[k], rec.attrs[k])
		}
	}
}

// rawSMTP dials addr and returns the connection plus a line reader.
func rawSMTP(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, bufio.NewReader(conn)
}

// writeSMTP sends a raw protocol line.
func writeSMTP(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}

// expectCode reads one reply line and returns its numeric code. When want is
// non-zero it asserts the code matches.
func expectCode(t *testing.T, br *bufio.Reader, want int) int {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if len(line) < 3 {
		t.Fatalf("short reply %q", line)
	}
	code := int(line[0]-'0')*100 + int(line[1]-'0')*10 + int(line[2]-'0')
	if want != 0 && code != want {
		t.Fatalf("reply %q got %d want %d", strings.TrimSpace(line), code, want)
	}
	return code
}

// drainMulti reads a multi-line reply until the final "code " line.
func drainMulti(t *testing.T, br *bufio.Reader, want int) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		if len(line) >= 4 && line[3] == ' ' {
			code := int(line[0]-'0')*100 + int(line[1]-'0')*10 + int(line[2]-'0')
			if code != want {
				t.Fatalf("reply %q got %d want %d", strings.TrimSpace(line), code, want)
			}
			return
		}
	}
}

// writeSelfSignedCert writes an ephemeral self-signed ECDSA certificate and key
// to temp files and returns their paths. The edge only needs a parseable pair.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mx.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"mx.example.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func messages(recs []capturedRecord) string {
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(r.msg)
		b.WriteByte(' ')
	}
	return b.String()
}
