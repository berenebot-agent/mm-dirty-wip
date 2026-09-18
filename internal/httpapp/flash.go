package httpapp

import (
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
)

// flashStore holds short-lived, one-shot state between a POST that redirects
// and the GET that renders it (Post/Redirect/Get). It exists so refreshing a
// page never re-submits a form and never triggers the browser's resubmission
// prompt. Entries are in-memory only and expire; a restart drops them.
type flashStore struct {
	mu       sync.Mutex
	entries  map[string]flashEntry
	maxN     int
	maxBytes int
	bytes    int
	ttl      time.Duration
}

type flashEntry struct {
	value   any
	size    int
	expires time.Time
}

func newFlashStore(maxEntries, maxBytes int) *flashStore {
	if maxEntries <= 0 {
		maxEntries = 64
	}
	return &flashStore{entries: map[string]flashEntry{}, maxN: maxEntries, maxBytes: maxBytes, ttl: 15 * time.Minute}
}

// put stores v and returns a random token. It returns "" when v is larger than
// the whole store or a token cannot be generated.
func (f *flashStore) put(v any, size int) string {
	if size < 0 {
		size = 0
	}
	if f.maxBytes > 0 && size > f.maxBytes {
		return ""
	}
	tok, err := auth.RandomToken(16)
	if err != nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(time.Now())
	for f.maxBytes > 0 && len(f.entries) > 0 && f.bytes+size > f.maxBytes {
		f.evictOldestLocked()
	}
	for len(f.entries) >= f.maxN {
		f.evictOldestLocked()
	}
	f.entries[tok] = flashEntry{value: v, size: size, expires: time.Now().Add(f.ttl)}
	f.bytes += size
	return tok
}

// peek returns the stored value without consuming it, so the GET render and a
// subsequent resubmit can both read it.
func (f *flashStore) peek(tok string) (any, bool) {
	if tok == "" {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[tok]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expires) {
		f.removeLocked(tok)
		return nil, false
	}
	return e.value, true
}

// take consumes and returns the stored value.
func (f *flashStore) take(tok string) (any, bool) {
	if tok == "" {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[tok]
	if !ok {
		return nil, false
	}
	f.removeLocked(tok)
	if time.Now().After(e.expires) {
		return nil, false
	}
	return e.value, true
}

func (f *flashStore) pruneLocked(now time.Time) {
	for k, e := range f.entries {
		if now.After(e.expires) {
			f.removeLocked(k)
		}
	}
}

func (f *flashStore) evictOldestLocked() {
	var oldest string
	var oldestExp time.Time
	for k, e := range f.entries {
		if oldest == "" || e.expires.Before(oldestExp) {
			oldest, oldestExp = k, e.expires
		}
	}
	if oldest != "" {
		f.removeLocked(oldest)
	}
}

func (f *flashStore) removeLocked(tok string) {
	if e, ok := f.entries[tok]; ok {
		f.bytes -= e.size
		delete(f.entries, tok)
	}
}
