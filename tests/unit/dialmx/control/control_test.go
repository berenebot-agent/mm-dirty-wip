package control_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// fakeHandler is an in-memory control.Handler that records the calls it
// receives and can be told to fail. It is safe for concurrent use because the
// control loop calls it from one goroutine while the test observes flags.
type fakeHandler struct {
	mu       sync.Mutex
	settings *control.Settings
	state    control.State
	err      error
	closed   bool
}

func newFakeHandler() *fakeHandler { return &fakeHandler{state: control.StateStandby} }

func (h *fakeHandler) Configure(_ context.Context, s control.Settings) (control.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return h.statusLocked(), h.err
	}
	h.settings = &s
	h.state = control.StateActive
	return h.statusLocked(), nil
}

func (h *fakeHandler) Deactivate(context.Context) (control.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = control.StateStandby
	return h.statusLocked(), nil
}

func (h *fakeHandler) Status() control.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked()
}

func (h *fakeHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

func (h *fakeHandler) statusLocked() control.Status {
	st := control.Status{State: h.state, SessionAddr: "127.0.0.1:8443", SMTPAddr: ":2525"}
	if h.settings != nil {
		st.ActiveConnections = 1
	}
	return st
}

// startPair runs a control.Serve loop over one end of a socketpair and returns
// a client over the other end plus a stop function.
func startPair(t *testing.T, h control.Handler) *control.Client {
	t.Helper()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = control.Serve(ctx, server, server, h, nil)
	}()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		_ = client.Close()
		<-done
	})
	return control.NewClient(client, client)
}

func TestClientConfigureStatusDeactivate(t *testing.T) {
	h := newFakeHandler()
	c := startPair(t, h)

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != control.StateStandby {
		t.Fatalf("initial state %q", st.State)
	}

	settings := control.Settings{
		Hostname: "mx.test",
		Secret:   "auto-generated",
		SMTP:     mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20},
	}
	st, err = c.Configure(context.Background(), settings)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if st.State != control.StateActive {
		t.Fatalf("configure state %q", st.State)
	}
	h.mu.Lock()
	got := h.settings
	h.mu.Unlock()
	if got == nil || got.Secret != "auto-generated" || got.Hostname != "mx.test" {
		t.Fatalf("handler settings round trip: %+v", got)
	}

	st, err = c.Deactivate(context.Background())
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if st.State != control.StateStandby {
		t.Fatalf("deactivate state %q", st.State)
	}
}

func TestClientShutdownClosesHandler(t *testing.T) {
	h := newFakeHandler()
	c := startPair(t, h)
	if _, err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if !closed {
		t.Fatal("handler not closed")
	}
}

func TestClientPropagatesHandlerError(t *testing.T) {
	h := newFakeHandler()
	h.err = errors.New("activation refused")
	c := startPair(t, h)
	if _, err := c.Configure(context.Background(), control.Settings{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientHonoursContextDeadline(t *testing.T) {
	// A server that never reads leaves the client blocked; the deadline must
	// release it.
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	c := control.NewClient(client, client)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Status(ctx); err == nil {
		t.Fatal("expected deadline error")
	}
}

func TestClientConcurrentCalls(t *testing.T) {
	h := newFakeHandler()
	c := startPair(t, h)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := c.Configure(context.Background(), control.Settings{Hostname: "mx.test"}); err != nil {
					t.Errorf("configure: %v", err)
				}
				return
			}
			if _, err := c.Status(context.Background()); err != nil {
				t.Errorf("status: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

func TestServeRejectsOversizeFrame(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- control.Serve(context.Background(), server, server, newFakeHandler(), nil) }()

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], control.MaxFrameBytes+1)
	if _, err := client.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected oversize frame error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not reject oversize frame")
	}
	_ = client.Close()
	_ = server.Close()
}

func TestServeReturnsOnEOF(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- control.Serve(context.Background(), server, server, newFakeHandler(), nil) }()
	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean EOF should return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return on EOF")
	}
	_ = server.Close()
}

func TestEnabledRequiresBothDescriptors(t *testing.T) {
	t.Setenv(control.CmdFDEnv, "3")
	t.Setenv(control.ReplyFDEnv, "")
	if control.Enabled() {
		t.Fatal("enabled with only the command descriptor")
	}
	t.Setenv(control.ReplyFDEnv, "4")
	if !control.Enabled() {
		t.Fatal("both descriptors set should enable control mode")
	}
}

func TestMaxFrameBytesIsNarrow(t *testing.T) {
	if control.MaxFrameBytes > 64<<10 {
		t.Fatalf("control frame bound %d exceeds 64 KiB", control.MaxFrameBytes)
	}
	// A full mxagent.Config plus hostname and secret must fit comfortably.
	if control.MaxFrameBytes < 8<<10 {
		t.Fatalf("control frame bound %d is too small for settings", control.MaxFrameBytes)
	}
}

// TestClientPoisonsAfterTimeout proves a timed-out round trip makes the channel
// fail fast on later calls instead of reading a stale reply and misattributing
// it to the wrong request.
func TestClientPoisonsAfterTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Read one request, then hold the reply until released. This simulates a
	// slow child whose reply arrives after the caller's deadline.
	readDone := make(chan struct{})
	release := make(chan struct{})
	go func() {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(server, buf); err != nil {
			close(readDone)
			return
		}
		n := binary.BigEndian.Uint32(buf)
		if _, err := io.ReadFull(server, make([]byte, n)); err != nil {
			close(readDone)
			return
		}
		close(readDone)
		<-release
		// A valid-looking reply that must never be consumed by a later call.
		_, _ = server.Write([]byte{0, 0, 0, 2, '{', '}'})
	}()

	c := control.NewClient(client, client)
	// A short message makes the fake server complete its read, then the reply is
	// withheld and the deadline fires.
	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Use Deactivate (empty request) as the request shape.
	if _, err := c.Deactivate(shortCtx); err == nil {
		t.Fatal("expected timeout")
	}
	<-readDone
	close(release)

	// A later call must fail with the poison error and must not read the stale
	// reply; a clean status would prove desynchronisation.
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("poisoned client accepted a stale reply")
	}
}

// TestClientCleanErrorReplyDoesNotPoison proves a well-formed error reply keeps
// the channel usable: the child deliberately rejected the request but the
// framing is intact.
func TestClientCleanErrorReplyDoesNotPoison(t *testing.T) {
	h := newFakeHandler()
	h.err = errors.New("rejected")
	c := startPair(t, h)
	if _, err := c.Configure(context.Background(), control.Settings{}); err == nil {
		t.Fatal("expected rejection")
	}
	h.mu.Lock()
	h.err = nil
	h.mu.Unlock()
	if st, err := c.Status(context.Background()); err != nil || st.State != control.StateStandby {
		t.Fatalf("channel should remain usable after a clean rejection: %+v, %v", st, err)
	}
}
