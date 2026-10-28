package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
)

func TestEmptyTrashLocalSnapshotExcludesLaterTrash(t *testing.T) {
	t.Parallel()
	s, u, _, boxes := testStore(t)
	ctx := context.Background()
	box := boxes[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	first, _, _, err := s.CommitInbound(ctx, inbound(box, "snapshot-first", "<first@test>", "", nil, "First", "body"))
	if err != nil {
		t.Fatal(err)
	}
	later, _, _, err := s.CommitInbound(ctx, inbound(box, "snapshot-later", "<later@test>", "", nil, "Later", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrashMessage(ctx, p, first.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotLocalTrash(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrashMessage(ctx, p, later.ID); err != nil {
		t.Fatal(err)
	}
	// Even an accidental repeated snapshot call after discovery is a no-op.
	if err := s.SnapshotLocalTrash(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueTrashBatch(ctx, a); err != nil || n != 1 {
		t.Fatalf("snapshot targets: %d %v", n, err)
	}
	if err := s.SettleMessageAction(ctx, a.ID, "pending", "", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	child, ok, err := s.ClaimMessageAction(ctx, "test")
	if err != nil || !ok || child.MessageID != first.ID || child.Input.TrashTarget.ID != first.ID {
		t.Fatalf("snapshot child: %+v %v %v", child, ok, err)
	}
	progress, err := s.EmptyTrashProgress(ctx, a.ID)
	if err != nil || progress.Total != 1 {
		t.Fatalf("snapshot progress: %+v %v", progress, err)
	}
}
