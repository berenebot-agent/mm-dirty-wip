package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestStandaloneWorkflowDeliveryStartedSucceeds proves that recording an
// in-flight workflow handoff for a standalone inbox succeeds.
//
// A standalone inbox has no managed domain, so its inboxes.domain_id is NULL.
// The delivery-log attribution query used to scan that NULL into a plain string,
// which failed every workflow delivery for a standalone inbox — the approval
// request or RemoteDraft handoff notification was claimed and then failed before
// the provider was ever called, logging
//
//	sql: Scan error on column index 0, name "domain_id": converting NULL to string is unsupported
//
// The attempt must instead be recorded with a NULL domain, exactly like a
// standalone message attempt. This is the exact call the outbox worker makes
// immediately before the provider send.
func TestStandaloneWorkflowDeliveryStartedSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)

	inbox, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "agent@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	// A standalone inbox is created without a domain; assert the precondition the
	// regression depends on rather than assuming it.
	if inbox.DomainID != "" {
		t.Fatalf("standalone inbox carries domain_id %q, want empty", inbox.DomainID)
	}

	wf, _, err := s.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox:     inbox,
		Kind:      model.WorkflowKindApprovalRequest,
		From:      model.Address{Address: inbox.Address},
		To:        []string{inbox.Address},
		Subject:   "Approval request",
		Text:      "x",
		RawPath:   "workflow/n.eml",
		SizeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID != "" {
		t.Fatalf("workflow unexpectedly produced a send request %q", wf.ID)
	}
	// CommitWorkflow returns the send request, not the job id; claim the queued
	// job the way the outbox worker does.
	jobID, _, err := s.ClaimNextWorkflow(ctx, time.Now().UTC(), "owner", time.Minute)
	if err != nil || jobID == "" {
		t.Fatalf("claim = %q err=%v", jobID, err)
	}
	if err := s.RecordWorkflowDeliveryStarted(ctx, u.AccountID, inbox.ID, jobID, "smtp"); err != nil {
		t.Fatalf("RecordWorkflowDeliveryStarted on standalone inbox: %v", err)
	}

	// The job is still pending and unattributed: recording the in-flight attempt
	// must not have failed the job nor attributed it to a non-existent domain.
	got, err := s.GetWorkflowInternal(ctx, u.AccountID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.WorkflowPending {
		t.Fatalf("workflow status = %q, want pending", got.Status)
	}
	// A failed attributing insert would have surfaced here instead.
	if got.Attempts != 0 {
		t.Fatalf("workflow attempts = %d, want 0", got.Attempts)
	}
}

// TestDomainWorkflowDeliveryStartedStillAttributed is the regression guard for
// the fix's other direction: a managed-domain inbox's workflow attempt must
// still carry its domain, so the nullable read did not silently drop it.
func TestDomainWorkflowDeliveryStartedStillAttributed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]

	wf, _, err := s.CommitWorkflow(ctx, store.WorkflowRecord{
		Inbox:     box,
		Kind:      model.WorkflowKindApprovalRequest,
		From:      model.Address{Address: box.Address},
		To:        []string{"approver@outside.test"},
		Subject:   "Approval required",
		Text:      "x",
		RawPath:   "workflow/test.eml",
		SizeBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWorkflowDeliveryStarted(ctx, u.AccountID, box.ID, wf.ID, "smtp"); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("domain attempts = %d, want 1: %+v", len(attempts), attempts)
	}
	if attempts[0].Status != "sending" {
		t.Fatalf("attempt status = %q, want sending", attempts[0].Status)
	}
	if attempts[0].DomainID != d.ID {
		t.Fatalf("attempt domain = %q, want %q", attempts[0].DomainID, d.ID)
	}
}

// TestGetFolderDestinationResolvesIDOrPath proves a move destination is resolved
// whether it is given as a folder id or as a folder path. The unified Labels API
// sends a folder id (`folder:<id>`), while the bulk-move form and API clients
// send a path; both must reach the same folder, because the move path previously
// only matched on path and silently failed a label-driven move with a
// not-found error.
func TestGetFolderDestinationResolvesIDOrPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	folder, err := s.CreateFolder(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, store.FolderCreate{Path: "Projects/2026"})
	if err != nil {
		t.Fatal(err)
	}
	byID, err := s.GetFolderDestination(ctx, u.AccountID, box.ID, folder.ID)
	if err != nil {
		t.Fatalf("resolve by id: %v", err)
	}
	if byID.ID != folder.ID {
		t.Fatalf("by id resolved %q, want %q", byID.ID, folder.ID)
	}
	byPath, err := s.GetFolderDestination(ctx, u.AccountID, box.ID, folder.Path)
	if err != nil {
		t.Fatalf("resolve by path: %v", err)
	}
	if byPath.ID != folder.ID {
		t.Fatalf("by path resolved %q, want %q", byPath.ID, folder.ID)
	}
	// An unknown destination is still a not-found, never a silent success.
	if _, err := s.GetFolderDestination(ctx, u.AccountID, box.ID, "does/not/exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown destination err = %v, want ErrNotFound", err)
	}
}
