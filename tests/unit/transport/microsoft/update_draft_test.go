package microsoft_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/microsoft"
)

// TestUpdateDraftPatchesInPlaceAndVerifiesRecipients proves a placed Outlook
// draft is edited where Outlook keeps it — one PATCH of subject, body and
// recipients on the existing message id — and that the save is only reported as
// done after the recipients are read back. A new draft would lose the message's
// conversation link and history on every save.
func TestUpdateDraftPatchesInPlaceAndVerifiesRecipients(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var method, path string
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/messages/draft-1"):
			method, path = r.Method, r.URL.Path
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &patched)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"draft-1"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages/draft-1"):
			// The read-back the transport performs before reporting success.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"draft-1","subject":"edited","toRecipients":[{"emailAddress":{"address":"someone@example.com"}}],"ccRecipients":[],"bccRecipients":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := microsoft.NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, err := c.UpdateDraft(context.Background(), "token", "agent@remote.test", "draft-1", microsoft.DraftEdit{
		Subject: "edited", Text: "body", To: []string{"someone@example.com"},
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", method)
	}
	if !strings.HasSuffix(path, "/users/agent@remote.test/messages/draft-1") {
		t.Fatalf("path = %s, want the existing message id", path)
	}
	if patched["subject"] != "edited" {
		t.Fatalf("patched subject = %v", patched["subject"])
	}
	body, ok := patched["body"].(map[string]any)
	if !ok || body["content"] != "body" {
		t.Fatalf("patched body = %v", patched["body"])
	}
	if _, ok := patched["toRecipients"]; !ok {
		t.Fatal("patch did not carry the edited recipients")
	}
}

// TestUpdateDraftStopsWhenTheRecipientsDoNotStick proves a PATCH whose result
// does not match is reported as a failure: a successful HTTP call is not proof
// the draft now says what the operator typed.
func TestUpdateDraftStopsWhenTheRecipientsDoNotStick(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			// No recipients survived the patch.
			_, _ = w.Write([]byte(`{"id":"draft-1","toRecipients":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"draft-1"}`))
	}))
	defer srv.Close()

	c := microsoft.NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, err := c.UpdateDraft(context.Background(), "token", "agent@remote.test", "draft-1", microsoft.DraftEdit{
		Subject: "edited", Text: "body", To: []string{"someone@example.com"},
	}); err == nil {
		t.Fatal("a draft that did not keep its recipients must not be reported as saved")
	}
}

// TestUpdateDraftStopsWhenADifferentRecipientStuck proves the read-back compares
// addresses, not counts: a list of the right length naming someone else fails.
func TestUpdateDraftStopsWhenADifferentRecipientStuck(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"draft-1","toRecipients":[{"emailAddress":{"address":"someone-else@example.com"}}],"ccRecipients":[],"bccRecipients":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"draft-1"}`))
	}))
	defer srv.Close()

	c := microsoft.NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, err := c.UpdateDraft(context.Background(), "token", "agent@remote.test", "draft-1", microsoft.DraftEdit{
		Subject: "edited", Text: "body", To: []string{"someone@example.com"},
	}); err == nil {
		t.Fatal("a draft that kept a different recipient of the same count must not be reported as saved")
	}
}

// TestUpdateDraftAcceptsReorderedAndRecasedRecipients proves the address check is
// not brittle: Graph may reorder or re-case what it stores.
func TestUpdateDraftAcceptsReorderedAndRecasedRecipients(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"draft-1","toRecipients":[{"emailAddress":{"address":"B@Example.com"}},{"emailAddress":{"address":"a@example.com"}}],"ccRecipients":[],"bccRecipients":[{"emailAddress":{"address":"hidden@example.com"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"draft-1"}`))
	}))
	defer srv.Close()

	c := microsoft.NewClient(srv.Client())
	c.BaseURL = srv.URL
	if _, err := c.UpdateDraft(context.Background(), "token", "agent@remote.test", "draft-1", microsoft.DraftEdit{
		Subject: "s", Text: "body", To: []string{"a@example.com", "b@example.com"}, BCC: []string{"Hidden <hidden@example.com>"},
	}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
}

// TestDraftStateReportsRevisionAndBcc proves the live read a save relies on:
// the item's changeKey and its Bcc list, which the MIME rendering omits.
func TestDraftStateReportsRevisionAndBcc(t *testing.T) {
	t.Parallel()
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"draft-1","changeKey":"CK-7","bccRecipients":[{"emailAddress":{"address":"hidden@example.com"}}]}`))
	}))
	defer srv.Close()

	c := microsoft.NewClient(srv.Client())
	c.BaseURL = srv.URL
	m, err := c.DraftState(context.Background(), "token", "agent@remote.test", "draft-1")
	if err != nil {
		t.Fatalf("DraftState: %v", err)
	}
	if m.ChangeKey != "CK-7" || len(m.BCCRecipients) != 1 || m.BCCRecipients[0].EmailAddress.Address != "hidden@example.com" {
		t.Fatalf("DraftState = %+v", m)
	}
	if !strings.Contains(query, "changeKey") || !strings.Contains(query, "bccRecipients") {
		t.Fatalf("select did not ask for the revision and Bcc: %s", query)
	}
}
