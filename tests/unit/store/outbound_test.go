package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestDomainOutboundCredentialResolution(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)

	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("expected store.ErrNoProvider, got %v", err)
	}

	a, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "A", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	// Saving a credential must not assign it to any domain.
	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("saving a credential must not auto-assign: %v", err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); err != nil || got.ID != a.ID {
		t.Fatalf("domain credential: %v %q", err, got.ID)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != a.ID {
		t.Fatalf("get domain: %v %+v", err, dom)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, "out_missing"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("unknown credential should be rejected, got %v", err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("cleared domain should have no provider: %v", err)
	}

	// Deleting a credential clears the domain assignment via the foreign key.
	b, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "B", "smtp", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if dom, err = s.GetDomain(ctx, u.AccountID, d.ID); err != nil || dom.OutboundCredentialID != "" {
		t.Fatalf("domain credential should clear on delete: %v %+v", err, dom)
	}

	// Resolution via an existing message uses the message's domain.
	c, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "C", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: boxes[0], Provider: c.Provider, RFCMessageID: "<m@test>", From: model.Address{Address: boxes[0].Address}, To: []string{"x@outside.test"}, Subject: "hi", Text: "body", RawPath: "messages/x.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OutboundCredentialForMessage(ctx, u.AccountID, msg.ID); err != nil || got.ID != c.ID {
		t.Fatalf("for message: %v %q", err, got.ID)
	}

	// store.HoldPending defers without counting an attempt.
	if err = s.HoldPending(ctx, u.AccountID, msg.ID, "no outbound provider configured for this domain", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	held, err := s.GetMessageByID(ctx, u.AccountID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "pending" || held.Attempts != 0 || held.LastError == "" {
		t.Fatalf("held message: status=%q attempts=%d err=%q", held.Status, held.Attempts, held.LastError)
	}
	if err = s.HoldPending(ctx, u.AccountID, "msg_missing", "x", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hold unknown: %v", err)
	}
}

func TestMigrationReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}
