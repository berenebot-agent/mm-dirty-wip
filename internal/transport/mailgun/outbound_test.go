package mailgun

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"

	"gatehouse-mail/internal/transport"
)

func TestOutboundSendMultipartAttachment(t *testing.T) {
	var contentType string
	var fields map[string]string
	var files map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		fields, files = readMultipart(t, r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<mg-1>"}`)
	}))
	defer srv.Close()

	res, err := Send(context.Background(), Config{APIKey: "key", Domain: "mg.example.com", APIBase: srv.URL}, SendRequest{
		From:        "Hermes <hermes@example.com>",
		To:          []string{"friend@example.net"},
		Subject:     "Hi",
		Text:        "body",
		MessageID:   "<m1@example.com>",
		Attachments: []transport.OutboundAttachment{{Filename: "quote.pdf", Content: []byte("pdf-bytes")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderMessageID != "<mg-1>" {
		t.Fatalf("id %q", res.ProviderMessageID)
	}
	if _, params, err := mime.ParseMediaType(contentType); err != nil || params["boundary"] == "" {
		t.Fatalf("content type %q", contentType)
	}
	if fields["from"] != "Hermes <hermes@example.com>" || fields["subject"] != "Hi" || fields["text"] != "body" {
		t.Fatalf("fields %#v", fields)
	}
	if files["attachment"] != "pdf-bytes" {
		t.Fatalf("files %#v", files)
	}
}

func readMultipart(t *testing.T, r *http.Request) (map[string]string, map[string]string) {
	t.Helper()
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for key, values := range r.MultipartForm.Value {
		fields[key] = values[0]
	}
	files := map[string]string{}
	for key, headers := range r.MultipartForm.File {
		for _, header := range headers {
			f, err := header.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(f)
			f.Close()
			files[key] = string(data)
		}
	}
	return fields, files
}
