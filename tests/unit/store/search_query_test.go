package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// A NUL byte in a search query must not reach the FTS engine. Before the fix it
// built a malformed MATCH expression that the driver rejected, surfacing as an
// unmapped store error and an HTTP 500 (retest finding B). It must now be a
// typed client-input error, and a control byte that is merely stripped must
// still search the remaining terms.
func TestSearchRejectsControlBytesInQuery(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	// Seed one searchable message so a "no matches" result is distinguishable
	// from an empty mailbox.
	if _, _, _, err := s.CommitInbound(ctx, inbound(b[0], "d1", "rfc1", "", nil, "generator price", "the generator price is listed")); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	// A raw NUL is refused as client input, not as an engine fault.
	if _, err := s.SearchMessages(ctx, p, "gen\x00erator", b[0].ID, 20); !errors.Is(err, store.ErrInvalidSearchQuery) {
		t.Fatalf("NUL in query: want ErrInvalidSearchQuery, got %v", err)
	}
	// The same applies on the filtered path (what /v1/search actually calls).
	if _, err := s.SearchMessagesFiltered(ctx, p, "gen\x00erator", store.MessageFilter{InboxID: b[0].ID, Limit: 20}); !errors.Is(err, store.ErrInvalidSearchQuery) {
		t.Fatalf("NUL in filtered query: want ErrInvalidSearchQuery, got %v", err)
	}

	// Other control characters are stripped rather than fatal, and the query
	// still matches on its remaining terms (no whole-request rejection).
	got, err := s.SearchMessages(ctx, p, "generator\x01 price", b[0].ID, 20)
	if err != nil {
		t.Fatalf("strip of a non-NUL control byte must not fail: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("control-byte query lost its match: got %d results", len(got))
	}

	// A query that is nothing but control bytes degrades to "no matches"
	// rather than an error, so a hostile query cannot self-inflict a 500.
	got, err = s.SearchMessages(ctx, p, "\x01\x02", b[0].ID, 20)
	if err != nil {
		t.Fatalf("all-control query must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("all-control query should match nothing, got %d", len(got))
	}

	// An ordinary query is unaffected.
	got, err = s.SearchMessages(ctx, p, "generator price", b[0].ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("normal search regressed: got %d results", len(got))
	}
}

// FTS operators must stay inert: quoting each term is what prevents a query
// from injecting syntax, and that behaviour must survive the control-byte fix.
func TestSearchQuotesFTSOperators(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	if _, _, _, err := s.CommitInbound(ctx, inbound(b[0], "d1", "rfc1", "", nil, "hello", "hello world")); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	// A bare operator-looking query must be treated as a literal term, not
	// syntax: it should return no matches and no error.
	for _, q := range []string{"NOT hello", "hello OR world", `"hello"`, "hello*"} {
		if _, err := s.SearchMessages(ctx, p, q, b[0].ID, 20); err != nil {
			t.Fatalf("query %q should be inert, got error %v", q, err)
		}
	}
	// A quote is escaped rather than terminating the term.
	if _, err := s.SearchMessages(ctx, p, strings.Repeat(`"`, 4), b[0].ID, 20); err != nil {
		t.Fatalf("quoted input must not error: %v", err)
	}
}
