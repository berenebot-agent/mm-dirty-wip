package store_test

import (
	"context"
	"testing"
	"time"

	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func inboundMX(t *testing.T, st *store.Store, box model.Inbox, provider, rcpt, delivery, fingerprint string, spam bool) model.Message {
	t.Helper()
	m, _, _, err := st.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: provider, ProviderDeliveryID: delivery, EnvelopeRecipient: rcpt,
		From: model.Address{Address: "s@outside.test"}, To: []string{box.Address},
		Subject: "mx", Text: "body", RawPath: "messages/x.eml", SizeBytes: 10,
		ReceivedAt: time.Now().UTC(), Spam: spam, SpamReason: "test", DeliveryFingerprint: fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMXReceiptSurvivesDelete verifies the receipt is durable across message
// deletion so a retry still deduplicates.
func TestMXReceiptSurvivesDelete(t *testing.T) {
	st, u, _, boxes := testStore(t)
	ctx := context.Background()
	box := boxes[0]
	fp := "mxfp-v1:abc"
	inboundMX(t, st, box, "mx", box.Address, fp, fp, false)
	rec, err := st.LookupMXReceipt(ctx, u.AccountID, "mx", box.Address, fp)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Disposition != store.DispositionStored {
		t.Fatalf("disposition %s", rec.Disposition)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	msgs, _ := st.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if len(msgs) != 1 {
		t.Fatalf("messages %d", len(msgs))
	}
	if _, _, _, err := st.DeleteMessage(ctx, p, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupMXReceipt(ctx, u.AccountID, "mx", box.Address, fp); err != nil {
		t.Fatalf("receipt should survive delete: %v", err)
	}
}

func TestMXReceiptSweep(t *testing.T) {
	st, u, _, boxes := testStore(t)
	ctx := context.Background()
	box := boxes[0]
	fp := "mxfp-v1:expire"
	inboundMX(t, st, box, "mx", box.Address, fp, fp, false)
	if n, err := st.SweepMXReceipts(ctx, time.Now().UTC().Add(8*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v", n, err)
	}
	if _, err := st.LookupMXReceipt(ctx, u.AccountID, "mx", box.Address, fp); err == nil {
		t.Fatal("expired receipt should be gone")
	}
}

// TestSpamVisibility verifies Spam is excluded from normal lists and unread
// counts, and included only in the explicit Spam view.
func TestSpamVisibility(t *testing.T) {
	st, u, _, boxes := testStore(t)
	ctx := context.Background()
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	inboundMX(t, st, box, "mx", box.Address, "fp1", "fp1", false)
	inboundMX(t, st, box, "mx", box.Address, "fp2", "fp2", true)
	normal, _ := st.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if len(normal) != 1 {
		t.Fatalf("normal list %d, want 1", len(normal))
	}
	all, _ := st.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, IncludeSpam: true})
	if len(all) != 2 {
		t.Fatalf("include-spam list %d, want 2", len(all))
	}
	spam, _ := st.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, SpamOnly: true})
	if len(spam) != 1 || !spam[0].Spam {
		t.Fatalf("spam list %+v", spam)
	}
	unread, _ := st.UnreadCounts(ctx, p)
	if unread[box.ID] != 1 {
		t.Fatalf("unread count should exclude spam: %d", unread[box.ID])
	}
}
