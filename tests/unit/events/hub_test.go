package events

import (
	"testing"
	"time"
)

func TestRegisterScopeCancelsOnRevoke(t *testing.T) {
	h := NewHub()
	ctx, unregister := h.RegisterScope("key:k1", "user:u1")
	defer unregister()
	h.CancelScope("key:k1")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("scope context was not cancelled")
	}
}

func TestUnregisterCancelsAndDetaches(t *testing.T) {
	h := NewHub()
	ctx, unregister := h.RegisterScope("key:k2")
	unregister()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("unregister did not cancel its own context")
	}
	// Cancelling the scope after unregister must be a safe no-op.
	h.CancelScope("key:k2")
}
