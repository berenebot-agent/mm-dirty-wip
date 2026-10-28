package transport_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport"
)

// TestProviderErrorTruncatesAndStripsBody proves a provider error keeps only a
// short, single-line snippet: a large body cannot be persisted to the delivery
// log in full, and control characters (including embedded newlines) are removed.
func TestProviderErrorTruncatesAndStripsBody(t *testing.T) {
	t.Parallel()
	body := "line1\r\nline2\t" + strings.Repeat("A", 4096) + "\x00secret-after-nul"
	err := transport.ProviderError("mailgun", "500 Internal Server Error", []byte(body))
	msg := err.Error()
	if !strings.HasPrefix(msg, "mailgun returned HTTP 500: ") {
		t.Fatalf("prefix: %q", msg)
	}
	snippet := strings.TrimPrefix(msg, "mailgun returned HTTP 500: ")
	if len(snippet) > 512 {
		t.Fatalf("snippet not truncated: %d bytes", len(snippet))
	}
	if strings.ContainsAny(snippet, "\r\n\t") {
		t.Fatalf("snippet retains control characters: %q", snippet)
	}
	if strings.ContainsRune(snippet, 0) {
		t.Fatalf("snippet retains NUL")
	}
	if strings.Contains(snippet, "line1") || strings.Contains(snippet, "secret") || !strings.Contains(snippet, "temporarily unavailable") {
		t.Fatalf("unsafe or unhelpful diagnostic: %q", snippet)
	}
}

func TestProviderErrorBrevoAttachmentRejection(t *testing.T) {
	t.Parallel()
	const fallback = "check recipients, sender verification and message requirements"
	const attachment = "provider rejected an attachment file extension; check Brevo's supported attachment types"
	for _, tc := range []struct {
		name, provider, status, body, want string
	}{
		{"real Brevo response", "brevo", "400 Bad Request", `{"code":"invalid_parameter","message":"Unsupported file format: md"}`, attachment},
		{"markdown format", "brevo", "400", `{"code":"invalid_parameter","message":"Unsupported file format: markdown"}`, attachment},
		{"json format", "brevo", "400", `{"code":"invalid_parameter","message":"Unsupported file format: json"}`, attachment},
		{"eml format", "brevo", "400", `{"code":"invalid_parameter","message":"Unsupported file format: eml"}`, attachment},
		{"format secret echo", "brevo", "400", `{"code":"invalid_parameter","message":"Unsupported file format: xkeysib-secret private-file.md private-body user@example.com"}`, attachment},
		{"format other provider", "resend", "400", `{"code":"invalid_parameter","message":"Unsupported file format: md"}`, fallback},
		{"format unknown code", "brevo", "400", `{"code":"unauthorized","message":"Unsupported file format: md"}`, fallback},
		{"format server status", "brevo", "500", `{"code":"invalid_parameter","message":"Unsupported file format: md"}`, "provider is temporarily unavailable"},
		{"extension not allowed", "brevo", "400 Bad Request", `{"code":"invalid_parameter","message":"Extension .md is not allowed for attachments"}`, attachment},
		{"invalid extension", "brevo", "400", `{"code":"invalid_parameter","message":"Invalid attachment extension"}`, attachment},
		{"unsupported extension", "brevo", "400", `{"code":"invalid_parameter","message":"Attachment extension is not supported"}`, attachment},
		{"secret echo", "brevo", "400 secret-reason", `{"code":"invalid_parameter","message":"Invalid attachment extension: private-file.md api-key=xkeysib-secret user@example.com","request":{"content":"private body"}}`, attachment},
		{"unknown reason", "brevo", "400", `{"code":"invalid_parameter","message":"private body xkeysib-secret"}`, fallback},
		{"unrelated validation", "brevo", "400", `{"code":"invalid_parameter","message":"valid sender email required"}`, fallback},
		{"unknown code", "brevo", "400", `{"code":"xkeysib-secret","message":"Invalid attachment extension"}`, fallback},
		{"malformed JSON", "brevo", "400", `{"code":"invalid_parameter","message":"Invalid attachment extension"`, fallback},
		{"wrong type", "brevo", "400", `{"code":"invalid_parameter","message":["Invalid attachment extension"]}`, fallback},
		{"plain text", "brevo", "400", "Invalid attachment extension xkeysib-secret", fallback},
		{"oversized JSON", "brevo", "400", `{"code":"invalid_parameter","message":"Invalid attachment extension ` + strings.Repeat("x", 4096) + `"}`, fallback},
		{"other provider", "mailgun", "400", `{"code":"invalid_parameter","message":"Invalid attachment extension"}`, fallback},
		{"other client status", "brevo", "422", `{"code":"invalid_parameter","message":"Invalid attachment extension"}`, fallback},
		{"auth status", "brevo", "401", `{"code":"invalid_parameter","message":"Invalid attachment extension xkeysib-secret"}`, "check the connector credentials and sender permissions"},
		{"server status", "brevo", "500", `{"code":"invalid_parameter","message":"Invalid attachment extension xkeysib-secret"}`, "provider is temporarily unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := transport.ProviderError(tc.provider, tc.status, []byte(tc.body)).Error()
			want := tc.provider + " returned HTTP " + tc.status[:3] + ": " + tc.want
			if got != want {
				t.Fatalf("diagnostic = %q, want %q", got, want)
			}
		})
	}
}
