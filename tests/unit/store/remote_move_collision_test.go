package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mammothmail/internal/store"
)

// A sync can index a confirmed move's destination before the local half of the
// move runs. The move must coalesce onto the source id instead of failing on
// the unique locator constraint.
func TestRemoteMoveReconcilesIndexedDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, acct, inbox := remoteMetaSetup(t)
	source, err := s.UpsertRemoteMessage(ctx, acct, inbox, store.RemoteMessageInput{FolderPath: "INBOX", UIDValidity: 1, UID: 10, RFCMessageID: "<move@test>", Subject: "source"})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := s.UpsertRemoteMessage(ctx, acct, inbox, store.RemoteMessageInput{FolderPath: "Trash", UIDValidity: 2, UID: 20, RFCMessageID: "<move@test>", Subject: "destination"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemoteMessageLabels(ctx, acct, inbox, source.ID, []string{"source"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRemoteMessageLabels(ctx, acct, inbox, dest.ID, []string{"destination"}); err != nil {
		t.Fatal(err)
	}
	before := accountUsed(t, s, acct)
	beforeBytes, err := s.RemoteMetadataBytes(ctx, acct, inbox)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveRemoteMessageMetadata(ctx, acct, inbox, source.ID, "Trash", 2, 20)
	if err != nil {
		t.Fatalf("confirmed move must reconcile an already-indexed destination: %v", err)
	}
	if got.ID != source.ID || got.FolderPath != "Trash" || got.UID != 20 {
		t.Fatalf("stable moved identity lost: %+v", got)
	}
	labels, err := s.RemoteMessageLabels(ctx, acct, inbox, source.ID)
	if err != nil || len(labels) != 2 {
		t.Fatalf("merged labels = %v, %v", labels, err)
	}
	afterBytes, err := s.RemoteMetadataBytes(ctx, acct, inbox)
	if err != nil {
		t.Fatal(err)
	}
	if delta := accountUsed(t, s, acct) - before; delta != afterBytes-beforeBytes {
		t.Fatalf("accounting delta=%d authoritative=%d", delta, afterBytes-beforeBytes)
	}
}
