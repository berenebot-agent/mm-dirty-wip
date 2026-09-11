package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSenderRestrictionAndApproverAlwaysAllowed covers the three cases that
// matter: an unrestricted inbox accepts any sender (even with an approver set),
// a restricted inbox blocks non-listed senders, and the configured approver is
// always accepted.
func TestSenderRestrictionAndApproverAlwaysAllowed(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	ctx := context.Background()

	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, "appr@approver.test"); err != nil {
		t.Fatal(err)
	}

	ingest := func(n int, from string) bool {
		raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: hi\r\nMessage-ID: <m%d@test>\r\nDate: %s\r\n\r\nbody", from, box.Address, n, time.Now().Format(time.RFC1123Z))
		m, _, err := svc.IngestInbound(ctx, "mailgun", mgRequest(t, testMailgunKey, fmt.Sprintf("d-%d", n), box.Address, raw))
		if err != nil {
			t.Fatalf("ingest %d: %v", n, err)
		}
		return m.Blocked
	}

	// Unrestricted: setting an approver must not restrict general receiving.
	if blocked := ingest(1, "stranger@outside.test"); blocked {
		t.Fatal("approver setting restricted general receiving")
	}

	// Restrict to one address.
	if err := svc.Store.SetInboxAllowedSenders(ctx, u.AccountID, box.ID, []string{"friend@outside.test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxSenderRestricted(ctx, u.AccountID, box.ID, true); err != nil {
		t.Fatal(err)
	}
	if blocked := ingest(2, "stranger@outside.test"); !blocked {
		t.Fatal("restricted inbox accepted a non-listed sender")
	}
	if blocked := ingest(3, "friend@outside.test"); blocked {
		t.Fatal("restricted inbox blocked a listed sender")
	}
	if blocked := ingest(4, "appr@approver.test"); blocked {
		t.Fatal("restricted inbox blocked the approver")
	}
}
