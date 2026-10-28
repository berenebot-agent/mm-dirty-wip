package httpapp

// Unexported access tests limiter capacity and semaphore release directly;
// transport-facing tests also live in tests/unit/httpapp.
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

type inboundBlockingBody struct {
	started chan struct{}
	release chan struct{}
}

func (b *inboundBlockingBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.release
	return 0, io.EOF
}

func TestInboundAdmissionRejectsWithoutQueueAndReleases(t *testing.T) {
	// Serial: app.New mutates the process-global outbound network policy.
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, InboundConcurrency: 2}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	s := New(svc, nil)
	h := s.Handler()
	// Registered first so this runs after both parked handlers finish.
	t.Cleanup(func() {
		if len(s.inboundSem) != 0 {
			t.Error("shared inbound slots leaked")
		}
		s.inboundPerIP.mu.Lock()
		defer s.inboundPerIP.mu.Unlock()
		if len(s.inboundPerIP.live) != 0 {
			t.Error("per-IP inbound slots leaked")
		}
	})
	start := func(ip string) (*inboundBlockingBody, chan int) {
		body := &inboundBlockingBody{started: make(chan struct{}), release: make(chan struct{})}
		result := make(chan int, 1)
		go func() {
			r := httptest.NewRequest("POST", "/internal/ingest/resend", body)
			r.RemoteAddr = ip
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			result <- w.Code
		}()
		t.Cleanup(func() { close(body.release); <-result })
		select {
		case <-body.started:
		case <-time.After(3 * time.Second):
			t.Fatal("inbound body never admitted")
		}
		return body, result
	}
	start("192.0.2.1:1234")
	request := func(ip string, cancelled bool) int {
		body := &inboundBlockingBody{started: make(chan struct{}), release: make(chan struct{})}
		// An overflow request must not read the body at all.
		r := httptest.NewRequest("POST", "/internal/ingest/resend", body)
		r.RemoteAddr = ip
		if cancelled {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		returned := make(chan struct{})
		go func() { h.ServeHTTP(w, r); close(returned) }()
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			close(body.release)
			t.Fatal("overflow request queued or read body")
		}
		select {
		case <-body.started:
			t.Fatal("rejected request read body")
		default:
		}
		if !cancelled && w.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry hint")
		}
		return w.Code
	}
	if request("192.0.2.1:5678", false) != http.StatusTooManyRequests {
		t.Fatal("one source monopolized two-slot pool")
	}
	start("192.0.2.2:1234")
	if request("192.0.2.3:1234", false) != http.StatusTooManyRequests {
		t.Fatal("full shared pool did not reject")
	}
	if request("192.0.2.3:1234", true) != 499 {
		t.Fatal("cancelled request not released")
	}
	// Verify that rejected/cancelled requests released their per-IP admission.
	s.inboundPerIP.mu.Lock()
	defer s.inboundPerIP.mu.Unlock()
	if s.inboundPerIP.live["192.0.2.3"] != 0 {
		t.Fatal("rejected requests leaked per-IP slots")
	}
}

func TestLimiterFailsClosedAtCapacity(t *testing.T) {
	t.Parallel()
	l := newLimiter(2, time.Hour)
	for i := 0; i < maxTrackedIPs; i++ {
		if !l.Allow(fmt.Sprint(i)) {
			t.Fatal("premature key rejection")
		}
	}
	if l.Allow("overflow") || len(l.m) != maxTrackedIPs {
		t.Fatal("limiter grew beyond key ceiling")
	}
	if !l.Allow("0") {
		t.Fatal("existing bucket cannot use remaining allowance")
	}
	if l.Allow("0") {
		t.Fatal("existing bucket exceeded allowance")
	}
}
