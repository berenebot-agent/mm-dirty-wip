package mailparse_test

import (
	"reflect"
	"testing"

	"github.com/dellarb/mammothmail/internal/mailparse"
)

// TestParseReadsBcc proves a stored draft's Bcc header is parsed, so editing it
// does not lose its hidden recipients.
func TestParseReadsBcc(t *testing.T) {
	t.Parallel()
	raw := "From: a@example.test\r\nTo: b@example.test\r\nBcc: Hidden <C@Example.test>, d@example.test\r\nSubject: s\r\n\r\nbody\r\n"
	p, err := mailparse.ParseBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c@example.test", "d@example.test"}; !reflect.DeepEqual(p.BCC, want) {
		t.Fatalf("BCC = %v, want %v", p.BCC, want)
	}
}
