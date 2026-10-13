package httpapp

import (
	"net/http"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
)

// flashStoreMaxBytes bounds the total bytes held by the flash store. Compose
// flashes can carry attachments, so it is larger than a status notice, but it
// is deliberately far below the 64 MiB the initial design allowed. Owner quotas
// and admission without cross-owner eviction keep one caller from displacing
// another caller's drafts or secrets.
const flashStoreMaxBytes = 8 << 20

const flashOwnerMaxEntries = 8
const flashOwnerMaxBytes = 2 << 20

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
	owner   string
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
func (f *flashStore) put(owner string, v any, size int) string {
	if owner == "" {
		return ""
	}
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
	var ownerN, ownerBytes int
	for _, e := range f.entries {
		if e.owner == owner {
			ownerN++
			ownerBytes += e.size
		}
	}
	if ownerN >= flashOwnerMaxEntries || size > flashOwnerMaxBytes-ownerBytes || len(f.entries) >= f.maxN || (f.maxBytes > 0 && size > f.maxBytes-f.bytes) {
		return ""
	}
	f.entries[tok] = flashEntry{owner: owner, value: v, size: size, expires: time.Now().Add(f.ttl)}
	f.bytes += size
	return tok
}

// peek returns the stored value without consuming it, so the GET render and a
// subsequent resubmit can both read it.
func (f *flashStore) peek(owner, tok string) (any, bool) {
	if tok == "" {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[tok]
	if !ok || owner == "" || e.owner != owner {
		return nil, false
	}
	if time.Now().After(e.expires) {
		f.removeLocked(tok)
		return nil, false
	}
	return e.value, true
}

// take consumes and returns the stored value.
func (f *flashStore) take(owner, tok string) (any, bool) {
	if tok == "" {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[tok]
	if !ok || owner == "" || e.owner != owner {
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

func (f *flashStore) removeLocked(tok string) {
	if e, ok := f.entries[tok]; ok {
		f.bytes -= e.size
		delete(f.entries, tok)
	}
}

// User/client identity survives re-login, but never permits a different user
// (even in the same account) to read a draft or one-time secret.
func (s *Server) flashOwner(r *http.Request) string {
	p := principal(r)
	if p.UserID != "" {
		return "user:" + p.AccountID + ":" + p.UserID
	}
	if p.APIKeyID != "" {
		return "key:" + p.AccountID + ":" + p.APIKeyID
	}
	if c, err := r.Cookie(preAuthCSRFCookie); err == nil && s.validPreAuthCSRF(c.Value) {
		return "browser:" + auth.HashToken(c.Value)
	}
	return ""
}

func (s *Server) putFlash(w http.ResponseWriter, r *http.Request, v any, size int) string {
	p := principal(r)
	if p.UserID == "" && p.APIKeyID == "" && size > 4096 {
		return ""
	}
	owner := s.flashOwner(r)
	if owner == "" {
		// Anonymous auth notices are browser-bound and deliberately small.
		tok := s.setPreAuthCSRF(w, r)
		if tok == "" {
			return ""
		}
		owner = "browser:" + auth.HashToken(tok)
	}
	return s.flashes.put(owner, v, size)
}

func (s *Server) peekFlash(r *http.Request, tok string) (any, bool) {
	return s.flashes.peek(s.flashOwner(r), tok)
}

func (s *Server) takeFlash(r *http.Request, tok string) (any, bool) {
	return s.flashes.take(s.flashOwner(r), tok)
}
