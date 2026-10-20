package app_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// errInjectedOutcome models an infrastructure fault (disk full, SQLite busy) that
// makes the outcome write itself fail after the provider already accepted the
// send.
var errInjectedOutcome = errors.New("injected outcome write failure")

// The fault tests below accept a provider send in full and then fail the store's
// outcome write, so the message must be held for review rather than resent.
//
// They run serially (no t.Parallel): the injected fault hook is process-global,
// so concurrent tests must not share it.
func TestAcceptedSendOutcomeWriteFailureNeverResends(t *testing.T) {
	svc, u, dom, box := testServiceConcurrency(t, 1)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<accepted-out>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "accepted", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := res.Message.ID

	// The provider accepts, but MarkSent's write fails.
	store.SetOutboundWriteFaultForTest("MarkSent", id, errInjectedOutcome)
	t.Cleanup(func() { store.SetOutboundWriteFaultForTest("", "", nil) })

	// Simulate the worker: claim, then deliver with that claim.
	_, claim, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil || claim == "" {
		t.Fatalf("claim = %q err=%v", claim, err)
	}
	if err = svc.Deliver(ctx, u.AccountID, id, claim); err == nil {
		t.Fatal("expected the outcome write to fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times, want 1", calls.Load())
	}

	// The uncertain handoff is now visible as a failed Outbox row, with its
	// original pre-send marker retained to prevent automatic duplicate delivery.
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "failed" || !store.IsUncertainDelivery(m.LastError) {
		t.Fatalf("status = %q error = %q, want delivery uncertain", m.Status, m.LastError)
	}
	if inFlight, _ := svc.Store.MessageSendInFlight(ctx, u.AccountID, id); !inFlight {
		t.Fatal("no pre-send marker after a failed outcome write")
	}
	if err = svc.Store.RecoverAbandonedClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC().Add(72*time.Hour), "w2", time.Hour); err != nil || again != "" {
		t.Fatalf("quarantined message became claimable: %q err=%v", again, err)
	}
	// It is surfaced through the ordinary failed Outbox.
	rows, err := svc.Store.ListOutbox(ctx, p, box.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("outbox = %+v err=%v, want uncertain message", rows, err)
	}
	if err = svc.Store.RequeueFailed(ctx, p, id); !errors.Is(err, store.ErrUncertainDelivery) {
		t.Fatalf("retry without duplicate-risk confirmation = %v", err)
	}
	// No automatic resend: the provider is still called exactly once.
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times after quarantine, want 1", calls.Load())
	}
}

// TestSafeKnownFailureStillRetries proves a *known* provider failure (the
// provider rejected the request, so nothing was accepted) still records its
// outcome and retries normally — the marker is resolved, not held.
func TestSafeKnownFailureStillRetries(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"message":"upstream error"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "safe", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err == nil {
		t.Fatal("expected a delivery error")
	}
	m, err := svc.Store.GetMessageByID(ctx, u.AccountID, res.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending" || m.Attempts != 1 || m.NextRetry == "" {
		t.Fatalf("known failure not retried: %+v", m)
	}
	if inFlight, _ := svc.Store.MessageSendInFlight(ctx, u.AccountID, res.Message.ID); inFlight {
		t.Fatal("marker not resolved after a definitive failure")
	}
	// The retry is actually claimable again.
	if id, _, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC().Add(48*time.Hour), "w1", time.Hour); err != nil || id != res.Message.ID {
		t.Fatalf("rescheduled message not claimable: %q err=%v", id, err)
	}
}

// TestWorkflowAcceptedSendOutcomeWriteFailureNeverResends is the workflow-queue
// counterpart: a handoff the provider accepted but whose outcome write failed is
// held for review, never resent.
func TestWorkflowAcceptedSendOutcomeWriteFailureNeverResends(t *testing.T) {
	svc, u, dom, box := testServiceConcurrency(t, 1)
	ctx := context.Background()
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<wf-accepted>"}`)
	}))
	defer api.Close()
	seedSending(t, svc, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL})

	// An Assistant requests approval, queueing the notification workfow job.
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	w, err := svc.Store.LatestWorkflowForInbox(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}

	store.SetOutboundWriteFaultForTest("MarkWorkflowSent", w.ID, errInjectedOutcome)
	t.Cleanup(func() { store.SetOutboundWriteFaultForTest("", "", nil) })

	_, claim, err := svc.Store.ClaimNextWorkflow(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil || claim == "" {
		t.Fatalf("claim = %q err=%v", claim, err)
	}
	if err = svc.DeliverWorkflow(ctx, u.AccountID, w.ID, claim); err == nil {
		t.Fatal("expected the outcome write to fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times, want 1", calls.Load())
	}
	if inFlight, _ := svc.Store.WorkflowSendInFlight(ctx, u.AccountID, w.ID); !inFlight {
		t.Fatal("workflow marker missing after a failed outcome write")
	}
	if err = svc.Store.RecoverAbandonedWorkflowClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if id, _, err := svc.Store.ClaimNextWorkflow(ctx, time.Now().UTC().Add(72*time.Hour), "w2", time.Hour); err != nil || id != "" {
		t.Fatalf("quarantined workflow became claimable: %q err=%v", id, err)
	}
	// The approval-notification state distinguishes ambiguity from rejection.
	job, err := svc.Store.GetWorkflowInternal(ctx, u.AccountID, w.ID)
	if err != nil || job.Status != model.WorkflowFailed || !store.IsUncertainDelivery(job.LastError) {
		t.Fatalf("uncertain workflow = %+v err=%v", job, err)
	}
	principal := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	request, err := svc.Store.GetSendRequestByDraft(ctx, principal, d.ID)
	if err != nil || request.NotificationStatus != model.NotificationUncertain {
		t.Fatalf("notification status = %+v err=%v", request, err)
	}
}
