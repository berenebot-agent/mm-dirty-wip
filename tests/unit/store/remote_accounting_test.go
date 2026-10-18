package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestRemoteMetadataIncrementalAccountingMatchesAuthoritative proves every
// individual remote-metadata mutation adjusts the counters by exactly the same
// bytes the authoritative SQL total changes by. The check runs across inserts,
// updates, labels, moves, deletes, folder removal and a thread merge.
func TestRemoteMetadataIncrementalAccountingMatchesAuthoritative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inboxID := remoteMetaSetup(t)

	if _, err := s.ReconcileRemoteFolders(ctx, acct, inboxID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
		{Path: "Archive", Name: "Archive", Role: model.FolderRoleArchive, Selectable: true, UIDValidity: 2},
	}); err != nil {
		t.Fatal(err)
	}
	used := accountUsed(t, s, acct)
	auth, err := s.RemoteMetadataBytes(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(step string) {
		t.Helper()
		nextUsed := accountUsed(t, s, acct)
		nextAuth, aerr := s.RemoteMetadataBytes(ctx, acct, inboxID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if nextUsed-used != nextAuth-auth {
			t.Fatalf("%s: counter delta=%d authoritative delta=%d", step, nextUsed-used, nextAuth-auth)
		}
		inboxUsed, ierr := s.InboxStorageUsed(ctx, acct, inboxID)
		if ierr != nil {
			t.Fatal(ierr)
		}
		if inboxUsed != nextUsed {
			t.Fatalf("%s: inbox counter=%d account counter=%d", step, inboxUsed, nextUsed)
		}
		used, auth = nextUsed, nextAuth
	}

	// A reply arrives before its parent, then the parent merges the sub-thread.
	if _, err := s.ReconcileRemoteFolderBatch(ctx, acct, inboxID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<child@remote>", InReplyTo: "<parent@remote>", References: []string{"<parent@remote>"}, FromAddress: "a@outside.test", Subject: "Child"},
		{UID: 2, RFCMessageID: "<parent@remote>", InReplyTo: "<grandparent@remote>", FromAddress: "b@outside.test", Subject: "Parent"},
	}, nil, false, 0, false); err != nil {
		t.Fatal(err)
	}
	check("batch insert and thread merge")

	child, err := s.GetRemoteMessageByUID(ctx, acct, inboxID, "INBOX", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRemoteMessageFlags(ctx, acct, inboxID, child.ID, true, true, false, false, []string{"\\Seen", "\\Flagged"}); err != nil {
		t.Fatal(err)
	}
	check("flag update")

	if _, err := s.SetRemoteMessageLabels(ctx, acct, inboxID, child.ID, []string{"work", "later"}); err != nil {
		t.Fatal(err)
	}
	check("label replacement")

	if _, err := s.MoveRemoteMessageMetadata(ctx, acct, inboxID, child.ID, "Archive", 2, 50); err != nil {
		t.Fatal(err)
	}
	check("move")

	if err := s.DeleteRemoteMessage(ctx, acct, inboxID, child.ID); err != nil {
		t.Fatal(err)
	}
	check("delete")

	if _, err := s.UpsertRemoteMessage(ctx, acct, inboxID, store.RemoteMessageInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 30, RFCMessageID: "<single@remote>", FromAddress: "c@outside.test", Subject: "Short",
	}); err != nil {
		t.Fatal(err)
	}
	check("single insert")

	if _, err := s.UpsertRemoteMessage(ctx, acct, inboxID, store.RemoteMessageInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 30, RFCMessageID: "<single@remote>", FromAddress: "c@outside.test", Subject: "A much longer subject than before",
	}); err != nil {
		t.Fatal(err)
	}
	check("single update")

	if err := s.DeleteRemoteFolderRow(ctx, acct, inboxID, "Archive"); err != nil {
		t.Fatal(err)
	}
	check("folder row removal")

	// The explicit check must agree with the counters after every step.
	finalUsed := accountUsed(t, s, acct)
	finalAuth, err := s.RemoteMetadataBytes(ctx, acct, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	if finalUsed != finalAuth {
		t.Fatalf("final account counter=%d authoritative=%d", finalUsed, finalAuth)
	}
}
