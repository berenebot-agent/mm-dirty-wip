package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestFolderChangedEventCarriesCanonicalFolder proves the durable
// folder_changed event names the canonical folder: the one the message
// projection reports. A message filed in the Archive or in a custom folder has
// no canonical folder, so the event reports null instead of a folder the
// reader cannot select.
func TestFolderChangedEventCarriesCanonicalFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	inbox, err := s.GetSystemFolder(ctx, u.AccountID, b[0].ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := s.GetSystemFolder(ctx, u.AccountID, b[0].ID, model.FolderRoleArchive)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := s.CreateFolder(ctx, p, b[0].ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := s.CommitInbound(ctx, inbound(b[0], "d1", "<one@test>", "", nil, "hello", "body"))
	if err != nil {
		t.Fatal(err)
	}
	// Into the Archive: the message leaves the Inbox view, but the Archive is
	// not a canonical location.
	_, ev, err := s.MoveMessageToFolder(ctx, p, msg.ID, archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil {
		t.Fatal("no folder_changed event")
	}
	if id, ok := ev.Payload["new_folder_id"]; !ok || id != nil {
		t.Fatalf("new_folder_id = %v, want null for the archive", ev.Payload["new_folder_id"])
	}
	if id, ok := ev.Payload["old_folder_id"]; !ok || id != inbox.ID {
		t.Fatalf("old_folder_id = %v, want the inbox folder", ev.Payload["old_folder_id"])
	}
	// Into a custom folder: also no canonical location.
	_, ev, err = s.MoveMessageToFolder(ctx, p, msg.ID, custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id := ev.Payload["new_folder_id"]; id != nil {
		t.Fatalf("new_folder_id = %v, want null for a custom folder", id)
	}
	// Back to the Inbox: the canonical Inbox folder.
	_, ev, err = s.MoveMessageToFolder(ctx, p, msg.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if id := ev.Payload["new_folder_id"]; id != inbox.ID {
		t.Fatalf("new_folder_id = %v, want the inbox folder", id)
	}
	if path := ev.Payload["new_folder_path"]; path != inbox.Path {
		t.Fatalf("new_folder_path = %v, want %q", path, inbox.Path)
	}
	_ = time.Now
}
