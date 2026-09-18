package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestFinalMIMESizeGuardRejectsExpansion proves the size limit is enforced on
// the built message, not just the pre-encoded attachment payload.
func TestFinalMIMESizeGuardRejectsExpansion(t *testing.T) {
	svc, u, _, box := testService(t)
	// A body just under the cap expands past it once MIME headers are added.
	svc.Config.MaxMessageBytes = 256
	ctx := context.Background()
	owner := ownerPrincipal(u.AccountID, box.ID)
	_, err := svc.Send(ctx, owner, app.SendInput{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: strings.Repeat("a", 300)}, "")
	if err == nil {
		t.Fatal("oversize final MIME was accepted")
	}
	if !strings.Contains(err.Error(), "maximum message size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWorkflowMailDoesNotConsumeQuotaOrCreateThread proves approval mail is not
// mailbox content: it creates no thread and never touches account storage.
func TestWorkflowMailDoesNotConsumeQuotaOrCreateThread(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	// Measure after the draft exists so only the workflow enqueue can change it.
	before, err := svc.Store.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Store.GetAccount(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StorageUsedBytes != before.StorageUsedBytes {
		t.Fatalf("workflow mail changed storage: before=%d after=%d", before.StorageUsedBytes, after.StorageUsedBytes)
	}
	threads, err := svc.Store.ListThreads(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 0 {
		t.Fatalf("workflow mail created threads: %+v", threads)
	}
}

// TestUnroutedAuditCoalescesPerDomain proves random local parts on the same
// receiving domain cannot each produce an audit row.
func TestUnroutedAuditCoalescesPerDomain(t *testing.T) {
	svc, u, dom, _ := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	ctx := context.Background()
	// Three different unknown local parts on the same domain.
	for i, rcpt := range []string{"a1@example.com", "a2@example.com", "a3@example.com"} {
		raw := "From: sender@outside.test\r\nTo: " + rcpt + "\r\nSubject: S\r\nMessage-ID: <u" + string(rune('0'+i)) + "@test>\r\n\r\nbody"
		req := mgRequest(t, testMailgunKey, "unrouted-"+rcpt, rcpt, raw)
		_, _, err := svc.IngestInbound(ctx, "mailgun", req)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ingest %s err=%v", rcpt, err)
		}
	}
	count, err := svc.Store.CountAuditKind(ctx, "mailgun.unrouted")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unrouted audit rows = %d, want 1 (coalesced per domain)", count)
	}
}
