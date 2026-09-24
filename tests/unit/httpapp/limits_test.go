package httpapp_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// signalWriter is a ResponseRecorder that signals once the handler has flushed,
// so a test can tell when a streaming request is live.
type signalWriter struct {
	*httptest.ResponseRecorder
	once  sync.Once
	ready chan struct{}
}

func newSignalWriter() *signalWriter {
	return &signalWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
}

func (w *signalWriter) Flush() {
	w.once.Do(func() { close(w.ready) })
	w.ResponseRecorder.Flush()
}

// TestEventStreamCappedPerCredential verifies one credential cannot park an
// unbounded number of SSE streams: the cap is 16 and the next is refused.
func TestEventStreamCappedPerCredential(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "stream", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	const cap = 16
	cancels := make([]context.CancelFunc, cap)
	writers := make([]*signalWriter, cap)
	for i := 0; i < cap; i++ {
		reqCtx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		w := newSignalWriter()
		writers[i] = w
		req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(reqCtx)
		req.Header.Set("Authorization", "Bearer "+key)
		go h.ServeHTTP(w, req)
	}
	for i, w := range writers {
		select {
		case <-w.ready:
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d did not start", i)
		}
	}
	req := httptest.NewRequest("GET", "/v1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("17th stream = %d, want 429", rr.Code)
	}
	for _, cancel := range cancels {
		cancel()
	}
}

// TestHSTSWhenForceHTTPS verifies the header is present on an HTTPS deployment
// that requires TLS.
func TestHSTSWhenForceHTTPS(t *testing.T) {
	svc, h, _, _, _ := httpFixture(t)
	svc.Config.ForceHTTPS = true
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.TLS = &tls.ConnectionState{}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got := rr.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("missing Strict-Transport-Security when FORCE_HTTPS=true")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", rr.Code)
	}
}
