package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestExternalAliasSendUsesAliasConnector proves an external alias send resolves
// that alias's own connector and never the inbox domain's.
func TestExternalAliasSendUsesAliasConnector(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	// The inbox domain has a connector, but the external alias does not yet.
	var domainCalls atomic.Int32
	domAPI := providerServer(t, "<domain-out>", &domainCalls)
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": domAPI.URL})

	a, err := svc.CreateExternalAlias(ctx, p, box.ID, "agent@gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxDefaultSender(ctx, u.AccountID, box.ID, a.Address); err != nil {
		t.Fatal(err)
	}

	// A send from the external default is held: the alias has no connector even
	// though the inbox domain does.
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, FromAddress: a.Address, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "ext-key")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.From.Address != "agent@gmail.com" {
		t.Fatalf("from %+v", res.Message.From)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	held, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil || held.Status != "pending" || held.Attempts != 0 {
		t.Fatalf("held message %+v err=%v", held, err)
	}
	if domainCalls.Load() != 0 {
		t.Fatal("external send fell back to the domain connector")
	}

	// Saving the alias's connector requeues and delivers only via that connector.
	var aliasCalls atomic.Int32
	aliasAPI := providerServer(t, "<alias-out>", &aliasCalls)
	if _, err := svc.SaveExternalAliasSendingConfig(ctx, p, box.ID, a.ID, "brevo", map[string]any{"api_key": "k", "api_base": aliasAPI.URL}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil || sent.Status != "sent" || aliasCalls.Load() != 1 || domainCalls.Load() != 0 {
		t.Fatalf("sent %+v alias=%d domain=%d err=%v", sent, aliasCalls.Load(), domainCalls.Load(), err)
	}
}

// TestExternalAliasDeletedMessageFailsPermanently proves a queued send whose
// alias was deleted fails immediately and never retries on a domain connector.
func TestExternalAliasDeletedMessageFailsPermanently(t *testing.T) {
	svc, u, d, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	// A working domain connector exists, to prove there is no silent fallback.
	var calls atomic.Int32
	api := providerServer(t, "<domain-out>", &calls)
	seedSending(t, svc, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	a, err := svc.CreateExternalAlias(ctx, p, box.ID, "agent@gmail.com", "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, FromAddress: a.Address, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"}, "del-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteExternalAlias(ctx, p, box.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	// Deliver returns the terminal error for the worker to log, but the message
	// must already be durably failed.
	if err := svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); !errors.Is(err, store.ErrExternalAliasDeleted) {
		t.Fatalf("deliver err=%v, want ErrExternalAliasDeleted", err)
	}
	failed, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil || failed.Status != "failed" || calls.Load() != 0 {
		t.Fatalf("failed %+v domainCalls=%d err=%v", failed, calls.Load(), err)
	}
}

// TestExternalAliasDraftFreezesIDThroughApproval proves an approved draft keeps
// its external alias even after the alias is deleted and recreated, and fails
// closed if the frozen alias is gone.
func TestExternalAliasDraftFreezesIDThroughApproval(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	a, err := svc.CreateExternalAlias(ctx, p, box.ID, "agent@gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	draft, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, FromAddress: a.Address, To: []string{"friend@example.net"}, Subject: "Hi", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if draft.FromExternalAliasID != a.ID {
		t.Fatalf("draft did not freeze external alias id: %+v", draft)
	}
	if _, err := svc.RequestSend(ctx, p, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	// Delete the original and create a new alias with the same address. The
	// frozen draft must still point at the original (now missing) alias.
	if err := svc.DeleteExternalAlias(ctx, p, box.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateExternalAlias(ctx, p, box.ID, "agent@gmail.com", "Agent Again"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApproveDraft(ctx, p, draft.ID, "", model.DecisionMethodUI, "")
	if !errors.Is(err, store.ErrExternalAliasDeleted) {
		t.Fatalf("approval after alias delete err=%v, want ErrExternalAliasDeleted", err)
	}
}

// TestExternalAliasAdminGate proves external aliases are Admin-only and, in
// hosted mode, unavailable even to an Admin.
func TestExternalAliasAdminGate(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	nonAdmin := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "owner"}}

	if err := svc.ExternalAliasAdmin(nonAdmin); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("non-admin gate err=%v", err)
	}
	if err := svc.ExternalAliasAdmin(admin); err != nil {
		t.Fatalf("admin gate err=%v", err)
	}
	if _, err := svc.CreateExternalAlias(ctx, nonAdmin, box.ID, "x@gmail.com", ""); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("non-admin create err=%v", err)
	}

	svc.Config.Mode = "hosted"
	if err := svc.ExternalAliasAdmin(admin); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("hosted admin gate err=%v", err)
	}
}
