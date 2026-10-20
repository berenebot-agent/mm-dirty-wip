package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// queuedWorkflow enqueues one pending workflow job for the fixture inbox and
// returns the created job's id (resolved by its raw path).
func queuedWorkflow(t *testing.T, s *store.Store, box model.Inbox, raw string) string {
	t.Helper()
	if _, _, err := s.CommitWorkflow(context.Background(), store.WorkflowRecord{
		Inbox: box, Kind: model.WorkflowKindApprovalRequest,
		From: model.Address{Address: box.Address}, To: []string{"approver@outside.test"},
		Subject: "Approval required", Text: "x", RawPath: raw, SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	w, err := s.LatestWorkflowForInbox(context.Background(), box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.RawPath != raw {
		t.Fatalf("latest workflow raw path = %q, want %q", w.RawPath, raw)
	}
	return w.ID
}

// claimSpecificWorkflow claims due workflow jobs until the one with wantID is
// claimed, holding every other job it picks up so the claim query cannot return
// it again. Releasing a job instead would let the same (oldest) row win every
// iteration, which is what the FIFO order is designed to do.
func claimSpecificWorkflow(t *testing.T, s *store.Store, wantID string) string {
	t.Helper()
	ctx := context.Background()
	held := map[string]string{}
	for i := 0; i < 20; i++ {
		id, claim, err := s.ClaimNextWorkflow(ctx, time.Now().UTC(), "w1", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Fatalf("workflow %s never became claimable", wantID)
		}
		if id == wantID {
			// Release anything else held before handing back the target claim.
			for otherID, otherClaim := range held {
				if err = s.ReleaseWorkflowClaim(ctx, otherID, otherClaim); err != nil {
					t.Fatal(err)
				}
			}
			return claim
		}
		held[id] = claim
	}
	t.Fatalf("could not claim workflow %s", wantID)
	return ""
}

// TestWorkflowPreSendMarkerBlocksResendAcrossRecovery is the workflow-queue
// counterpart of TestPreSendMarkerBlocksResendAcrossRecovery: a job whose
// provider handoff started but never resolved is never re-claimed, including
// across startup recovery.
func TestWorkflowPreSendMarkerBlocksResendAcrossRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	id := queuedWorkflow(t, s, b[0], "workflow/pre.eml")

	claim := claimSpecificWorkflow(t, s, id)
	if err := s.BeginWorkflowSend(ctx, u.AccountID, id, claim); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.ClaimNextWorkflow(ctx, time.Now().UTC().Add(48*time.Hour), "w2", time.Hour); err != nil || got != "" {
		t.Fatalf("claim of an unresolved workflow send: id=%q err=%v, want empty", got, err)
	}
	if err := s.RecoverAbandonedWorkflowClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.ClaimNextWorkflow(ctx, time.Now().UTC().Add(96*time.Hour), "w3", time.Hour); err != nil || got != "" {
		t.Fatalf("claim after recovery: id=%q err=%v, want empty", got, err)
	}
	if owner, _ := s.WorkflowClaimOwner(ctx, u.AccountID, id); owner != claim {
		t.Fatalf("claim owner = %q, want the unresolved attempt's token", owner)
	}
	if inFlight, _ := s.WorkflowSendInFlight(ctx, u.AccountID, id); !inFlight {
		t.Fatal("workflow marker missing")
	}
	// ReleaseWorkflowClaim is a no-op while a marker is present.
	if err := s.ReleaseWorkflowClaim(ctx, id, claim); err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.WorkflowClaimOwner(ctx, u.AccountID, id); owner != claim {
		t.Fatalf("claim released despite an in-flight marker: %q", owner)
	}
	// A definitive outcome resolves it.
	if _, err := s.MarkWorkflowSent(ctx, u.AccountID, id, "<prov>", "smtp", time.Hour, claim); err != nil {
		t.Fatal(err)
	}
	if inFlight, _ := s.WorkflowSendInFlight(ctx, u.AccountID, id); inFlight {
		t.Fatal("marker survived MarkWorkflowSent")
	}
}

// TestRequeuePendingWorkflowForDomainPreservesLiveSend proves a sending-config
// save does not disturb an in-flight workflow handoff while it requeues idle
// pending jobs.
func TestRequeuePendingWorkflowForDomainPreservesLiveSend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]

	idle := queuedWorkflow(t, s, box, "workflow/idle.eml")
	active := queuedWorkflow(t, s, box, "workflow/active.eml")

	claim := claimSpecificWorkflow(t, s, active)
	if err := s.BeginWorkflowSend(ctx, u.AccountID, active, claim); err != nil {
		t.Fatal(err)
	}
	// The domain requeue leaves the in-flight job's claim and marker intact. It
	// does not filter by domain ownership, which is why the requeued count is not
	// asserted here (a workflow job created by CommitWorkflow has no domain).
	if _, err := s.RequeuePendingWorkflowForDomain(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	if inFlight, _ := s.WorkflowSendInFlight(ctx, u.AccountID, active); !inFlight {
		t.Fatal("active workflow marker cleared by domain requeue")
	}
	if owner, _ := s.WorkflowClaimOwner(ctx, u.AccountID, active); owner != claim {
		t.Fatalf("active workflow claim changed to %q", owner)
	}
	// The idle job is claimable again with a fresh budget.
	iw, err := s.GetWorkflowInternal(ctx, u.AccountID, idle)
	if err != nil {
		t.Fatal(err)
	}
	if iw.Attempts != 0 || iw.SendAttemptState != "" {
		t.Fatalf("idle job not requeued: attempts=%d marker=%q", iw.Attempts, iw.SendAttemptState)
	}
}

// TestResolveQuarantinedWorkflowSendDiscard proves the operator discard path
// settles the job terminally and clears its marker.
func TestResolveQuarantinedWorkflowSendDiscard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	id := queuedWorkflow(t, s, b[0], "workflow/q.eml")
	claim := claimSpecificWorkflow(t, s, id)
	if err := s.BeginWorkflowSend(ctx, u.AccountID, id, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveQuarantinedWorkflowSend(ctx, u.AccountID, id, "discarded: unresolved", false); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetWorkflowInternal(ctx, u.AccountID, id)
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != model.WorkflowFailed || w.SendAttemptState != "" {
		t.Fatalf("after discard: status=%q marker=%q", w.Status, w.SendAttemptState)
	}
	// Listed no longer.
	q, err := s.ListQuarantinedWorkflowSends(ctx, u.AccountID, 10)
	if err != nil || len(q) != 0 {
		t.Fatalf("quarantined = %+v err=%v, want none", q, err)
	}
}
