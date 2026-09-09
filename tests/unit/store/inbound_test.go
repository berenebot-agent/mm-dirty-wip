package store_test

import (
	"context"
	"errors"
	"testing"

	"gatehouse-mail/internal/store"
)

func TestInboundCredentialCRUDAndBinding(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	if _, err := s.ResolveInboundBinding(ctx, "mailgun", "x@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured resolve err=%v", err)
	}
	cred, err := s.SaveInboundCredential(ctx, u.AccountID, "", "MG", "mailgun", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetInboundCredential(ctx, u.AccountID, cred.ID); err != nil || got.Name != "MG" {
		t.Fatalf("get %+v err=%v", got, err)
	}
	if list, err := s.ListInboundCredentials(ctx, u.AccountID); err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.ResolveInboundBinding(ctx, "mailgun", "Hermes@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != u.AccountID || b.DomainID != d.ID || b.CredentialID != cred.ID || b.Recipient != "hermes@example.com" {
		t.Fatalf("binding %+v", b)
	}
	if _, err = s.ResolveInboundBinding(ctx, "cloudflare", "hermes@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong provider err=%v", err)
	}

	// Cross-account assignment is forbidden.
	u2, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDomain(ctx, u2.AccountID, "b.test")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainInboundCredential(ctx, u2.AccountID, d2.ID, cred.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-account assign err=%v", err)
	}

	// Clearing leaves the domain unconfigured.
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveInboundBinding(ctx, "mailgun", "hermes@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleared resolve err=%v", err)
	}
	// Deleting a credential leaves assigned domains unconfigured.
	if err = s.SetDomainInboundCredential(ctx, u.AccountID, d.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteInboundCredential(ctx, u.AccountID, cred.ID); err != nil {
		t.Fatal(err)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.InboundCredentialID != "" {
		t.Fatalf("domain after delete %+v err=%v", dom, err)
	}
}

func TestInboundDedupScopedByRecipient(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)
	r1 := inbound(b[0], "same-delivery", "<one@test>", "", nil, "s", "body")
	r1.EnvelopeRecipient = b[0].Address
	r2 := inbound(b[1], "same-delivery", "<two@test>", "", nil, "s", "body")
	r2.EnvelopeRecipient = b[1].Address

	m1, _, dup, err := s.CommitInbound(ctx, r1)
	if err != nil || dup {
		t.Fatalf("first %v dup=%v", err, dup)
	}
	m2, _, dup, err := s.CommitInbound(ctx, r2)
	if err != nil || dup {
		t.Fatalf("same delivery different recipient must not dedup: %v dup=%v", err, dup)
	}
	if m1.ID == m2.ID {
		t.Fatal("distinct deliveries collapsed")
	}
	m3, _, dup, err := s.CommitInbound(ctx, r1)
	if err != nil || !dup || m3.ID != m1.ID {
		t.Fatalf("same recipient retry must dedup: %v dup=%v", err, dup)
	}

	// Blocked messages use the same scoped identity.
	bm1, dup, err := s.CommitBlockedInbound(ctx, store.BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[0].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[0].Address})
	if err != nil || dup {
		t.Fatalf("blocked first %v dup=%v", err, dup)
	}
	_, dup, err = s.CommitBlockedInbound(ctx, store.BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[1].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[1].Address})
	if err != nil || dup {
		t.Fatalf("blocked different recipient %v dup=%v", err, dup)
	}
	bm2, dup, err := s.CommitBlockedInbound(ctx, store.BlockedRecord{AccountID: r1.Inbox.AccountID, InboxID: b[0].ID, Provider: "mailgun", ProviderDeliveryID: "blk-1", EnvelopeRecipient: b[0].Address})
	if err != nil || !dup || bm2.ID != bm1.ID {
		t.Fatalf("blocked retry %v dup=%v", err, dup)
	}
}
