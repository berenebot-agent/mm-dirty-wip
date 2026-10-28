package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestSearchMatchesOnlyIndexedTokens pins down the difference between the
// message listing and the full-text search for the same query. The listing
// matches a substring anywhere in the record; search matches whole tokens in the
// FTS index. UAT reported search returning a different set than the listing for
// one shared token; this proves which difference is real so the reader is not
// chasing a ranking bug.
func TestSearchMatchesOnlyIndexedTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}

	// A carries the token in its own subject; B carries it only in its body.
	a, _, _, err := s.CommitInbound(ctx, inbound(box, "gap-a", "rfc-gap-a", "", nil, "Invoice 4821 for October", "see attached"))
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, err := s.CommitInbound(ctx, inbound(box, "gap-b", "rfc-gap-b", "", nil, "Quarterly summary", "the invoice is enclosed"))
	if err != nil {
		t.Fatal(err)
	}

	search := func(q string) []string {
		t.Helper()
		got, err := s.SearchMessagesFiltered(ctx, p, q, store.MessageFilter{InboxID: box.ID, Limit: 20})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		ids := make([]string, 0, len(got))
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		return ids
	}
	list := func(subject string) []string {
		t.Helper()
		got, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Subject: subject, Limit: 20})
		if err != nil {
			t.Fatalf("list %q: %v", subject, err)
		}
		ids := make([]string, 0, len(got))
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		return ids
	}

	// A whole token shared by both messages is found in both places.
	both := search("invoice")
	if len(both) != 2 {
		t.Fatalf("exact token search = %v, want both messages", both)
	}

	// A substring of a token is a listing match but never a search match: FTS
	// indexes whole tokens, and the listing is the one that matches loosely.
	if got := list("482"); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("listing for a token substring = %v, want only %s", got, a.ID)
	}
	if got := search("482"); len(got) != 0 {
		t.Fatalf("search for a token substring = %v, want no matches: search matches whole tokens only", got)
	}
	_ = c
}
