package mailparse_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/mailparse"
)

func TestBoundedMIMEPreview(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"plain", "Content-Type: text/plain; charset=utf-8\r\n\r\nhello\nworld", "hello world"},
		{"encoded", "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ=", "hello world"},
		{"html", "Content-Type: text/html\r\n\r\n<p>hello</p><script>secret()</script>", "hello"},
		{"attachment", "Content-Type: text/plain\r\nContent-Disposition: attachment; filename=secret.txt\r\n\r\nsecret", ""},
		{"truncated multipart", "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nhello world", "hello world"},
		{"invalid", "not MIME", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailparse.PreviewBytes([]byte(tc.raw)); got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
	got := mailparse.PreviewBytes([]byte("Content-Type: text/plain\r\n\r\n" + strings.Repeat("x", 1<<20)))
	if len(got) != 200 {
		t.Fatalf("preview not bounded: %d", len(got))
	}
}
