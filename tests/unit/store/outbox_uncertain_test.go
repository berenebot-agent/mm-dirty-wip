package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestUncertainOutboxRequiresExplicitRetryConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	m := queuedOutbound(t, s, box, "<uncertain-operator@test>")
	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "sender", time.Hour)
	if err != nil || claim == "" {
		t.Fatalf("claim %q: %v", claim, err)
	}
	if err := s.BeginMessageSend(ctx, u.AccountID, m.ID, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMessageUncertain(ctx, u.AccountID, m.ID, "provider accepted but database write failed", claim); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || got.Status != "failed" || !store.IsUncertainDelivery(got.LastError) {
		t.Fatalf("unresolved status=%q error=%q read=%v", got.Status, got.LastError, err)
	}
	list, err := s.ListOutbox(ctx, p, box.ID, 20)
	if err != nil || len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("uncertain send must remain in Outbox: %+v, %v", list, err)
	}
	if next, _, err := s.ClaimNextPending(ctx, time.Now().Add(48*time.Hour), "other", time.Hour); err != nil || next != "" {
		t.Fatalf("uncertain send must not be reclaimed: %q %v", next, err)
	}
	if err := s.RequeueFailed(ctx, p, m.ID); !errors.Is(err, store.ErrUncertainDelivery) {
		t.Fatalf("unconfirmed retry = %v; want ErrUncertainDelivery", err)
	}
	if _, err := s.RequeueFailedConfirmed(ctx, p, m.ID, false); !errors.Is(err, store.ErrUncertainDelivery) {
		t.Fatalf("unconfirmed explicit retry = %v", err)
	}
	got, err = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || got.Status != "failed" {
		t.Fatalf("unsafe retry changed status: %+v %v", got, err)
	}
	warning, err := s.RequeueFailedConfirmed(ctx, p, m.ID, true)
	if err != nil || !strings.Contains(warning, "duplicate") {
		t.Fatalf("confirmed retry warning=%q error=%v", warning, err)
	}
	if next, newClaim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "other", time.Hour); err != nil || next != m.ID || newClaim == "" {
		t.Fatalf("confirmed retry not claimable: %q %q %v", next, newClaim, err)
	}
}

func TestReconcileUncertainSendsOnExpiredLeaseAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	m := queuedOutbound(t, s, boxes[0], "<uncertain-recovery@test>")
	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "sender", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginMessageSend(ctx, u.AccountID, m.ID, claim); err != nil {
		t.Fatal(err)
	}
	// The live lease is not abandoned by maintenance.
	if n, w, err := s.ReconcileUncertainSends(ctx, time.Now().UTC(), false); err != nil || n != 0 || w != 0 {
		t.Fatalf("early reconciliation: %d %d %v", n, w, err)
	}
	// An expired handoff is surfaced as failed, but its marker remains.
	if n, w, err := s.ReconcileUncertainSends(ctx, time.Now().Add(2*time.Hour), false); err != nil || n != 1 || w != 0 {
		t.Fatalf("expiry reconciliation: %d %d %v", n, w, err)
	}
	got, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil || got.Status != "failed" || !store.IsUncertainDelivery(got.LastError) {
		t.Fatalf("after expiry: %+v %v", got, err)
	}
	if marker, err := s.MessageSendInFlight(ctx, u.AccountID, m.ID); err != nil || !marker {
		t.Fatalf("pre-send proof lost: %v %v", marker, err)
	}
	if n, w, err := s.ReconcileUncertainSends(ctx, time.Now().UTC(), true); err != nil || n != 0 || w != 0 {
		t.Fatalf("repeated startup reconciliation: %d %d %v", n, w, err)
	}
}

func TestReconcileUncertainApprovalNotifications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, boxes := testStore(t)
	id := queuedWorkflow(t, s, boxes[0], "workflow/uncertain.eml")
	claim := claimSpecificWorkflow(t, s, id)
	if err := s.BeginWorkflowSend(ctx, u.AccountID, id, claim); err != nil {
		t.Fatal(err)
	}
	if n, w, err := s.ReconcileUncertainSends(ctx, time.Now().UTC(), true); err != nil || n != 0 || w != 1 {
		t.Fatalf("workflow reconciliation: %d %d %v", n, w, err)
	}
	job, err := s.GetWorkflowInternal(ctx, u.AccountID, id)
	if err != nil || job.Status != model.WorkflowFailed || !store.IsUncertainDelivery(job.LastError) {
		t.Fatalf("workflow state: %+v %v", job, err)
	}
	if next, _, err := s.ClaimNextWorkflow(ctx, time.Now().Add(48*time.Hour), "other", time.Hour); err != nil || next != "" {
		t.Fatalf("workflow must not resend: %q %v", next, err)
	}
}
