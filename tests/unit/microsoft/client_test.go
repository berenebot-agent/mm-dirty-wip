package microsoft_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/transport"
	"github.com/dellarb/mammothmail/internal/transport/microsoft"
)

func TestCursorBoundaryAndNativeReply(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
			t.Error("missing immutable ID preference")
		}
		switch {
		case r.Method == "GET" && r.URL.Query().Get("$filter") != "":
			w.Write([]byte(`{"value":[{"id":"original"}]}`))
		case strings.HasSuffix(r.URL.Path, "/original/createReply"):
			b, _ := io.ReadAll(r.Body)
			raw, e := base64.StdEncoding.DecodeString(string(b))
			if e != nil || !strings.Contains(string(raw), "In-Reply-To: <original@example.net>") || r.Header.Get("Content-Type") != "text/plain" {
				t.Error("invalid MIME reply")
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"draft"}`))
		case strings.HasSuffix(r.URL.Path, "/draft/send"):
			w.WriteHeader(202)
		default:
			w.Write([]byte(`{"value":[]}`))
		}
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	for _, cursor := range []string{s.URL + "/users/other/messages?token=x", "https://evil.example/users/selected/messages", s.URL + "/users/selected/mailFolders/inbox/messages"} {
		if _, e := c.Messages(context.Background(), "token", "selected", "", "", cursor, 100); e == nil {
			t.Fatalf("cursor accepted %s", cursor)
		}
	}
	if calls != 0 {
		t.Fatal("invalid cursor reached provider")
	}
	d, e := c.CreateDraft(context.Background(), "token", "selected", strings.NewReader("From: selected@example.net\r\nTo: sender@example.net\r\nIn-Reply-To: <original@example.net>\r\nSubject: Re: Hello\r\n\r\nReply"))
	if e != nil || d.ID != "draft" {
		t.Fatalf("draft %#v %v", d, e)
	}
	if e = c.SendDraft(context.Background(), "token", "selected", d.ID); e != nil {
		t.Fatal(e)
	}
}

func TestSharedMailboxDiscoveryProbesAccess(t *testing.T) {
	var mu sync.Mutex
	probed := map[string]int{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users":
			if got := r.URL.Query().Get("$select"); !strings.Contains(got, "displayName") || !strings.Contains(got, "mail") {
				t.Errorf("directory select: %s", got)
			}
			w.Write([]byte(`{"value":[{"id":"1","displayName":"Owner","mail":"owner@example.net"},{"id":"2","displayName":"Box","mail":"box@example.net"},{"id":"3","displayName":"Other","mail":"other@example.net"},{"id":"4","displayName":"NoMail","mail":""}]}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			mu.Lock()
			probed[r.URL.Path]++
			mu.Unlock()
			if r.URL.Path != "/users/box@example.net/mailFolders/inbox" {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte(`{"id":"inbox"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	found, e := c.SharedMailboxes(context.Background(), "token", "owner@example.net")
	if e != nil {
		t.Fatal(e)
	}
	if len(found) != 1 || found[0].Mail != "box@example.net" || found[0].DisplayName != "Box" {
		t.Fatalf("discovery %#v", found)
	}
	mu.Lock()
	defer mu.Unlock()
	if probed["/users/owner@example.net/mailFolders/inbox"] != 0 {
		t.Fatal("signed-in mailbox was probed")
	}
	if probed["/users/other@example.net/mailFolders/inbox"] != 1 {
		t.Fatal("candidate without access was not probed")
	}
}

func TestSendServerFailureIsAmbiguous(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("secret provider details"))
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	e := c.SendDraft(context.Background(), "secret-token", "mailbox", "draft")
	if !transport.AsAmbiguous(e) || strings.Contains(e.Error(), "secret") {
		t.Fatalf("unsafe retry classification: %v", e)
	}
}

func TestFreshDraftRecipientRepairAndMismatchStopsSend(t *testing.T) {
	for _, ignorePatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair", true: "mismatch"}[ignorePatch], func(t *testing.T) {
			to := []map[string]any{{"emailAddress": map[string]string{"address": "b.dellar@outlook.com"}}}
			cc, bcc := []map[string]any{}, []map[string]any{}
			sent := false
			patched := false
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
					b, _ := io.ReadAll(r.Body)
					raw, e := base64.StdEncoding.DecodeString(string(b))
					if e != nil || !strings.Contains(string(raw), "To: dellarb@gmail.com") || strings.Contains(string(raw), "In-Reply-To:") {
						t.Fatal("not a fresh composed message")
					}
					w.WriteHeader(201)
					w.Write([]byte(`{"id":"draft","toRecipients":[{"emailAddress":{"address":"b.dellar@outlook.com"}}]}`))
				case r.Method == "PATCH":
					var body struct {
						To  []map[string]any `json:"toRecipients"`
						CC  []map[string]any `json:"ccRecipients"`
						BCC []map[string]any `json:"bccRecipients"`
					}
					if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
						t.Error(e)
					}
					patched = true
					if !ignorePatch {
						to, cc, bcc = body.To, body.CC, body.BCC
					}
					w.Write([]byte(`{"id":"draft"}`))
				case r.Method == "GET":
					json.NewEncoder(w).Encode(map[string]any{"id": "draft", "toRecipients": to, "ccRecipients": cc, "bccRecipients": bcc})
				case strings.HasSuffix(r.URL.Path, "/send"):
					sent = true
					w.WriteHeader(202)
				default:
					t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
				}
			}))
			defer s.Close()
			c := microsoft.NewClient(s.Client())
			c.BaseURL = s.URL
			d, e := c.CreateVerifiedDraft(context.Background(), "token", "mailbox", []byte("From: b.dellar@outlook.com\r\nTo: dellarb@gmail.com\r\nCc: copied@example.net\r\nBcc: hidden@example.net\r\nSubject: Microsoft\r\n\r\nHello"))
			if !patched {
				t.Fatal("draft recipients were not explicitly set")
			}
			if ignorePatch {
				if e == nil || !transport.AsAmbiguous(e) {
					t.Fatalf("mismatch accepted: %v", e)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if e = c.SendDraft(context.Background(), "token", "mailbox", d.ID); e != nil {
					t.Fatal(e)
				}
				if len(cc) != 1 || len(bcc) != 1 || to[0]["emailAddress"].(map[string]any)["address"] != "dellarb@gmail.com" {
					t.Fatal("recipient envelope lost")
				}
			}
			if sent == ignorePatch {
				t.Fatalf("submission state: sent=%v ignorePatch=%v", sent, ignorePatch)
			}
		})
	}
}

// TestBackoffIsPerMailbox proves one mailbox's 429 does not stall others: a
// 429/Retry-After response for mailbox A defers A's follow-up requests without
// touching the server again, while mailbox B continues immediately.
func TestBackoffIsPerMailbox(t *testing.T) {
	var mu sync.Mutex
	aCalls, bCalls := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/users/a@outlook.test/"):
			aCalls++
			if aCalls == 1 {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		case strings.HasPrefix(r.URL.Path, "/users/b@outlook.test/"):
			bCalls++
		}
		w.Write([]byte(`{"value":[]}`))
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	ctx := context.Background()
	if _, err := c.Messages(ctx, "token", "a@outlook.test", "", "", "", 10); err == nil {
		t.Fatal("expected the 429 to surface as an error")
	}
	// Mailbox A is deferred: the retry fails without another server request.
	if _, err := c.Messages(ctx, "token", "a@outlook.test", "", "", "", 10); err == nil || !strings.Contains(err.Error(), "backoff in progress") {
		t.Fatalf("mailbox A after 429 = %v, want backoff error without a server call", err)
	}
	// Mailbox B continues immediately.
	if _, err := c.Messages(ctx, "token", "b@outlook.test", "", "", "", 10); err != nil {
		t.Fatalf("mailbox B blocked by A's backoff: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if aCalls != 1 {
		t.Fatalf("mailbox A server hits = %d, want 1 (the 429)", aCalls)
	}
	if bCalls != 1 {
		t.Fatalf("mailbox B server hits = %d, want 1", bCalls)
	}
}

// TestMessageByInternetIDRequiresOneMatch proves the retired-id lookup never
// guesses: only a single match is a message, while no match and an ambiguous
// Message-ID (a resent copy) both report not found.
func TestMessageByInternetIDRequiresOneMatch(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/users/selected/messages") {
			t.Errorf("lookup path = %s", r.URL.Path)
		}
		filter := r.URL.Query().Get("$filter")
		switch {
		case strings.Contains(filter, "<one@example.net>"):
			w.Write([]byte(`{"value":[{"id":"one"}]}`))
		case strings.Contains(filter, "<two@example.net>"):
			w.Write([]byte(`{"value":[{"id":"a"},{"id":"b"}]}`))
		default:
			w.Write([]byte(`{"value":[]}`))
		}
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	ctx := context.Background()
	if m, e := c.MessageByInternetID(ctx, "token", "selected", " <one@example.net> "); e != nil || m.ID != "one" {
		t.Fatalf("single match = %#v %v", m, e)
	}
	for _, rfc := range []string{"<two@example.net>", "<none@example.net>", ""} {
		if _, e := c.MessageByInternetID(ctx, "token", "selected", rfc); e == nil {
			t.Fatalf("%q was accepted as one message", rfc)
		}
	}
}

// TestCreateFolderRejectionCarriesGraphCode proves a Graph refusal surfaces the
// status and error code instead of one opaque message, so an operator can tell a
// bad parent id from missing consent or throttling. The raw body is not echoed.
func TestCreateFolderRejectionCarriesGraphCode(t *testing.T) {
	t.Parallel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/users/box@example.net/mailFolders/Projects/childFolders" {
			t.Errorf("create route %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied. secret-token"}}`))
	}))
	defer s.Close()
	c := microsoft.NewClient(s.Client())
	c.BaseURL = s.URL
	_, e := c.CreateFolder(context.Background(), "secret-token", "box@example.net", "Projects", "Child")
	if e == nil || !strings.Contains(e.Error(), "403 ErrorAccessDenied") || !strings.Contains(e.Error(), "Access is denied") {
		t.Fatalf("rejection detail missing: %v", e)
	}
	var mb *model.MailboxError
	if !errors.As(e, &mb) || mb.Kind != model.ErrKindForbidden {
		t.Fatalf("kind lost: %v", e)
	}
}
