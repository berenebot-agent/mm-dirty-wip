package app_test

import (
	"context"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

// TestSendAsAliasUsesAliasDomainAndName proves that sending as a cross-domain
// alias resolves the provider from the alias's own domain and writes the
// alias's display name into the stored From header.
func TestSendAsAliasUsesAliasDomainAndName(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	d2, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d2.ID, LocalPart: "sales", DisplayName: "Acme Sales"}}); err != nil {
		t.Fatal(err)
	}

	// Configuring the alias domain lets the cross-domain send deliver.
	var calls atomic.Int32
	api := providerServer(t, "<alias-out>", &calls)
	seedSending(t, svc, u.AccountID, d2.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	_ = d

	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, FromAddress: "sales@other.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "alias-key")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.From.Address != "sales@other.com" || res.Message.From.Name != "Acme Sales" {
		t.Fatalf("from %+v", res.Message.From)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Status != "sent" || calls.Load() != 1 {
		t.Fatalf("status %q calls %d", sent.Status, calls.Load())
	}
}

// TestSendRejectsUnknownAliasSender proves a sender that is neither the primary
// nor one of the inbox's aliases is rejected before anything is enqueued.
func TestSendRejectsUnknownAliasSender(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	if _, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, FromAddress: "nope@example.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, ""); err == nil {
		t.Fatal("unknown sender was accepted")
	}
}

// TestDraftFromFrozenInApproval verifies the draft's sender and display name are
// frozen into the approval fingerprint: changing the alias name after the
// request is made blocks the approval.
func TestDraftFromFrozenInApproval(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	if err := svc.Store.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales", DisplayName: "Acme Sales"}}); err != nil {
		t.Fatal(err)
	}
	draft, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, FromAddress: "sales@example.com", To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if draft.FromAddress != "sales@example.com" || draft.FromName != "Acme Sales" {
		t.Fatalf("draft from %q name %q", draft.FromAddress, draft.FromName)
	}
	if _, err := svc.RequestSend(ctx, p, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	// Renaming the alias changes the resolved name, so the frozen fingerprint no
	// longer matches and the approval is refused.
	if err := svc.Store.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: d.ID, LocalPart: "sales", DisplayName: "Other Name"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveDraft(ctx, p, draft.ID, "", model.DecisionMethodUI, ""); err == nil {
		t.Fatal("approval accepted after frozen sender name changed")
	}
}
