package brevo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/transport"
	"gatehouse-mail/internal/transport/brevo"
	"gatehouse-mail/internal/transport/netutil"
)

// brevoPayload mirrors the unexported JSON payload that brevo.Send posts.
type brevoPayload struct {
	Sender struct {
		Name  string `json:"name,omitempty"`
		Email string `json:"email"`
	} `json:"sender"`
	To []struct {
		Name  string `json:"name,omitempty"`
		Email string `json:"email"`
	} `json:"to"`
	Subject     string            `json:"subject"`
	TextContent string            `json:"textContent,omitempty"`
	HTMLContent string            `json:"htmlContent,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachment  []struct {
		Name    string `json:"name"`
		Content []byte `json:"content"`
	} `json:"attachment,omitempty"`
}

func TestSendMapsPayloadAndParsesMessageID(t *testing.T) {
	var got *http.Request
	var body brevoPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<brevo-1@relay>"}`)
	}))
	defer srv.Close()

	res, err := brevo.Send(context.Background(), brevo.Config{APIKey: "xkeysib-test", APIBase: srv.URL}, transport.OutboundMessage{
		FromName:    "Hermes",
		FromAddress: "hermes@example.com",
		To:          []string{"friend@example.net"},
		CC:          []string{"cc@example.net"},
		Subject:     "Hello",
		Text:        "plain body",
		HTML:        "<p>rich body</p>",
		MessageID:   "<m1@example.com>",
		InReplyTo:   "<m0@example.com>",
		References:  []string{"<m0@example.com>"},
		Attachments: []transport.OutboundAttachment{{Filename: "quote.pdf", ContentType: "application/pdf", Content: []byte("pdf")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderMessageID != "<brevo-1@relay>" {
		t.Fatalf("message id %q", res.ProviderMessageID)
	}
	if got.URL.Path != "/v3/smtp/email" {
		t.Fatalf("path %s", got.URL.Path)
	}
	if got.Header.Get("api-key") != "xkeysib-test" || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %#v", got.Header)
	}
	if body.Sender.Email != "hermes@example.com" || body.Sender.Name != "Hermes" {
		t.Fatalf("sender %#v", body.Sender)
	}
	if len(body.To) != 1 || body.To[0].Email != "friend@example.net" {
		t.Fatalf("to %#v", body.To)
	}
	if body.Subject != "Hello" || body.TextContent != "plain body" || body.HTMLContent != "<p>rich body</p>" {
		t.Fatalf("content %#v", body)
	}
	if body.Headers["Message-ID"] != "<m1@example.com>" || body.Headers["In-Reply-To"] != "<m0@example.com>" {
		t.Fatalf("headers %#v", body.Headers)
	}
	if len(body.Attachment) != 1 || body.Attachment[0].Name != "quote.pdf" {
		t.Fatalf("attachments %#v", body.Attachment)
	}
}

func TestSendErrorsAndEmptyHTML(t *testing.T) {
	if _, err := brevo.Send(context.Background(), brevo.Config{}, transport.OutboundMessage{}); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("missing key err %v", err)
	}
	var body brevoPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := brevo.Send(context.Background(), brevo.Config{APIKey: "k", APIBase: srv.URL}, transport.OutboundMessage{FromAddress: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Text: "t"}); err == nil || !strings.Contains(err.Error(), "brevo returned") {
		t.Fatalf("error status %v", err)
	}
	if body.HTMLContent != "" {
		t.Fatalf("html should be omitted: %#v", body)
	}
}

func TestHostedRejectsPrivateAPIBase(t *testing.T) {
	netutil.SetHosted(true)
	defer netutil.SetHosted(false)
	_, err := brevo.Send(context.Background(), brevo.Config{APIKey: "k", APIBase: "http://127.0.0.1:9999"}, transport.OutboundMessage{FromAddress: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "not public-routable") {
		t.Fatalf("hosted private API base should be rejected: %v", err)
	}
}
