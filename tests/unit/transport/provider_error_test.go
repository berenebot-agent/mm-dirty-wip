package transport_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
)

// TestProviderErrorTruncatesAndStripsBody proves a provider error keeps only a
// short, single-line snippet: a large body cannot be persisted to the delivery
// log in full, and control characters (including embedded newlines) are removed.
func TestProviderErrorTruncatesAndStripsBody(t *testing.T) {
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
