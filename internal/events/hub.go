package events

import (
	"context"
	"sync"

	"github.com/dellarb/mailmoose/internal/model"
)

type Hub struct {
	mu           sync.RWMutex
	next         int
	subs         map[int]chan model.Event
	scopeNext    int
	scopeCancels map[string]map[int]context.CancelFunc
}

func NewHub() *Hub {
	return &Hub{subs: map[int]chan model.Event{}, scopeCancels: map[string]map[int]context.CancelFunc{}}
}
func (h *Hub) Subscribe(buffer int) (int, <-chan model.Event, func()) {
	if buffer < 1 {
		buffer = 16
	}
	h.mu.Lock()
	id := h.next
	h.next++
	ch := make(chan model.Event, buffer)
	h.subs[id] = ch
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
		h.mu.Unlock()
	}
	return id, ch, cancel
}
func (h *Hub) Publish(e model.Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// RegisterScope returns a context that is cancelled when any of the given
// scopes is revoked. Long-lived connections (SSE, relay sockets) register their
// credential scopes so revoking, rotating, or rescoping a credential terminates
// them immediately instead of only blocking the next authentication.
func (h *Hub) RegisterScope(scopes ...string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	id := h.scopeNext
	h.scopeNext++
	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		if h.scopeCancels[scope] == nil {
			h.scopeCancels[scope] = map[int]context.CancelFunc{}
		}
		h.scopeCancels[scope][id] = cancel
	}
	h.mu.Unlock()
	unregister := func() {
		h.mu.Lock()
		for _, scope := range scopes {
			if m := h.scopeCancels[scope]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(h.scopeCancels, scope)
				}
			}
		}
		h.mu.Unlock()
		cancel()
	}
	return ctx, unregister
}

// CancelScope cancels every registered connection for a scope.
func (h *Hub) CancelScope(scope string) {
	if scope == "" {
		return
	}
	h.mu.Lock()
	cancels := h.scopeCancels[scope]
	delete(h.scopeCancels, scope)
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
