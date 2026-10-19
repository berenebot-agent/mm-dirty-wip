package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// microsoftPageSetup creates a Microsoft-connected standalone inbox on an
// isolated account, so the page transaction has its connection row (the metadata
// upsert selects through inbox_microsoft_connections) and its own storage
// counters.
func microsoftPageSetup(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box := standaloneBox(t, s, u.AccountID, "ms-page@outlook.test")
	if err := s.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{
		AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common",
		IdentityID: "identity", Mailbox: box.Address, EncryptedToken: "encrypted",
	}); err != nil {
		t.Fatal(err)
	}
	return s, u.AccountID, box.ID
}

func microsoftCache(provider, subject string, labels ...string) *store.MicrosoftDeltaCache {
	return &store.MicrosoftDeltaCache{
		ProviderID: provider,
		Thread:     "thread-" + provider,
		FolderID:   "inbox-folder",
		Input: store.RemoteMessageInput{
			FolderPath: "microsoft:" + provider, RFCMessageID: "<" + provider + "@graph>",
			FromAddress: "sender@outside.test", Subject: subject,
		},
		Labels: labels,
	}
}

// TestMicrosoftDeltaPageAppliesArrivalsMetadataAndPrune proves one guarded page
// transaction persists the whole effect set — a new arrival with its metadata and
// labels, plus a final-page prune of a superseded-generation row — and advances
// the cursor exactly once.
func TestMicrosoftDeltaPageAppliesArrivalsMetadataAndPrune(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := microsoftPageSetup(t)

	// A row left behind by a superseded sweep: recorded under the pre-resync
	// generation, so the final page's prune is what removes it.
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox-folder", "old-cursor", true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertMicrosoftMessage(ctx, acct, inbox, "OldMsg", "thread-old", "inbox-folder", store.RemoteMessageInput{
		RFCMessageID: "<old@graph>", FromAddress: "sender@outside.test", Subject: "superseded sweep row",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartMicrosoftResync(ctx, inbox, "inbox-folder"); err != nil {
		t.Fatal(err)
	}
	_, _, generation, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil || generation == "" {
		t.Fatalf("resync generation: %q %v", generation, err)
	}

	received, err := time.Parse(time.RFC3339, "2026-10-11T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	page := store.MicrosoftDeltaPage{
		AccountID: acct, InboxID: inbox, FolderID: "inbox-folder", Generation: generation,
		Initialized: true, Cursor: "cursor-new", Prune: true,
		Effects: []store.MicrosoftDeltaEffect{{
			ProviderID: "NewMsg",
			Cache:      microsoftCache("NewMsg", "fresh arrival", "Projects"),
			Detect:     true,
			Arrival: store.RemoteArrivalInput{
				FolderPath: "microsoft:NewMsg", RFCMessageID: "<NewMsg@graph>",
				FromAddress: "sender@outside.test", Subject: "fresh arrival", InternalDate: &received, Source: "microsoft",
			},
		}},
	}
	result, err := s.ApplyMicrosoftDeltaPage(ctx, page)
	if err != nil {
		t.Fatalf("apply page: %v", err)
	}
	if result.Arrived != 1 || result.Pruned != 1 {
		t.Fatalf("page result = %+v, want one arrival and one pruned row", result)
	}

	arrivals, err := s.ListRemoteArrivalsForInbox(ctx, acct, inbox, 10)
	if err != nil || len(arrivals) != 1 || arrivals[0].RFCMessageID != "<NewMsg@graph>" {
		t.Fatalf("arrivals = %+v (%v), want the one new arrival", arrivals, err)
	}
	id, err := s.MicrosoftLocalID(ctx, acct, inbox, "NewMsg")
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.GetRemoteMessage(ctx, acct, inbox, id)
	if err != nil || row.Subject != "fresh arrival" {
		t.Fatalf("cached row = %+v (%v)", row, err)
	}
	labels, err := s.RemoteMessageLabels(ctx, acct, inbox, id)
	if err != nil || len(labels) != 1 || labels[0] != "Projects" {
		t.Fatalf("labels = %v (%v), want Projects", labels, err)
	}
	if _, err := s.MicrosoftLocalID(ctx, acct, inbox, "OldMsg"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("superseded-generation row survived the final-page prune: %v", err)
	}
	cursor, initialized, generation, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil || cursor != "cursor-new" || !initialized {
		t.Fatalf("cursor = %q initialized=%v (%v)", cursor, initialized, err)
	}

	// Replaying the identical page must not re-emit the arrival: the detected
	// mark committed with it.
	replay := page
	replay.Cursor = "cursor-replayed"
	result, err = s.ApplyMicrosoftDeltaPage(ctx, replay)
	if err != nil || result.Arrived != 0 {
		t.Fatalf("replayed page = %+v (%v), want no second arrival", result, err)
	}
	arrivals, _ = s.ListRemoteArrivalsForInbox(ctx, acct, inbox, 10)
	if len(arrivals) != 1 {
		t.Fatalf("replay duplicated the arrival: %+v", arrivals)
	}
}

// TestMicrosoftDeltaPageStaleLeavesNoEffects proves the entire page — not only
// its cursor — is dropped when a resync supersedes the sweep between the
// provider fetch and the write: no metadata, no deletion, no arrival, no detected
// mark and no prune from the old page survive.
func TestMicrosoftDeltaPageStaleLeavesNoEffects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := microsoftPageSetup(t)

	// Establish a cached row the stale page will try to delete, and a second row
	// its final page will try to prune: neither must be touched.
	for _, provider := range []string{"Doomed", "Superseded"} {
		if _, err := s.UpsertMicrosoftMessage(ctx, acct, inbox, provider, "thread-"+provider, "inbox-folder", store.RemoteMessageInput{
			RFCMessageID: "<" + provider + "@graph>", FromAddress: "sender@outside.test", Subject: provider,
		}); err != nil {
			t.Fatal(err)
		}
	}
	doomed, err := s.MicrosoftLocalID(ctx, acct, inbox, "Doomed")
	if err != nil {
		t.Fatal(err)
	}
	// Mark the sweep the page belongs to, then supersede it.
	_, _, generation, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMicrosoftDelta(ctx, inbox, "inbox-folder", "sweep-1", false, generation); err != nil {
		t.Fatal(err)
	}
	if err := s.StartMicrosoftResync(ctx, inbox, "inbox-folder"); err != nil {
		t.Fatal(err)
	}

	receivedAt, err := time.Parse(time.RFC3339, "2026-10-11T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyMicrosoftDeltaPage(ctx, store.MicrosoftDeltaPage{
		AccountID: acct, InboxID: inbox, FolderID: "inbox-folder", Generation: generation,
		Initialized: true, Cursor: "stale-cursor", Prune: true,
		Effects: []store.MicrosoftDeltaEffect{
			{ProviderID: "StaleArrival", Cache: microsoftCache("StaleArrival", "stale arrival", "Stale"), Detect: true,
				Arrival: store.RemoteArrivalInput{
					FolderPath: "microsoft:StaleArrival", RFCMessageID: "<StaleArrival@graph>",
					FromAddress: "sender@outside.test", Subject: "stale arrival", InternalDate: &receivedAt, Source: "microsoft",
				}},
			{ProviderID: "Doomed", Delete: true},
		},
	})
	if !errors.Is(err, store.ErrStaleCursor) {
		t.Fatalf("stale page = %v, want ErrStaleCursor", err)
	}
	if _, err := s.MicrosoftLocalID(ctx, acct, inbox, "StaleArrival"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale page cached metadata: %v", err)
	}
	if _, err := s.GetRemoteMessage(ctx, acct, inbox, doomed); err != nil {
		t.Fatalf("stale page deleted a live row: %v", err)
	}
	if _, err := s.MicrosoftLocalID(ctx, acct, inbox, "Superseded"); err != nil {
		t.Fatalf("stale page pruned a row: %v", err)
	}
	arrivals, err := s.ListRemoteArrivalsForInbox(ctx, acct, inbox, 10)
	if err != nil || len(arrivals) != 0 {
		t.Fatalf("stale page recorded an arrival: %+v (%v)", arrivals, err)
	}
	cursor, initialized, _, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil || cursor != "" || initialized {
		t.Fatalf("stale page advanced the cursor: cursor=%q initialized=%v (%v)", cursor, initialized, err)
	}
}

// TestMicrosoftDeltaPageQuotaRollsBackWholePage proves a metadata-cap refusal
// rolls the page back whole: the message the page had already written leaves no
// metadata or arrival behind, and the cursor does not move, so the next pass
// replays the page from the same baseline.
func TestMicrosoftDeltaPageQuotaRollsBackWholePage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := microsoftPageSetup(t)

	before, err := s.GetAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// Measure what one small cached row costs, so the cap below admits exactly
	// one more small row and refuses the oversized one.
	if _, err := s.ApplyMicrosoftDeltaPage(ctx, store.MicrosoftDeltaPage{
		AccountID: acct, InboxID: inbox, FolderID: "inbox-folder", Generation: "",
		Initialized: true, Cursor: "cursor-1",
		Effects: []store.MicrosoftDeltaEffect{{ProviderID: "Sized", Cache: microsoftCache("Sized", "sized")}},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetAccount(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	growth := after.StorageUsedBytes - before.StorageUsedBytes
	if growth <= 0 {
		t.Fatalf("caching one row did not charge storage (growth=%d)", growth)
	}
	// Room for one small row with a little slack; the estimate mirror used for
	// admission omits the resolved thread key, so it is never larger than the
	// committed cost.
	if err := s.SetAccountStorageQuota(ctx, acct, after.StorageUsedBytes+growth+64); err != nil {
		t.Fatal(err)
	}
	_, _, generation, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil {
		t.Fatal(err)
	}
	receivedAt, err := time.Parse(time.RFC3339, "2026-10-11T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyMicrosoftDeltaPage(ctx, store.MicrosoftDeltaPage{
		AccountID: acct, InboxID: inbox, FolderID: "inbox-folder", Generation: generation,
		Initialized: true, Cursor: "cursor-2", Prune: true,
		Effects: []store.MicrosoftDeltaEffect{
			{ProviderID: "Fits", Cache: microsoftCache("Fits", "fits")},
			{ProviderID: "TooBig", Cache: microsoftCache("TooBig", strings.Repeat("x", 4096))},
		},
	})
	if !errors.Is(err, store.ErrRemoteMetadataQuota) {
		t.Fatalf("over-cap page = %v, want ErrRemoteMetadataQuota", err)
	}
	for _, provider := range []string{"Fits", "TooBig"} {
		if _, err := s.MicrosoftLocalID(ctx, acct, inbox, provider); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("quota rollback left metadata for %s: %v", provider, err)
		}
	}
	cursor, _, _, err := s.MicrosoftDelta(ctx, inbox, "inbox-folder")
	if err != nil || cursor != "cursor-1" {
		t.Fatalf("quota rollback advanced the cursor: %q (%v)", cursor, err)
	}
	// The page replays cleanly once the cap is lifted, and then it arrives.
	if err := s.SetAccountStorageQuota(ctx, acct, 0); err != nil {
		t.Fatal(err)
	}
	page := store.MicrosoftDeltaPage{
		AccountID: acct, InboxID: inbox, FolderID: "inbox-folder", Generation: generation,
		Initialized: true, Cursor: "cursor-2", Prune: true,
		Effects: []store.MicrosoftDeltaEffect{{
			ProviderID: "Fits", Cache: microsoftCache("Fits", "fits"),
			Detect: true,
			Arrival: store.RemoteArrivalInput{
				FolderPath: "microsoft:Fits", RFCMessageID: "<Fits@graph>",
				FromAddress: "sender@outside.test", Subject: "fits", InternalDate: &receivedAt, Source: "microsoft",
			},
		}},
	}
	result, err := s.ApplyMicrosoftDeltaPage(ctx, page)
	if err != nil || result.Arrived != 1 {
		t.Fatalf("replayed page = %+v (%v), want one arrival", result, err)
	}
	if c, _, _, _ := s.MicrosoftDelta(ctx, inbox, "inbox-folder"); c != "cursor-2" {
		t.Fatalf("replayed cursor = %q, want cursor-2", c)
	}
}

// TestMicrosoftUpsertRetargetsSecondProviderID proves the cache keeps one row per
// message when the provider serves that message under a second id form: the
// existing row takes the new provider id (keeping its local id) instead of a
// duplicate appearing.
func TestMicrosoftUpsertRetargetsSecondProviderID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := microsoftPageSetup(t)

	in := store.RemoteMessageInput{FolderPath: "microsoft:feed-id", RFCMessageID: "<dup@graph>", FromAddress: "sender@outside.test", Subject: "MMTEST phantom"}
	if _, err := s.UpsertMicrosoftMessage(ctx, acct, inbox, "feed-id", "thread", "inbox-folder", in); err != nil {
		t.Fatal(err)
	}
	first, err := s.MicrosoftLocalID(ctx, acct, inbox, "feed-id")
	if err != nil {
		t.Fatal(err)
	}
	moved := in
	moved.FolderPath = "microsoft:move-id"
	if _, err = s.UpsertMicrosoftMessage(ctx, acct, inbox, "move-id", "thread", "trash-folder", moved); err != nil {
		t.Fatal(err)
	}
	second, err := s.MicrosoftLocalID(ctx, acct, inbox, "move-id")
	if err != nil || second != first {
		t.Fatalf("second id resolved to %q (%v), want the existing row %q", second, err, first)
	}
	if _, err = s.MicrosoftLocalID(ctx, acct, inbox, "feed-id"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retired id still mapped: %v", err)
	}
	trashed, err := s.ListRemoteMessagesFiltered(ctx, acct, inbox, store.RemoteMessageFilter{FolderPath: "trash-folder"})
	if err != nil || len(trashed) != 1 || trashed[0].ID != first {
		t.Fatalf("trash rows = %+v (%v), want the one retargeted row", trashed, err)
	}
	left, err := s.ListRemoteMessagesFiltered(ctx, acct, inbox, store.RemoteMessageFilter{FolderPath: "inbox-folder"})
	if err != nil || len(left) != 0 {
		t.Fatalf("inbox rows = %+v (%v), want the row to have left", left, err)
	}
}

// TestMicrosoftDedupeFoldsOneMessagesTwoRows proves the one-shot sweep drops a
// duplicate row and that a row with an unsettled action is never folded away: the
// action addresses that row.
func TestMicrosoftDedupeFoldsOneMessagesTwoRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := microsoftPageSetup(t)
	p := model.Principal{AccountID: acct, Admin: true}
	stamped := "2026-10-11T10:00:00Z"

	seed := func(rfc, stale, live string) string {
		t.Helper()
		in := store.RemoteMessageInput{FolderPath: "microsoft:" + stale, RFCMessageID: rfc, FromAddress: "sender@outside.test", Subject: "MMTEST phantom"}
		if _, err := s.UpsertMicrosoftMessage(ctx, acct, inbox, stale, "thread", "inbox-folder", in); err != nil {
			t.Fatal(err)
		}
		// A different received stamp is what the write path cannot fold, so this
		// recreates the duplicate state an inbox reached before it could.
		moved := in
		moved.FolderPath = "microsoft:" + live
		moved.ReceivedAt = &stamped
		if _, err := s.UpsertMicrosoftMessage(ctx, acct, inbox, live, "thread", "trash-folder", moved); err != nil {
			t.Fatal(err)
		}
		id, err := s.MicrosoftLocalID(ctx, acct, inbox, live)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	seed("<fold@graph>", "fold-stale", "fold-live")
	removed, err := s.DedupeMicrosoftInbox(ctx, acct, inbox)
	if err != nil || removed != 1 {
		t.Fatalf("dedupe removed %d (%v), want one duplicate", removed, err)
	}
	rows, err := s.ListRemoteMessagesFiltered(ctx, acct, inbox, store.RemoteMessageFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after dedupe = %+v (%v), want one", rows, err)
	}

	protected := seed("<busy@graph>", "busy-stale", "busy-live")
	if _, err = s.EnqueueMessageAction(ctx, p, inbox, protected, store.MessageActionInput{Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	removed, err = s.DedupeMicrosoftInbox(ctx, acct, inbox)
	if err != nil || removed != 0 {
		t.Fatalf("dedupe removed %d with an unsettled action (%v), want none", removed, err)
	}
	rows, err = s.ListRemoteMessagesFiltered(ctx, acct, inbox, store.RemoteMessageFilter{})
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v (%v), want the protected pair untouched", rows, err)
	}
}
