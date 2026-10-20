// These tests live in package resend rather than under tests/unit because
// canonicalRecipients is unexported and is a pure normalisation function; the
// black-box tests/unit convention cannot reach it, and building a signed Svix
// webhook for every boundary case would obscure what is under test. This
// mirrors the documented in-package exception for cmd/server/mxruntime_test.go.
package resend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
)

func TestCanonicalRecipientsDedupesAndCaps(t *testing.T) {
	// Duplicates and blanks are removed, order preserved.
	in := []string{"A@Example.com", "b@example.com", "", "A@example.com", "  c@example.com  "}
	got := canonicalRecipients(in)
	if len(got) != 3 || got[0] != "a@example.com" || got[1] != "b@example.com" || got[2] != "c@example.com" {
		t.Fatalf("dedupe/normalise = %#v", got)
	}
	// Normalisation never silently truncates; Receive rejects overflow before lookup.
	many := make([]string, 0, maxRecipients*2)
	for i := 0; i < maxRecipients*2; i++ {
		many = append(many, fmt.Sprintf("u%d@example.com", i))
	}
	if capped := canonicalRecipients(many); len(capped) != len(many) {
		t.Fatalf("recipients were silently dropped: %d", len(capped))
	}
}

type noLookupResolver struct{ calls int }

func (r *noLookupResolver) ResolveInboundBinding(context.Context, string, string) (transport.InboundBinding, error) {
	r.calls++
	return transport.InboundBinding{}, transport.ErrInboundUnauthorized
}

func TestOverflowRejectedBeforeUnauthenticatedLookups(t *testing.T) {
	wh := webhook{Type: eventReceived, Data: webhookData{To: make([]string, maxRecipients+1)}}
	for i := range wh.Data.To {
		wh.Data.To[i] = fmt.Sprintf("u%d@example.com", i)
	}
	body, _ := json.Marshal(wh)
	r := noLookupResolver{}
	_, _, err := (Transport{}).Receive(context.Background(), httptest.NewRequest("POST", "/", strings.NewReader(string(body))), &r, t.TempDir()+"/m.eml", 1<<20)
	if err == nil || !strings.Contains(err.Error(), "too many envelope recipients") || r.calls != 0 {
		t.Fatalf("overflow err=%v lookups=%d", err, r.calls)
	}
}
