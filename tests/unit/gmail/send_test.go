package gmail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
)

func TestSendRaw(t *testing.T) {
	const mime = "From: me@example.com\r\nSubject: Hi\r\n\r\nbody"
	cases := []struct{ name, threadID string }{
		{"threaded", "t1"},
		{"unthreaded", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth string
			var got struct {
				Raw      string `json:"raw"`
				ThreadID string `json:"threadId"`
			}
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"m1","threadId":"t1","labelIds":["SENT"],"internalDate":"1700000000000"}`)
			}))

			msg, err := c.SendRaw(context.Background(), "tok", strings.NewReader(mime), tc.threadID)
			if err != nil {
				t.Fatalf("SendRaw: %v", err)
			}
			if gotPath != apiPrefix+"/messages/send" {
				t.Fatalf("path = %q", gotPath)
			}
			if gotAuth != "Bearer tok" {
				t.Fatalf("auth = %q", gotAuth)
			}
			if got.ThreadID != tc.threadID {
				t.Fatalf("threadId = %q want %q", got.ThreadID, tc.threadID)
			}
			decoded, err := base64.RawURLEncoding.DecodeString(got.Raw)
			if err != nil {
				t.Fatalf("decode raw: %v", err)
			}
			if string(decoded) != mime {
				t.Fatalf("raw = %q want %q", decoded, mime)
			}
			if msg.ID != "m1" || msg.LabelIDs[0] != "SENT" || msg.InternalDate != 1700000000000 {
				t.Fatalf("message = %#v", msg)
			}
		})
	}
}

func TestCreateDraft(t *testing.T) {
	const mime = "From: me@example.com\r\n\r\nbody"
	var gotPath string
	var got struct {
		Message struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"d1","message":{"id":"m1","threadId":"t1"}}`)
	}))

	d, err := c.CreateDraft(context.Background(), "tok", strings.NewReader(mime), "t1")
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if gotPath != apiPrefix+"/drafts" {
		t.Fatalf("path = %q", gotPath)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(got.Message.Raw)
	if err != nil || string(decoded) != mime {
		t.Fatalf("raw = %q err %v", decoded, err)
	}
	if got.Message.ThreadID != "t1" || d.ID != "d1" || d.Message.ID != "m1" {
		t.Fatalf("draft = %#v", d)
	}
}

func TestListDraftsAndGetDraft(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == apiPrefix+"/drafts":
			if r.URL.Query().Get("pageToken") != "p1" || r.URL.Query().Get("maxResults") != "10" {
				t.Errorf("list query = %v", r.URL.RawQuery)
			}
			io.WriteString(w, `{"drafts":[{"id":"d1","message":{"id":"m1"}}],"nextPageToken":"p2","resultSizeEstimate":1}`)
		case r.URL.Path == apiPrefix+"/drafts/d1":
			if r.URL.Query().Get("format") != "metadata" {
				t.Errorf("get format = %q", r.URL.Query().Get("format"))
			}
			io.WriteString(w, `{"id":"d1","message":{"id":"m1","threadId":"t1"}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))

	page, err := c.ListDrafts(context.Background(), "tok", "p1", 10)
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	if len(page.Drafts) != 1 || page.NextPageToken != "p2" {
		t.Fatalf("page = %#v", page)
	}
	d, err := c.GetDraft(context.Background(), "tok", "d1", "metadata")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if d.Message.ThreadID != "t1" {
		t.Fatalf("draft = %#v", d)
	}
}

func TestSendErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   string
	}{
		{"unauthorized", 401, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, model.ErrKindAuth},
		{"forbidden", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`, model.ErrKindForbidden},
		{"notfound", 404, `{"error":{"code":404,"status":"NOT_FOUND"}}`, model.ErrKindNotFound},
		{"invalid", 400, `{"error":{"code":400,"status":"INVALID_ARGUMENT"}}`, model.ErrKindInvalid},
		{"ratelimited", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","errors":[{"reason":"rateLimitExceeded"}]}}`, model.ErrKindRetryable},
		{"conflict", 409, `{"error":{"code":409,"status":"ABORTED"}}`, model.ErrKindConflict},
		{"server", 500, `{"error":{"code":500,"status":"INTERNAL"}}`, model.ErrKindUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			_, err := c.SendRaw(context.Background(), "tok", strings.NewReader("mime"), "")
			if err == nil {
				t.Fatal("expected error")
			}
			if transport.AsAmbiguous(err) {
				t.Fatalf("definitive rejection marked ambiguous: %v", err)
			}
			assertKind(t, err, tc.kind)
			if contains(err.Error(), "status\":") || contains(err.Error(), "INTERNAL") {
				t.Fatalf("error leaked provider body: %q", err.Error())
			}
		})
	}
}

func TestSendTimeoutIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := gmail.NewClient(&http.Client{Timeout: 50 * time.Millisecond})
	c.BaseURL = srv.URL
	c.TokenURL = srv.URL + "/token"

	_, err := c.SendRaw(context.Background(), "tok", strings.NewReader("mime"), "t1")
	if !transport.AsAmbiguous(err) {
		t.Fatalf("err = %v, want ambiguous", err)
	}
}

func TestCreateDraftTimeoutIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := gmail.NewClient(&http.Client{Timeout: 50 * time.Millisecond})
	c.BaseURL = srv.URL
	c.TokenURL = srv.URL + "/token"

	_, err := c.CreateDraft(context.Background(), "tok", strings.NewReader("mime"), "")
	if !transport.AsAmbiguous(err) {
		t.Fatalf("err = %v, want ambiguous", err)
	}
}

func TestSendBodyErrorIsDefinitive(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"id":"m1"}`)
	}))

	_, err := c.SendRaw(context.Background(), "tok", errReader{}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if transport.AsAmbiguous(err) {
		t.Fatalf("local body error must not be ambiguous: %v", err)
	}
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindInternal {
		t.Fatalf("err = %v, want internal MailboxError", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
