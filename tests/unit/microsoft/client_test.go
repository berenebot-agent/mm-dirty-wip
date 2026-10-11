package microsoft_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
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
