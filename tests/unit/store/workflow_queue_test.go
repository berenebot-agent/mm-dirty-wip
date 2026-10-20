package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestWorkflowQueueLifecycleAndRedaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	w, ev, err := s.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox:     box,
		Kind:      model.WorkflowKindApprovalRequest,
		From:      model.Address{Address: box.Address},
		To:        []string{"approver@outside.test"},
		Subject:   "Approval required",
		Text:      "Reference: [GH-REQUEST:abcdefghijklmnop]",
		HTML:      "<p>[GH-APPROVE:abcdefghijklmnop]</p>",
		RawPath:   "workflow/test.eml",
		SizeBytes: 10,
		NewSendRequest: &store.SendRequestInsert{
			ID: "dsr_wf", DraftID: "drf_wf", InboxID: box.ID,
			RequestedAt: time.Now().UTC(), ApproverEmail: "approver@outside.test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != model.EventDraftSendRequested {
		t.Fatalf("event = %+v", ev)
	}
	if w.NotificationStatus != model.NotificationQueued || w.ApprovalWorkflowID == "" {
		t.Fatalf("request not queued: %+v", w)
	}
	if w.TokenExpiresAt != nil {
		t.Fatalf("expiry started before handoff: %v", w.TokenExpiresAt)
	}

	// Claim and mark sent, which starts the expiry clock.
	id, claim, err := s.ClaimNextWorkflow(ctx, time.Now().UTC(), "owner", time.Minute)
	if err != nil || id == "" {
		t.Fatalf("claim = %q err=%v", id, err)
	}
	events, err := s.MarkWorkflowSent(ctx, u.AccountID, id, "<provider@x>", "smtp", 48*time.Hour, claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != model.EventDraftNotificationSent {
		t.Fatalf("sent events = %+v", events)
	}
	sr, err := s.GetSendRequestInternal(ctx, u.AccountID, "dsr_wf")
	if err != nil {
		t.Fatal(err)
	}
	if sr.NotificationStatus != model.NotificationSent || sr.TokenExpiresAt == nil {
		t.Fatalf("after send: notification=%q expiry=%v", sr.NotificationStatus, sr.TokenExpiresAt)
	}

	// Terminal jobs are listed for redaction; redact then sweep after retention.
	jobs, err := s.TerminalUnredactedWorkflows(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("unredacted = %+v err=%v", jobs, err)
	}
	if err := s.UpdateWorkflowBodies(ctx, u.AccountID, id, "Reference: [GH-REQUEST:REDACTED]", "[GH-APPROVE:REDACTED]"); err != nil {
		t.Fatal(err)
	}
	if jobs, _ = s.TerminalUnredactedWorkflows(ctx); len(jobs) != 0 {
		t.Fatalf("still unredacted: %+v", jobs)
	}
	// A cutoff after the terminal time models the retention window elapsing.
	paths, err := s.SweepWorkflows(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "workflow/test.eml" {
		t.Fatalf("sweep paths = %v", paths)
	}
	if _, err = s.GetWorkflowInternal(ctx, u.AccountID, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow not swept: %v", err)
	}
}

func TestWorkflowTerminalFailureNotifiesRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	if _, _, err := s.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox: box, Kind: model.WorkflowKindApprovalRequest,
		From: model.Address{Address: box.Address}, To: []string{"approver@outside.test"},
		Subject: "Approval required", Text: "x", RawPath: "workflow/f.eml", SizeBytes: 1,
		NewSendRequest: &store.SendRequestInsert{
			ID: "dsr_f", DraftID: "drf_f", InboxID: box.ID,
			RequestedAt: time.Now().UTC(), ApproverEmail: "approver@outside.test",
		},
	}); err != nil {
		t.Fatal(err)
	}
	id, claim, err := s.ClaimNextWorkflow(ctx, time.Now().UTC(), "owner", time.Minute)
	if err != nil || id == "" {
		t.Fatalf("claim = %q err=%v", id, err)
	}
	events, err := s.MarkWorkflowFailed(ctx, u.AccountID, id, "permanent rejection", time.Time{}, 1, "smtp", claim)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != model.EventDraftNotificationFailed {
		t.Fatalf("failed events = %+v", events)
	}
	sr, err := s.GetSendRequestInternal(ctx, u.AccountID, "dsr_f")
	if err != nil || sr.NotificationStatus != model.NotificationFailed {
		t.Fatalf("request after failure: %+v err=%v", sr, err)
	}
}

func TestRecoverStaleIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	// A reservation with no message, older than the lease, is reclaimed.
	claimed, _, err := s.IdempotencyReserve(ctx, u.AccountID, "stale-key", box.ID)
	if err != nil || !claimed {
		t.Fatalf("reserve = %v err=%v", claimed, err)
	}
	n, err := s.RecoverStaleIdempotency(ctx, time.Now().UTC().Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("recover = %d err=%v", n, err)
	}
	claimed, _, err = s.IdempotencyReserve(ctx, u.AccountID, "stale-key", box.ID)
	if err != nil || !claimed {
		t.Fatalf("reserve after recovery = %v err=%v", claimed, err)
	}

	// A reservation that completed keeps replaying and is never reclaimed.
	if _, _, err = s.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "smtp", From: model.Address{Address: box.Address},
		To: []string{"x@y.test"}, Subject: "s", Text: "b", RawPath: "messages/m.eml", IdemKey: "done-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverStaleIdempotency(ctx, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	claimed, mid, err := s.IdempotencyReserve(ctx, u.AccountID, "done-key", box.ID)
	if err != nil || claimed || mid == "" {
		t.Fatalf("done key reclaimed: claimed=%v mid=%q err=%v", claimed, mid, err)
	}
}

// TestStaleWorkflowClaimCannotRecordOutcome is the workflow-queue counterpart
// of the outbox claim-token guard: a job whose send outlived its claim lease
// cannot have its outcome recorded over a live re-claim, for either outcome.
func TestStaleWorkflowClaimCannotRecordOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	if _, _, err := s.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox: box, Kind: model.WorkflowKindApprovalRequest,
		From: model.Address{Address: box.Address}, To: []string{"approver@outside.test"},
		Subject: "Approval required", Text: "x", RawPath: "workflow/f.eml", SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 claims with a one-minute lease; the send runs longer than that.
	id, claim1, err := s.ClaimNextWorkflow(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil || id == "" {
		t.Fatalf("claim = %q err=%v", id, err)
	}
	// Attempt 2 re-claims after the lease lapsed.
	_, claim2, err := s.ClaimNextWorkflow(ctx, time.Now().UTC().Add(2*time.Minute), "w2", time.Hour)
	if err != nil || claim2 == "" {
		t.Fatalf("re-claim = %q err=%v", claim2, err)
	}
	// Attempt 1's outcomes must both be rejected, and attempt 2's live claim is
	// untouched.
	if _, err = s.MarkWorkflowSent(ctx, u.AccountID, id, "<prov>", "smtp", time.Hour, claim1); !errors.Is(err, store.ErrClaimTaken) {
		t.Fatalf("stale sent = %v, want ErrClaimTaken", err)
	}
	if _, err = s.MarkWorkflowFailed(ctx, u.AccountID, id, "down", time.Now().UTC().Add(time.Hour), 6, "smtp", claim1); !errors.Is(err, store.ErrClaimTaken) {
		t.Fatalf("stale failed = %v, want ErrClaimTaken", err)
	}
	w, err := s.GetWorkflowInternal(ctx, u.AccountID, id)
	if err != nil || w.ClaimOwner != claim2 {
		t.Fatalf("after stale attempts: claim=%q err=%v", w.ClaimOwner, err)
	}
	if _, err = s.MarkWorkflowFailed(ctx, u.AccountID, id, "down", time.Now().UTC().Add(time.Hour), 6, "smtp", claim2); err != nil {
		t.Fatalf("live attempt's MarkWorkflowFailed = %v", err)
	}
}
