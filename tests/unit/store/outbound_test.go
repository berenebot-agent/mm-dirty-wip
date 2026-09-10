package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestDomainSendingConfigAndHold(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)

	// A domain with no sending config reports ErrNoProvider, not ErrNotFound.
	if _, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("expected store.ErrNoProvider, got %v", err)
	}
	// A missing/foreign domain reports ErrNotFound.
	if _, err := s.GetDomainSendingConfig(ctx, u.AccountID, "dom_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound, got %v", err)
	}

	cfg, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "Brevo", "enc", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID == "" || cfg.Provider != "brevo" || cfg.Revision != 1 || cfg.DomainID != d.ID {
		t.Fatalf("created config %+v", cfg)
	}
	// Creating again without a version is a conflict.
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", "enc2", store.ConfigVersion{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate create err=%v, want conflict", err)
	}
	// The domain summary hydrates the sending provider.
	dom, err := s.GetDomain(ctx, u.AccountID, d.ID)
	if err != nil || dom.SendingProvider != "brevo" || dom.ReceivingProvider != "" {
		t.Fatalf("domain summary %+v err=%v", dom, err)
	}

	// Resolution via an existing message uses the message's inbox domain.
	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: boxes[0], Provider: cfg.Provider, RFCMessageID: "<m@test>", From: model.Address{Address: boxes[0].Address}, To: []string{"x@outside.test"}, Subject: "hi", Text: "body", RawPath: "messages/x.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	byMsg, err := s.DomainSendingConfigForMessage(ctx, u.AccountID, msg.ID)
	if err != nil || byMsg.ID != cfg.ID {
		t.Fatalf("for message: %v %q", err, byMsg.ID)
	}
	if _, err := s.DomainSendingConfigForMessage(ctx, u.AccountID, "msg_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("for missing message err=%v, want not found", err)
	}

	// store.HoldPending defers without counting an attempt.
	if err = s.HoldPending(ctx, u.AccountID, msg.ID, "no sending provider configured", time.Now().Add(5*time.Minute)); err != nil {
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
