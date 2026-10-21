package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestQuotaRefusedInboundIsRecorded pins the public-beta UAT finding D-2:
// inbound mail refused because the inbox storage quota is full must leave a
// durable trace in the domain's receiving log, and the refusal must still fail
// the delivery so the sender's retry lands once there is room.
func TestQuotaRefusedInboundIsRecorded(t *testing.T) {
	t.Parallel()
	svc, u, d, box := mxService(t)
	ctx := context.Background()
	owner := ownerPrincipal(u.AccountID, box.ID)

	// Cap the inbox well below the size of the message.
	quota := int64(8)
	if err := svc.Store.SetInboxStorageQuota(ctx, u.AccountID, box.ID, &quota); err != nil {
		t.Fatal(err)
	}

	res, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, res); r.MachineCode != mxwire.CodeQuota {
		t.Fatalf("machine code = %q, want %q", r.MachineCode, mxwire.CodeQuota)
	}

	// A refusal is not a delivery: nothing was stored.
	msgs, err := svc.Store.ListMessages(ctx, owner, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("stored %d messages after a quota refusal, want 0", len(msgs))
	}

	// The refusal is visible to the operator in the domain's receiving log.
	entries, err := svc.Store.ListDomainReceivingLog(ctx, u.AccountID, d.ID, 10, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	refusals := 0
	for _, e := range entries {
		if e.InboxID == box.ID && e.Reason == "storage quota exceeded" {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("receiving log rows for the refusal = %d, want 1 (entries: %+v)", refusals, entries)
	}

	// The refusal wrote no receipt, so the sender's retry is still a delivery.
	if err := svc.Store.SetInboxStorageQuota(ctx, u.AccountID, box.ID, nil); err != nil {
		t.Fatal(err)
	}
	res2, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if r := mxResult(t, res2); r.MachineCode != mxwire.CodeOK {
		t.Fatalf("machine code after the quota was lifted = %q, want %q", r.MachineCode, mxwire.CodeOK)
	}
	msgs, err = svc.Store.ListMessages(ctx, owner, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages after the retry, want 1", len(msgs))
	}
}
