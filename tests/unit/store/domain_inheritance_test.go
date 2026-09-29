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
