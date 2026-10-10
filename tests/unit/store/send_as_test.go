package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestResolveSendingTarget covers primary, same-domain alias, cross-domain alias
// (which selects the alias's own domain and name) and rejection of anything
// that is neither the primary nor an alias. It also proves the default sender
// is validated and cleared when its alias is removed.
func TestResolveSendingTarget(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	box := boxes[0]

	d2, err := s.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{
		{DomainID: d.ID, LocalPart: "sales", DisplayName: "Acme Sales"},
		{DomainID: d2.ID, LocalPart: "billing"}, // no name -> falls back to inbox name
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxDisplayName(ctx, u.AccountID, box.ID, "Acme"); err != nil {
		t.Fatal(err)
	}

	// Empty and primary (in any case) both resolve to the primary with the
	// inbox name/domain.
	for _, requested := range []string{"", box.Address, strings.ToUpper(box.Address)} {
		from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, requested)
		if err != nil || from.Address != box.Address || from.Name != "Acme" || target.DomainID != d.ID {
			t.Fatalf("primary %q -> %+v target=%+v err=%v", requested, from, target, err)
		}
	}

	// Same-domain alias uses its own display name and the inbox domain.
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "sales@example.com")
	if err != nil || from.Address != "sales@example.com" || from.Name != "Acme Sales" || target.DomainID != d.ID {
		t.Fatalf("same-domain alias -> %+v target=%+v err=%v", from, target, err)
	}

	// Cross-domain alias resolves its own domain and falls back to the inbox
	// display name when it has none.
	from, target, err = s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "billing@other.com")
	if err != nil || from.Address != "billing@other.com" || from.Name != "Acme" || target.DomainID != d2.ID {
		t.Fatalf("cross-domain alias -> %+v target=%+v err=%v", from, target, err)
	}

	// An address that is neither primary nor alias is rejected.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "nope@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("unknown sender err=%v, want forbidden", err)
	}
	// A foreign inbox is not found.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, "in_missing", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing inbox err=%v, want not found", err)
	}

	// Default sender must be primary or an alias.
	if err := s.SetInboxDefaultSender(ctx, u.AccountID, box.ID, "sales@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID); err != nil || got.DefaultSender != "sales@example.com" {
		t.Fatalf("default sender %q err=%v", got.DefaultSender, err)
	}
	if err := s.SetInboxDefaultSender(ctx, u.AccountID, box.ID, "nope@example.com"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("invalid default sender err=%v, want forbidden", err)
	}

	// Replacing the alias set without the default's alias clears it.
	if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d2.ID, LocalPart: "billing"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetInboxInternal(ctx, u.AccountID, box.ID); err != nil || got.DefaultSender != "" {
		t.Fatalf("default sender after alias removal %q err=%v", got.DefaultSender, err)
	}
}

// TestSendAsMessageRecordsSendingDomain proves a send-as-alias stamps the
// message with the alias's own domain, and that delivery/requeue resolve that
// domain's config rather than the inbox's.
func TestSendAsMessageRecordsSendingDomain(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]

	d2, err := s.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d2.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "sales@other.com")
	if err != nil {
		t.Fatal(err)
	}
	if target.DomainID != d2.ID {
		t.Fatalf("sending domain = %s, want %s", target.DomainID, d2.ID)
	}
	sendingDomainID := target.DomainID

	// Neither domain has a config yet.
	msg, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<m@test>",
		From: from, SendingDomainID: sendingDomainID, To: []string{"x@outside.test"}, Subject: "s", Text: "t",
		RawPath: "messages/x.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DomainSendingConfigForMessage(ctx, u.AccountID, msg.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("config for message err=%v, want no provider", err)
	}

	// Configuring the alias domain (not the inbox domain) makes it resolvable.
	cfg, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d2.ID, "brevo", "enc", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.DomainSendingConfigForMessage(ctx, u.AccountID, msg.ID); err != nil || got.ID != cfg.ID {
		t.Fatalf("config after alias-domain save = %q err=%v", got.ID, err)
	}
	// The outbound delivery log attributes the attempt to the alias domain.
	if _, _, err := s.MarkSent(ctx, u.AccountID, msg.ID, "<prov>", "brevo"); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d2.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].FromAddress != "sales@other.com" {
		t.Fatalf("alias-domain attempts %#v", attempts)
	}
}

// TestAliasDisplayNameValidation rejects names that would corrupt the From
// header or the parallel-field encoding.
func TestAliasDisplayNameValidation(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	box := boxes[0]
	for _, bad := range []string{"a,b", "line\nbreak", "car\rriage", "ctrl\x01"} {
		if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "x", DisplayName: bad}}); err == nil {
			t.Fatalf("alias display name %q accepted", bad)
		}
	}
	if err := s.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "x", DisplayName: "  Acme Sales  "}}); err != nil {
		t.Fatal(err)
	}
	box, err := s.GetInboxInternal(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if box.AliasNames["x@example.com"] != "Acme Sales" {
		t.Fatalf("alias names %#v", box.AliasNames)
	}
}

// TestRequeuePendingForSendingDomain proves saving an alias domain's config
// requeues that alias domain's pending sends.
func TestRequeuePendingForSendingDomain(t *testing.T) {
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	d2, err := s.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	// A pending send stamped to d2 (an alias send).
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<a@test>",
		From: model.Address{Address: "sales@other.com"}, SendingDomainID: d2.ID, To: []string{"x@outside.test"},
		Subject: "s", Text: "t", RawPath: "messages/a.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "down", time.Now().UTC().Add(time.Hour), 6, "brevo"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RequeuePendingForDomain(ctx, u.AccountID, d2.ID); err != nil || n != 1 {
		t.Fatalf("requeue alias domain n=%d err=%v", n, err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || got.Attempts != 0 || got.Status != "pending" {
		t.Fatalf("requeued message %+v err=%v", got, err)
	}
}
