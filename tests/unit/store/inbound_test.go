package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
)

func TestResolveInboundBindingAndIsolation(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	if _, err := s.ResolveInboundBinding(ctx, "mailgun", "x@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured resolve err=%v", err)
	}
	cfg, err := s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "Mailgun", "enc", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "mailgun" || cfg.Revision != 1 {
		t.Fatalf("receiving config %+v", cfg)
	}
	b, err := s.ResolveInboundBinding(ctx, "mailgun", "Hermes@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != u.AccountID || b.DomainID != d.ID || b.CredentialID != cfg.ID || b.Recipient != "hermes@example.com" {
		t.Fatalf("binding %+v", b)
	}
	if _, err = s.ResolveInboundBinding(ctx, "cloudflare", "hermes@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong provider err=%v", err)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.ReceivingProvider != "mailgun" || dom.SendingProvider != "" {
		t.Fatalf("domain summary %+v err=%v", dom, err)
	}

	// A foreign account cannot configure or read another account's domain.
	u2, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDomain(ctx, u2.AccountID, "b.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveDomainReceivingConfig(ctx, u2.AccountID, d.ID, "resend", "enc", store.ConfigVersion{ID: cfg.ID, Revision: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account save err=%v, want not found", err)
	}
	if _, err = s.GetDomainReceivingConfig(ctx, u2.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account get err=%v, want not found", err)
	}
	if err = s.DeleteDomainReceivingConfig(ctx, u2.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account delete err=%v, want not found", err)
	}
	if err = s.DeleteDomainReceivingConfig(ctx, u2.AccountID, d2.ID); err != nil {
		t.Fatalf("delete on existing domain without config should be idempotent: %v", err)
	}

	// Deleting the config leaves the domain unconfigured.
	if err = s.DeleteDomainReceivingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveInboundBinding(ctx, "mailgun", "hermes@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cleared resolve err=%v", err)
	}
}

func TestInboundPersistsTransportEnvelopeMetadata(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)
	box := b[0]

	// The transport-supplied envelope sender and the canonical original
	// envelope recipient are persisted verbatim and read back on the message.
	rec := inbound(box, "env-1", "<env@test>", "", nil, "s", "body")
	rec.EnvelopeFrom = "Sender@Outside.Test"
	rec.EnvelopeRecipient = "catchall@example.com"
	m, _, dup, err := s.CommitInbound(ctx, rec)
	if err != nil || dup {
		t.Fatalf("commit %v dup=%v", err, dup)
	}
	if m.EnvelopeFrom != "Sender@Outside.Test" || m.EnvelopeRecipient != "catchall@example.com" {
		t.Fatalf("envelope metadata %#v", m)
	}
	got, err := s.GetMessageByID(ctx, rec.Inbox.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvelopeFrom != "Sender@Outside.Test" || got.EnvelopeRecipient != "catchall@example.com" {
		t.Fatalf("read back %#v", got)
	}

	// A transport that supplied no sender leaves the column empty; nothing is
	// inferred from the MIME From header.
	empty := inbound(box, "env-2", "<env2@test>", "", nil, "s2", "body2")
	empty.EnvelopeFrom = ""
	m2, _, _, err := s.CommitInbound(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	if m2.EnvelopeFrom != "" {
		t.Fatalf("missing sender was backfilled: %q", m2.EnvelopeFrom)
	}
}

func TestWebhookSkippedDeliveryAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	c, err := s.CreateWebhookClient(ctx, u.AccountID, b[0].ID, "ep", "https://hooks.example.test/x", "notify", "signature", "enc")
	if err != nil {
		t.Fatal(err)
	}
	_, e1, _, err := s.CommitInbound(ctx, inbound(b[0], "sk-1", "<sk1@test>", "", nil, "one", "body"))
	if err != nil {
		t.Fatal(err)
	}
	_, e2, _, err := s.CommitInbound(ctx, inbound(b[0], "sk-2", "<sk2@test>", "", nil, "two", "body"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if got, err := s.NextWebhookDelivery(ctx, now); err != nil || got.EventID != e1.ID {
		t.Fatalf("head %v %#v", err, got)
	}
	// A skipped event is terminal and the cursor advances, so the next event is
	// the new head without any network call.
	if err := s.RecordWebhookSkipped(ctx, c.ID, e1.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.NextWebhookDelivery(ctx, now)
	if err != nil || next.EventID != e2.ID {
		t.Fatalf("after skip %v %#v", err, next)
	}
	if next.Client.LastAckEventID != e1.ID {
		t.Fatalf("cursor %d, want %d", next.Client.LastAckEventID, e1.ID)
	}
	// Skipping again is idempotent and a skipped event is never re-picked.
	if err := s.RecordWebhookSkipped(ctx, c.ID, e1.ID); err != nil {
		t.Fatal(err)
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
