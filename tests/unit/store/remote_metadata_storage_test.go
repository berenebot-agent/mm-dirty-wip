package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// remoteMetaSetup creates a standalone inbox on an isolated account so its
// storage counters start at zero and are unaffected by any domain mail.
func remoteMetaSetup(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	u, err := s.CreateAccountAndAdmin(ctx, "RemoteMeta", "rm-admin@example.com", "correct horse battery staple", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "meta@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	return s, u.AccountID, in.ID
}

func accountUsed(t *testing.T, s *store.Store, accountID string) int64 {
	t.Helper()
	a, err := s.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return a.StorageUsedBytes
}

// TestRemoteMetadataCountsTowardQuota proves stored remote message metadata is
// charged to both the account and the inbox storage counters, and that the
// remote-reported size_bytes is NOT what is charged.
func TestRemoteMetadataCountsTowardQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)

	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}

	before := accountUsed(t, s, acct)
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test",
			To: []string{"x@y.test"}, References: []string{"<root@x>"}, Snippet: "hello", SizeBytes: 5_000_000},
	}, true); err != nil {
		t.Fatal(err)
	}

	used := accountUsed(t, s, acct)
	if used <= before {
		t.Fatalf("account storage did not grow with remote metadata: before=%d after=%d", before, used)
	}
	// The huge remote body claim must not have been charged: if it had, the growth
	// would be at least 5 MB. The stored metadata is a few hundred bytes.
	if used-before >= 1_000_000 {
		t.Fatalf("remote size_bytes was charged: growth=%d, want only stored metadata bytes", used-before)
	}
	inboxUsed, err := s.InboxStorageUsed(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	if inboxUsed != used {
		t.Fatalf("inbox used=%d account used=%d, want equal after folder+message metadata", inboxUsed, used)
	}
}

// TestRemoteMetadataDoesNotChargeSizeBytes proves the reported remote body size
// is ignored: two messages differing only in size_bytes cost the same.
func TestRemoteMetadataDoesNotChargeSizeBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	base := accountUsed(t, s, acct)
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<a@remote>", Subject: "S", FromAddress: "s@x.test", SizeBytes: 1},
	}, true); err != nil {
		t.Fatal(err)
	}
	afterSmall := accountUsed(t, s, acct)
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 2, RFCMessageID: "<b@remote>", Subject: "S", FromAddress: "s@x.test", SizeBytes: 900_000_000},
	}, false); err != nil {
		t.Fatal(err)
	}
	afterBig := accountUsed(t, s, acct)
	deltaSmall := afterSmall - base
	deltaBig := afterBig - afterSmall
	if deltaSmall != deltaBig {
		t.Fatalf("size_bytes influenced the charge: small=%d big=%d", deltaSmall, deltaBig)
	}
}

// TestRemoteMetadataRefundedOnDelete proves removing remote metadata returns its
// bytes to the counters.
func TestRemoteMetadataRefundedOnDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	baseline := accountUsed(t, s, acct)
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test"},
	}, true); err != nil {
		t.Fatal(err)
	}
	msg, err := s.GetRemoteMessageByUID(ctx, acct, inboxID, "INBOX", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRemoteMessage(ctx, acct, inboxID, msg.ID); err != nil {
		t.Fatal(err)
	}
	if got := accountUsed(t, s, acct); got != baseline {
		t.Fatalf("after delete account used=%d want baseline=%d", got, baseline)
	}
	inboxUsed, err := s.InboxStorageUsed(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	if inboxUsed != baseline {
		t.Fatalf("after delete inbox used=%d want baseline=%d", inboxUsed, baseline)
	}
}

// TestRemoteMetadataRefundedOnReset proves ResetRemoteInboxState refunds every
// cached remote byte.
func TestRemoteMetadataRefundedOnReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test"},
		{UID: 2, RFCMessageID: "<m2@remote>", Subject: "Two", FromAddress: "s@x.test"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if accountUsed(t, s, acct) == 0 {
		t.Fatal("expected nonzero usage before reset")
	}
	if err := s.ResetRemoteInboxState(ctx, acct, inboxID); err != nil {
		t.Fatal(err)
	}
	if got := accountUsed(t, s, acct); got != 0 {
		t.Fatalf("after reset account used=%d want 0", got)
	}
	inboxUsed, err := s.InboxStorageUsed(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	if inboxUsed != 0 {
		t.Fatalf("after reset inbox used=%d want 0", inboxUsed)
	}
}

// TestRemoteMetadataLabelsAndFoldersCount proves label and folder metadata are
// charged, and refunded when cleared.
func TestRemoteMetadataLabelsAndFoldersCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	base := accountUsed(t, s, acct)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
		{Path: "Archive", Name: "Archive", Role: model.FolderRoleArchive, Selectable: true, UIDValidity: 2},
	}); err != nil {
		t.Fatal(err)
	}
	afterFolders := accountUsed(t, s, acct)
	if afterFolders <= base {
		t.Fatalf("folder metadata not charged: base=%d after=%d", base, afterFolders)
	}
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test"},
	}, true); err != nil {
		t.Fatal(err)
	}
	msg, err := s.GetRemoteMessageByUID(ctx, acct, inboxID, "INBOX", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	beforeLabels := accountUsed(t, s, acct)
	if _, err := s.SetRemoteMessageLabels(ctx, acct, inboxID, msg.ID, []string{"work", "keeping-this-label"}); err != nil {
		t.Fatal(err)
	}
	afterLabels := accountUsed(t, s, acct)
	if afterLabels <= beforeLabels {
		t.Fatalf("label metadata not charged: before=%d after=%d", beforeLabels, afterLabels)
	}
	if _, err := s.SetRemoteMessageLabels(ctx, acct, inboxID, msg.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := accountUsed(t, s, acct); got != beforeLabels {
		t.Fatalf("label removal did not refund: got=%d want=%d", got, beforeLabels)
	}
}

// TestRemoteMetadataCapStopsIndexing proves that once the account cap is reached
// the reconcile stops adding new rows (leaving the folder partial) rather than
// failing with ErrQuota, and that existing rows remain.
func TestRemoteMetadataCapStopsIndexing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Cap the account very low so only a couple of message rows fit.
	if err := s.SetAccountStorageQuota(ctx, acct, 500); err != nil {
		t.Fatal(err)
	}
	msgs := make([]store.RemoteMessageInput, 0, 20)
	for i := 1; i <= 20; i++ {
		msgs = append(msgs, store.RemoteMessageInput{
			UID: uint32(i), RFCMessageID: "<m" + string(rune('a'+i)) + "@remote>",
			Subject: "Subject number one", FromAddress: "sender@example.test",
		})
	}
	written, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, msgs, true)
	if err != nil {
		t.Fatalf("cap-truncated reconcile must not error: %v", err)
	}
	if written == 0 || written == len(msgs) {
		t.Fatalf("expected a partial write under the cap, wrote %d of %d", written, len(msgs))
	}
	msgsList, err := s.ListRemoteMessages(ctx, acct, inboxID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgsList) != written {
		t.Fatalf("cached rows=%d want %d", len(msgsList), written)
	}
	// The folder must not be marked complete by a truncated pass.
	bf, err := s.GetRemoteFolderBackfill(ctx, acct, inboxID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if bf.Complete {
		t.Fatal("a cap-truncated pass must not mark the folder backfill complete")
	}
}

// TestRemoteMetadataUpsertCapSentinel proves the single-message upsert path
// returns ErrRemoteMetadataQuota when its new row would exceed the cap.
func TestRemoteMetadataUpsertCapSentinel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if err := s.SetAccountStorageQuota(ctx, acct, 50); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpsertRemoteMessage(ctx, acct, inboxID, store.RemoteMessageInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 1, RFCMessageID: "<big@remote>",
		Subject: "Subject", FromAddress: "s@x.test",
	})
	if !errors.Is(err, store.ErrRemoteMetadataQuota) {
		t.Fatalf("upsert over cap: err=%v want ErrRemoteMetadataQuota", err)
	}
}

// TestInboxStorageUsedMatchesMaintainedCounter proves the read fallback (SUM)
// equals the maintained counter after remote writes, exercising the SQL byte
// definition end to end.
func TestInboxStorageUsedMatchesMaintainedCounter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)
	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileRemoteFolderMessages(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test", To: []string{"a@b.test"}},
		{UID: 2, RFCMessageID: "<m2@remote>", Subject: "Two", FromAddress: "s@x.test"},
	}, true); err != nil {
		t.Fatal(err)
	}
	msg, err := s.GetRemoteMessageByUID(ctx, acct, inboxID, "INBOX", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemoteMessageLabels(ctx, acct, inboxID, msg.ID, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	// Read fallback: NULL the counter so InboxStorageUsed must SUM.
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE inboxes SET storage_used_bytes=NULL WHERE id=?`, inboxID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	sum, err := s.InboxStorageUsed(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	if sum != accountUsed(t, s, acct) {
		t.Fatalf("read SUM=%d account counter=%d, want equal", sum, accountUsed(t, s, acct))
	}
}
