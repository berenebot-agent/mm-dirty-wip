package gmail_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/gmail"
)

// TestUpdateDraftPutsTheDraftInPlace proves a placed draft is edited where Gmail
// keeps it: drafts.update is a PUT of the whole message to the draft's own id,
// not a create-and-delete pair that would churn the draft to the operator.
func TestUpdateDraftPutsTheDraftInPlace(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var method, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method, path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"draft-1","message":{"id":"msg-1"}}`))
	}))
	defer srv.Close()

	c := gmail.NewClient(srv.Client())
	c.BaseURL = srv.URL
	raw := "Subject: edited\r\n\r\nbody\r\n"
	if _, err := c.UpdateDraft(context.Background(), "token", "draft-1", strings.NewReader(raw), ""); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPut {
		t.Fatalf("method = %s, want PUT", method)
	}
	if path != "/gmail/v1/users/me/drafts/draft-1" {
		t.Fatalf("path = %s, want the draft's own id", path)
	}
	// The message travels as Gmail's base64url raw envelope.
	if !strings.Contains(body, `"raw":`) {
		t.Fatalf("body is not a raw envelope: %s", body)
	}
}

// TestDraftIDForMessageResolvesTheDraftsOwnID proves the write path can find the
// draft that holds a cached message: drafts.update is addressed by draft id,
// while the cached remote index stores message ids.
func TestDraftIDForMessageResolvesTheDraftsOwnID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gmail/v1/users/me/drafts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"drafts":[{"id":"draft-a","message":{"id":"msg-a"}},{"id":"draft-b","message":{"id":"msg-b"}}]}`))
	}))
	defer srv.Close()

	c := gmail.NewClient(srv.Client())
	c.BaseURL = srv.URL
	got, err := c.DraftIDForMessage(context.Background(), "token", "msg-b")
	if err != nil {
		t.Fatalf("DraftIDForMessage: %v", err)
	}
	if got != "draft-b" {
		t.Fatalf("draft id = %q, want draft-b", got)
	}
	if _, err = c.DraftIDForMessage(context.Background(), "token", "msg-missing"); err == nil {
		t.Fatal("a message with no draft must not resolve to an id")
	}
}
