package store_test

import (
	"context"
	"github.com/dellarb/mammothmail/internal/model"
	"testing"

	"github.com/dellarb/mammothmail/internal/store"
)

// TestManagedAliasesSurviveExternalAliasRemoval proves the removal migration
// leaves managed-domain aliases (inbox_aliases) and ordinary mail intact, since
// only the external sending-alias feature is removed.
func TestManagedAliasesSurviveExternalAliasRemoval(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetInboxAliases(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales", DisplayName: "Sales"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "sales@example.com" {
		t.Fatalf("managed aliases not preserved: %+v", got.Aliases)
	}
}
