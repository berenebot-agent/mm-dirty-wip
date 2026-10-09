// These tests live in package resend rather than under tests/unit because
// canonicalRecipients is unexported and is a pure normalisation function; the
// black-box tests/unit convention cannot reach it, and building a signed Svix
// webhook for every boundary case would obscure what is under test. This
// mirrors the documented in-package exception for cmd/server/mxruntime_test.go.
package resend

import (
	"fmt"
	"testing"
)

func TestCanonicalRecipientsDedupesAndCaps(t *testing.T) {
	// Duplicates and blanks are removed, order preserved.
	in := []string{"A@Example.com", "b@example.com", "", "A@example.com", "  c@example.com  "}
	got := canonicalRecipients(in)
	if len(got) != 3 || got[0] != "a@example.com" || got[1] != "b@example.com" || got[2] != "c@example.com" {
		t.Fatalf("dedupe/normalise = %#v", got)
	}
	// The count is capped even when every address is unique.
	many := make([]string, 0, maxRecipients*2)
	for i := 0; i < maxRecipients*2; i++ {
		many = append(many, fmt.Sprintf("u%d@example.com", i))
	}
	if capped := canonicalRecipients(many); len(capped) != maxRecipients {
		t.Fatalf("cap = %d, want %d", len(capped), maxRecipients)
	}
}
