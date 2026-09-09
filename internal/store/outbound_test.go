package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
)

func TestActiveOutboundCredential(t *testing.T) {
	s, u, _, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.ActiveOutboundCredential(ctx, u.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	a, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "A", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "B", "smtp", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveOutboundCredential(ctx, u.AccountID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("active %v %q", err, got.ID)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ActiveOutboundCredential(ctx, u.AccountID); got.ID != b.ID {
		t.Fatalf("switch active %q", got.ID)
	}
	if err = s.DeleteOutboundCredential(ctx, u.AccountID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActiveOutboundCredential(ctx, u.AccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete should clear active, got %v", err)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, "out_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown credential should be rejected, got %v", err)
	}
}

func TestDomainOutboundCredentialResolution(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)

	if _, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}

	a, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "A", "brevo", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetActiveOutboundCredential(ctx, u.AccountID, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); err != nil || got.ID != a.ID {
		t.Fatalf("account fallback: %v %q", err, got.ID)
	}

	b, err := s.SaveOutboundCredential(ctx, u.AccountID, "", "B", "smtp", "enc")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); err != nil || got.ID != b.ID {
		t.Fatalf("domain override: %v %q", err, got.ID)
	}
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.OutboundCredentialID != b.ID {
		t.Fatalf("get domain: %v %+v", err, dom)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, "out_missing"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown credential should be rejected, got %v", err)
	}
	if err = s.SetDomainOutboundCredential(ctx, u.AccountID, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainOutboundCredential(ctx, u.AccountID, d.ID); err != nil || got.ID != a.ID {
		t.Fatalf("cleared fallback: %v %q", err, got.ID)
	}

	// Deleting a credential clears the domain assignment via the foreign key.
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
	msg, _, err := s.CommitOutbound(ctx, OutboundRecord{Inbox: boxes[0], Provider: a.Provider, RFCMessageID: "<m@test>", From: model.Address{Address: boxes[0].Address}, To: []string{"x@outside.test"}, Subject: "hi", Text: "body", RawPath: "messages/x.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OutboundCredentialForMessage(ctx, u.AccountID, msg.ID); err != nil || got.ID != a.ID {
		t.Fatalf("for message: %v %q", err, got.ID)
	}

	// HoldPending defers without counting an attempt.
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
	if err = s.HoldPending(ctx, u.AccountID, "msg_missing", "x", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hold unknown: %v", err)
	}
}

func TestMigrationReopenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}
