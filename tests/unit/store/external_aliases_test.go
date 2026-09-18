package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestExternalAliasCRUDAndResolver covers creation, address immutability,
// managed-address conflicts, per-inbox listing, and that an external alias
// resolves as a sending target (never as an inbound recipient).
func TestExternalAliasCRUDAndResolver(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	box := boxes[0]

	a, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "Agent@Gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if a.Address != "agent@gmail.com" || a.DisplayName != "Agent" || a.Configured {
		t.Fatalf("created alias %+v", a)
	}

	// It resolves as a sender with the alias's own name.
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "agent@gmail.com")
	if err != nil || from.Address != "agent@gmail.com" || from.Name != "Agent" || target.ExternalAliasID != a.ID || target.DomainID != "" {
		t.Fatalf("resolve -> %+v target=%+v err=%v", from, target, err)
	}

	// It never resolves as an inbound recipient (no managed domain claims it).
	if _, route, err := s.ResolveRecipient(ctx, "agent@gmail.com"); !errors.Is(err, store.ErrNotFound) || route != store.RouteNone {
		t.Fatalf("external alias resolved inbound: route=%v err=%v", route, err)
	}

	// Address is immutable: only the display name is editable.
	updated, err := s.UpdateExternalAlias(ctx, u.AccountID, box.ID, a.ID, "Agent Two")
	if err != nil || updated.DisplayName != "Agent Two" {
		t.Fatalf("update -> %+v err=%v", updated, err)
	}
	if updated.Revision <= a.Revision {
		t.Fatalf("revision did not advance: %d -> %d", a.Revision, updated.Revision)
	}

	// The primary and a managed alias cannot be used as an external address.
	if _, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, box.Address, ""); !errors.Is(err, store.ErrInvalidAlias) {
		t.Fatalf("primary address accepted: %v", err)
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "sales@example.com", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("managed alias address accepted: %v", err)
	}

	// Listing is grouped by inbox and redacts credentials.
	groups, err := s.ListExternalAliases(ctx, u.AccountID)
	if err != nil || len(groups[box.ID]) != 1 {
		t.Fatalf("list groups=%#v err=%v", groups, err)
	}

	// Deleting clears a default sender that referenced it and removes the row.
	if err := s.SetInboxDefaultSender(ctx, u.AccountID, box.ID, "agent@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExternalAlias(ctx, u.AccountID, box.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil || got.DefaultSender != "" {
		t.Fatalf("default sender after delete %q err=%v", got.DefaultSender, err)
	}
	if _, err := s.GetExternalAlias(ctx, u.AccountID, box.ID, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alias survived delete: %v", err)
	}
}

// TestExternalAliasSenderResolutionFreezesID proves a resolved send records the
// alias's immutable id, and that resolving a frozen id fails closed after the
// alias is deleted instead of falling back to a domain connector.
func TestExternalAliasSenderResolutionFreezesID(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]

	a, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "agent@gmail.com", "")
	if err != nil {
		t.Fatal(err)
	}
	// With no display name it falls back to the inbox display name.
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "agent@gmail.com")
	if err != nil || target.ExternalAliasID != a.ID || from.Name != box.DisplayName {
		t.Fatalf("resolve -> %+v target=%+v err=%v", from, target, err)
	}

	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<e@test>",
		From: from, SendingExternalAliasID: target.ExternalAliasID, To: []string{"x@outside.test"},
		Subject: "s", Text: "t", RawPath: "messages/e.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}

	// A connector on the alias resolves for delivery; deleting the alias after
	// enqueue makes the message un-resolvable (never a domain fallback).
	if _, err := s.SaveExternalAliasSendingConfig(ctx, u.AccountID, box.ID, a.ID, "brevo", "enc", store.ConfigVersion{ID: a.ID, Revision: a.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendingConfigForMessage(ctx, u.AccountID, msg.ID); err != nil {
		t.Fatalf("config after alias connector save: %v", err)
	}
	if err := s.DeleteExternalAlias(ctx, u.AccountID, box.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendingConfigForMessage(ctx, u.AccountID, msg.ID); !errors.Is(err, store.ErrExternalAliasDeleted) {
		t.Fatalf("config after alias delete err=%v, want ErrExternalAliasDeleted", err)
	}
	// A recreated alias with the same address must not rebind the frozen id.
	if _, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "agent@gmail.com", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendingConfigForMessage(ctx, u.AccountID, msg.ID); !errors.Is(err, store.ErrExternalAliasDeleted) {
		t.Fatalf("recreated alias rebound the message: %v", err)
	}
}

// TestExternalAliasMigrationReopen proves migration 028's schema survives a
// reopen, preserves the alias and its external attribution, and leaves the
// database with no foreign-key violations.
func TestExternalAliasMigrationReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "agent@gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<r@test>",
		From: model.Address{Address: a.Address}, SendingExternalAliasID: a.ID, To: []string{"x@outside.test"},
		Subject: "s", Text: "t", RawPath: "messages/r.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkSent(ctx, u.AccountID, msg.ID, "<prov>", "brevo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if n := queryInt(t, s2, `SELECT count(*) FROM schema_migrations WHERE version='028'`); n != 1 {
		t.Fatalf("028 marker count = %d, want 1", n)
	}
	// The external-alias attribution is preserved, not re-derived.
	if n := queryInt(t, s2, `SELECT count(*) FROM outbound_delivery_log WHERE external_alias_id=?`, a.ID); n != 1 {
		t.Fatalf("external alias attribution lost on reopen: %d", n)
	}
	attempts, err := s2.ListExternalAliasDeliveryAttempts(ctx, u.AccountID, box.ID, a.ID, 10, 0)
	if err != nil || len(attempts) != 1 || attempts[0].ExternalAliasID != a.ID {
		t.Fatalf("attempts after reopen %#v err=%v", attempts, err)
	}
	db := rawDB(t, s2.Path())
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation after reopen")
	}
}

// TestExternalAliasConnectorLifecycle covers CAS conflict, credential clearing
// on delete, and requeue-on-save scoping to the alias's own pending messages.
func TestExternalAliasConnectorLifecycle(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]

	a, err := s.CreateExternalAlias(ctx, u.AccountID, box.ID, "agent@gmail.com", "")
	if err != nil {
		t.Fatal(err)
	}
	// Hold a message on the alias because it has no connector yet.
	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "", RFCMessageID: "<h@test>",
		From: model.Address{Address: "agent@gmail.com"}, SendingExternalAliasID: a.ID, To: []string{"x@outside.test"},
		Subject: "s", Text: "t", RawPath: "messages/h.eml", SizeBytes: 10, LastError: "no sending connector configured for this external alias"})
	if err != nil {
		t.Fatal(err)
	}

	saved, err := s.SaveExternalAliasSendingConfig(ctx, u.AccountID, box.ID, a.ID, "brevo", "enc1", store.ConfigVersion{ID: a.ID, Revision: a.Revision})
	if err != nil || saved.Provider != "brevo" {
		t.Fatalf("save connector %+v err=%v", saved, err)
	}
	// A stale revision is rejected.
	if _, err := s.SaveExternalAliasSendingConfig(ctx, u.AccountID, box.ID, a.ID, "brevo", "enc2", store.ConfigVersion{ID: a.ID, Revision: a.Revision}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision err=%v, want conflict", err)
	}
	// Requeue affects only this alias's pending messages.
	if n, err := s.RequeuePendingForExternalAlias(ctx, u.AccountID, a.ID); err != nil || n != 1 {
		t.Fatalf("requeue n=%d err=%v", n, err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, msg.ID)
	if err != nil || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("requeued message %+v err=%v", got, err)
	}

	// Removing the connector clears provider/credentials.
	if _, err := s.SaveExternalAliasSendingConfig(ctx, u.AccountID, box.ID, a.ID, "", "", store.ConfigVersion{ID: saved.ID, Revision: saved.Revision}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.GetExternalAlias(ctx, u.AccountID, box.ID, a.ID)
	if err != nil || cleared.Configured || cleared.Provider != "" {
		t.Fatalf("cleared alias %+v err=%v", cleared, err)
	}
}
