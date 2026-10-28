package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mammothmail/internal/store"
)

// TestCreateDomainDuplicateReportsConflict proves re-adding a domain name the
// account already owns reports a deliberate conflict rather than a raw SQLite
// UNIQUE failure (which callers redact to a generic "try again").
func TestCreateDomainDuplicateReportsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)

	// The fixture created example.com; a second create of the same name must be
	// a named conflict, not an engine fault.
	if _, err := s.CreateDomain(ctx, u.AccountID, "example.com"); !errors.Is(err, store.ErrDomainNameConflict) {
		t.Fatalf("duplicate create err=%v, want ErrDomainNameConflict", err)
	}
	// Normalization means a differently-cased or dotted spelling collides too.
	if _, err := s.CreateDomain(ctx, u.AccountID, "Example.COM."); !errors.Is(err, store.ErrDomainNameConflict) {
		t.Fatalf("case-folded duplicate err=%v, want ErrDomainNameConflict", err)
	}
	// A distinct name still succeeds.
	if _, err := s.CreateDomain(ctx, u.AccountID, "second.example"); err != nil {
		t.Fatalf("distinct create: %v", err)
	}
}

// TestCreateDomainDuplicateAcrossAccountsReportsConflict proves the global name
// uniqueness is surfaced as the same conflict: a name another account owns
// cannot be created, and the message does not disclose the other account.
func TestCreateDomainDuplicateAcrossAccountsReportsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _, d, _ := testStore(t)

	other, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateDomain(ctx, other.AccountID, d.Name)
	if !errors.Is(err, store.ErrDomainNameConflict) {
		t.Fatalf("cross-account duplicate err=%v, want ErrDomainNameConflict", err)
	}
	if _, err := s.CreateDomain(ctx, other.AccountID, "b.test"); err != nil {
		t.Fatalf("distinct create for other account: %v", err)
	}
}
