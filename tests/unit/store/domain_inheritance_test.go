package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// A subdomain created under a configured parent inherits the parent's receiving
// configuration and resolves inbound bindings without its own credential, while
// still resolving to its own domain id.
func TestSubdomainInheritsReceiving(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", "enc-parent", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ParentDomainID != d.ID || sub.ParentDomain != "example.com" || !sub.InheritReceiving || !sub.InheritSending {
		t.Fatalf("subdomain defaults %+v", sub)
	}
	// No own config row exists for the subdomain.
	if _, err := s.GetDomainReceivingConfig(ctx, u.AccountID, sub.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("subdomain own config err=%v, want ErrNoProvider", err)
	}
	b, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != u.AccountID || b.DomainID != sub.ID {
		t.Fatalf("binding must use the subdomain's own domain id: %+v", b)
	}
	if b.Provider != "cloudflare" || b.EncryptedConfig != "enc-parent" {
		t.Fatalf("binding must inherit the parent credential: %+v", b)
	}
	// The effective provider is surfaced for display and names the parent.
	got, err := s.GetDomain(ctx, u.AccountID, sub.ID)
	if err != nil || got.ReceivingProvider != "cloudflare" || got.ReceivingInheritedFrom != "example.com" {
		t.Fatalf("effective receiving %+v err=%v", got, err)
	}
	// A different provider is not satisfied by the inherited config.
	if _, err := s.ResolveInboundBinding(ctx, "mailgun", "foo@agent.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong provider err=%v, want not found", err)
	}
}

// An own configuration always wins over the inherited one, and deleting it
// falls back to the parent again.
func TestSubdomainOwnConfigOverridesInherited(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", "enc-parent", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, sub.ID, "mailgun", "enc-own", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	b, err := s.ResolveInboundBinding(ctx, "mailgun", "foo@agent.example.com")
	if err != nil || b.EncryptedConfig != "enc-own" {
		t.Fatalf("own mailgun binding %+v err=%v", b, err)
	}
	// The subdomain is pinned to its own provider: the parent's cloudflare
	// config must not leak through.
	if _, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("own provider must not fall through to parent: err=%v", err)
	}
	if err := s.DeleteDomainReceivingConfig(ctx, u.AccountID, sub.ID); err != nil {
		t.Fatal(err)
	}
	if b, err = s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.example.com"); err != nil || b.EncryptedConfig != "enc-parent" {
		t.Fatalf("after delete should inherit parent again: %+v err=%v", b, err)
	}
}

// A subdomain with inheritance disabled and no own config does not resolve.
func TestSubdomainInheritanceDisabledRejects(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", "enc-parent", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateDomainWithOptions(ctx, u.AccountID, "agent.example.com", store.DomainCreateOptions{DisableReceiving: true})
	if err != nil {
		t.Fatal(err)
	}
	if sub.InheritReceiving {
		t.Fatalf("receiving inheritance should be off: %+v", sub)
	}
	if _, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve with inheritance off err=%v, want not found", err)
	}
	// Turning it back on restores inheritance without a new config row.
	if err := s.SetDomainInheritance(ctx, u.AccountID, sub.ID, true, sub.InheritSending); err != nil {
		t.Fatal(err)
	}
	if b, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.example.com"); err != nil || b.EncryptedConfig != "enc-parent" {
		t.Fatalf("after re-enable %+v err=%v", b, err)
	}
}

// A deep subdomain inherits from the nearest configured ancestor, and a
// subdomain never inherits from another account's domain of the same name.
func TestSubdomainNestingAndAccountIsolation(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", "enc-root", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	mid, err := s.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	deep, err := s.CreateDomain(ctx, u.AccountID, "eu.agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if deep.ParentDomainID != mid.ID {
		t.Fatalf("deep parent must be the nearest ancestor: %+v", deep)
	}
	if b, err := s.ResolveInboundBinding(ctx, "cloudflare", "x@eu.agent.example.com"); err != nil || b.EncryptedConfig != "enc-root" || b.DomainID != deep.ID {
		t.Fatalf("deep inherit %+v err=%v", b, err)
	}

	// A second account owning a parent cannot lend its config to the first
	// account's subdomain: a same-named domain is globally unique anyway, so
	// isolation is proven by the second account's own subdomain resolving only
	// against its own parent.
	u2, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	bd, err := s.CreateDomain(ctx, u2.AccountID, "other.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDomainReceivingConfig(ctx, u2.AccountID, bd.ID, "resend", "enc-b", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	bsub, err := s.CreateDomain(ctx, u2.AccountID, "sub.other.test")
	if err != nil {
		t.Fatal(err)
	}
	if bsub.ParentDomainID != bd.ID {
		t.Fatalf("account B subdomain parent %+v", bsub)
	}
	if b, err := s.ResolveInboundBinding(ctx, "resend", "x@sub.other.test"); err != nil || b.AccountID != u2.AccountID || b.EncryptedConfig != "enc-b" {
		t.Fatalf("account B inherit %+v err=%v", b, err)
	}
	if _, err := s.ResolveInboundBinding(ctx, "resend", "x@eu.agent.example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account A must not see account B's provider: err=%v", err)
	}
}

// Sending configuration inherits the same way.
func TestSubdomainInheritsSending(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "enc-send-parent", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.ResolveDomainSendingConfig(ctx, u.AccountID, sub.ID)
	if err != nil || cfg.Provider != "brevo" || cfg.EncryptedConfig != "enc-send-parent" {
		t.Fatalf("inherited sending %+v err=%v", cfg, err)
	}
	got, err := s.GetDomain(ctx, u.AccountID, sub.ID)
	if err != nil || got.SendingProvider != "brevo" || got.SendingInheritedFrom != "example.com" {
		t.Fatalf("effective sending display %+v err=%v", got, err)
	}
}

// The parent-name override must name a genuine ancestor in the same account.
func TestCreateSubdomainParentOverrideRejectsNonAncestor(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	if _, err := s.CreateDomainWithOptions(ctx, u.AccountID, "nope.test", store.DomainCreateOptions{ParentDomainID: d.ID}); err == nil {
		t.Fatal("expected error when parent is not an ancestor")
	}
}

// A subdomain added before its parent can be linked to the parent afterwards,
// inheriting its connector while keeping the child domain id. Unlinking clears
// the switch and stops resolution.
func TestLinkSubdomainToParentAddedLater(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	// Subdomain first: no ancestor exists, so it is created as a root domain.
	sub, err := s.CreateDomain(ctx, u.AccountID, "agent.late.test")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ParentDomainID != "" || sub.InheritReceiving || sub.InheritSending {
		t.Fatalf("subdomain added first must be a root: %+v", sub)
	}
	// The ancestor is added later and configured.
	parent, err := s.CreateDomain(ctx, u.AccountID, "late.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, parent.ID, "cloudflare", "enc-late", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	// It is offered as a candidate parent.
	cands, err := s.InheritableAncestors(ctx, u.AccountID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if c.ID == parent.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("parent must be offered as a candidate: %+v", cands)
	}
	// Link and inherit.
	if err := s.SetDomainParent(ctx, u.AccountID, sub.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.late.test")
	if err != nil || b.EncryptedConfig != "enc-late" || b.DomainID != sub.ID {
		t.Fatalf("linked subdomain must inherit while keeping its own id: %+v err=%v", b, err)
	}
	got, err := s.GetDomain(ctx, u.AccountID, sub.ID)
	if err != nil || got.ParentDomain != "late.test" || !got.InheritReceiving || got.ReceivingInheritedFrom != "late.test" {
		t.Fatalf("linked domain view %+v err=%v", got, err)
	}
	// Unlink: resolution stops and the switches clear.
	if err := s.SetDomainParent(ctx, u.AccountID, sub.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetDomain(ctx, u.AccountID, sub.ID)
	if err != nil || got.ParentDomainID != "" || got.InheritReceiving || got.InheritSending {
		t.Fatalf("unlinked domain view %+v err=%v", got, err)
	}
	if _, err := s.ResolveInboundBinding(ctx, "cloudflare", "foo@agent.late.test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unlinked subdomain must not resolve: err=%v", err)
	}
}

// Linking rejects a parent that is not an ancestor, the domain itself, or a
// descendant that would form a cycle.
func TestLinkSubdomainRejectsInvalidParents(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	sub, err := s.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainParent(ctx, u.AccountID, sub.ID, d.ID); err != nil {
		t.Fatalf("linking to the real ancestor should succeed: %v", err)
	}
	// The parent is not an ancestor of a root domain.
	root, err := s.CreateDomain(ctx, u.AccountID, "other.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainParent(ctx, u.AccountID, root.ID, d.ID); err == nil {
		t.Fatal("expected rejection when parent is not an ancestor")
	}
	// A domain cannot be its own parent.
	if err := s.SetDomainParent(ctx, u.AccountID, sub.ID, sub.ID); err == nil {
		t.Fatal("expected rejection when linking a domain to itself")
	}
	// A descendant of the domain cannot become its parent (cycle).
	if err := s.SetDomainParent(ctx, u.AccountID, d.ID, sub.ID); err == nil {
		t.Fatal("expected rejection when linking to a descendant")
	}
	// A parent in another account is rejected.
	u2, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	bd, err := s.CreateDomain(ctx, u2.AccountID, "elsewhere.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDomainParent(ctx, u2.AccountID, sub.ID, bd.ID); err == nil {
		t.Fatal("expected rejection for cross-account parent")
	}
}
