package mxagent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// blockingDelivery is a fake Delivery whose first Ingest blocks until released,
// so the test can observe that the staging reservation is held for the whole
// transaction (verify + ingest), not merely while the copy goroutine is
// reading. Later ingests return an immediate durable success.
type blockingDelivery struct {
	blockFirst bool
	firstGate  chan struct{}
	firstOnce  sync.Once

	resolves   int64
	ingests    int64
	closeCount int64
}

func (d *blockingDelivery) Resolve(_ context.Context, addresses []string) (mxwire.ResolveResponse, error) {
	atomic.AddInt64(&d.resolves, 1)
	resp := mxwire.ResolveResponse{Version: mxwire.V2Protocol}
	for _, a := range addresses {
		resp.Results = append(resp.Results, mxwire.ResolveRecipient{Recipient: a, Accept: true, Domain: "example.test"})
	}
	return resp, nil
}

func (d *blockingDelivery) Ingest(ctx context.Context, meta mxwire.IngestMetadata, body io.Reader, _ int64, _ string) (mxwire.IngestResponse, error) {
	n := atomic.AddInt64(&d.ingests, 1)
	if d.blockFirst && n == 1 {
		select {
		case <-d.firstGate:
		case <-ctx.Done():
			return mxwire.IngestResponse{}, ctx.Err()
		}
	}
	resp := mxwire.IngestResponse{Version: mxwire.V2Protocol}
	for _, r := range meta.Recipients {
		resp.PerRecipient = append(resp.PerRecipient, mxwire.RecipientIngestResult{
			Recipient:   r,
			Disposition: mxwire.DispositionStored,
			MachineCode: mxwire.CodeOK,
		})
	}
	return resp, nil
}

func (d *blockingDelivery) Close() error {
	atomic.AddInt64(&d.closeCount, 1)
	return nil
}

func (d *blockingDelivery) releaseFirst() {
	if d.firstGate != nil {
		d.firstOnce.Do(func() { close(d.firstGate) })
	}
}

func startStagingEdge(t *testing.T, cfg mxagent.Config, d mxagent.Delivery) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := mxagent.NewServerWithDelivery(cfg, nil, func() mxagent.Delivery { return d })
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	return ln.Addr().String(), func() { cancel(); <-done }
}

// sendOnce performs one full SMTP transaction, returning the DATA-close error
// (the SMTP response to the message body).
func sendOnce(t *testing.T, addr, body string) error {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatalf("mail: %v", err)
	}
	if err := c.Rcpt("box@example.test", nil); err != nil {
		t.Fatalf("rcpt: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func smtpCode(err error) int {
	var se *smtp.SMTPError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// TestStagingReservationHeldThroughIngest is the regression for the lifetime
// bug: the reservation must stay held while the raw bytes are resident through
// verification and ingest, not be released when the copy goroutine finishes.
// With a single staging slot equal to maxBytes+1, the first transaction is made
// to block inside Ingest; a concurrent second DATA must be refused 451 because
// the first transaction still holds the whole budget. Releasing the first makes
// a third DATA succeed.
func TestStagingReservationHeldThroughIngest(t *testing.T) {
	const maxBytes = 4096
	d := &blockingDelivery{blockFirst: true, firstGate: make(chan struct{})}
	addr, stop := startStagingEdge(t, mxagent.Config{
		Hostname: "mx.example.test", MaxMessageBytes: maxBytes,
		// Exactly one slot: a single staged message consumes the whole budget.
		MaxStagingBytes: maxBytes + 1,
		MaxRecipients:   10, MaxConnections: 10,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DataTimeout: 2 * time.Second, DNSTimeout: 2 * time.Second,
	}, d)
	defer stop()

	body := "From: Sender <sender@outside.test>\r\nTo: box@example.test\r\nSubject: s\r\n\r\nhello"

	// First transaction: ingest blocks, holding the reservation.
	firstErr := make(chan error, 1)
	go func() { firstErr <- sendOnce(t, addr, body) }()

	// Wait until the first ingest is actually inside Ingest.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt64(&d.ingests) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first ingest never reached the delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second transaction must be refused 451 while the first holds the budget.
	if err := sendOnce(t, addr, body); smtpCode(err) != 451 {
		t.Fatalf("second DATA: want 451 busy, got %v (code %d)", err, smtpCode(err))
	}

	// Release the first; it must complete with 250.
	d.releaseFirst()
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatalf("first DATA: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("first transaction did not complete after release")
	}

	// A third transaction now succeeds: the reservation was released only after
	// ingest finished.
	if err := sendOnce(t, addr, body); err != nil {
		t.Fatalf("third DATA after release: %v", err)
	}
	if got := atomic.LoadInt64(&d.ingests); got != 2 {
		t.Fatalf("ingests=%d want 2 (the refused second DATA never reaches the delivery)", got)
	}
}

// TestStageMessageExactPreallocationAndBounds verifies the staged read reads
// exactly one byte past the cap, rejects oversize, accepts a short message up
// to the cap, rejects an empty message, and returns the exact bytes/digest.
func TestStageMessageExactPreallocationAndBounds(t *testing.T) {
	raw := strings.Repeat("A", 1000)

	got, size, digest, err := mxagent.StageMessage(strings.NewReader(raw), 2000, 5*time.Second)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if string(got) != raw || size != int64(len(raw)) {
		t.Fatalf("staged %d bytes (size %d), want %d exact", len(got), size, len(raw))
	}
	if digest != mxwire.BodyDigest([]byte(raw)) {
		t.Fatalf("digest mismatch")
	}

	// Exactly the cap is accepted.
	if _, size, _, err := mxagent.StageMessage(strings.NewReader(strings.Repeat("A", 2000)), 2000, 5*time.Second); err != nil || size != 2000 {
		t.Fatalf("cap-sized message: size=%d err=%v", size, err)
	}
	// One over the cap is rejected as too large.
	if _, _, _, err := mxagent.StageMessage(strings.NewReader(strings.Repeat("A", 2001)), 2000, 5*time.Second); !errors.Is(err, mxagent.ErrTooLarge) {
		t.Fatalf("oversize: want ErrTooLarge, got %v", err)
	}
	// Empty is an error, not a zero-length success.
	if _, _, _, err := mxagent.StageMessage(bytes.NewReader(nil), 2000, 5*time.Second); err == nil {
		t.Fatal("empty: want error")
	}
}

// slowReader yields one byte per interval, letting a read be interrupted by the
// timeout while still producing more data if left alone.
type slowReader struct {
	data []byte
	i    int
	d    time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, io.EOF
	}
	time.Sleep(r.d)
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

// TestStageMessageTimeoutAlwaysErrors verifies a read timeout always returns an
// error even though the reader eventually delivers all bytes, and that the
// onDone callback fires (so the caller's reservation is released) without the
// caller blocking on the slow reader.
func TestStageMessageTimeoutAlwaysErrors(t *testing.T) {
	data := []byte(strings.Repeat("A", 32))
	done := make(chan struct{})
	var cancel context.CancelFunc

	_, _, _, err := mxagent.StageMessageCtx(&slowReader{data: data, d: 20 * time.Millisecond}, 1024, 30*time.Millisecond, func() {
		close(done)
	}, &cancel)
	if err == nil {
		t.Fatal("timeout must return an error even though the reader completes late")
	}
	if cancel != nil {
		cancel()
	}
	// onDone (copyDone close) must fire promptly after cancellation so the
	// caller can release the staged reservation.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onDone never fired after timeout")
	}
}
