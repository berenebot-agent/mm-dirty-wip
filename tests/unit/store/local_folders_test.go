package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// localFolderStore builds a domain inbox owned by an admin principal and returns
// the store, principal, inbox and a second inbox owned by the same account. The
// principal has Owner on box and Read on other.
func localFolderStore(t *testing.T) (*store.Store, model.Principal, model.Inbox) {
	t.Helper()
	s, u, _, b := testStore(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	return s, p, b[0]
}

// TestMigration054FolderModel proves migration 054 adds the local/domain folder
// model on a genuinely fresh database: messages.mailbox_id, the folder
// system/origin/metadata columns and the folder indexes. It uses store.Open on a
// real temp dir because it asserts migration behaviour.
func TestMigration054FolderModel(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "054"); n != 1 {
		t.Fatalf("migration 054 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('messages') WHERE name='mailbox_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("messages.mailbox_id missing after migration 054")
	}
	for _, col := range []string{"is_system", "origin", "remote_metadata_json"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_folders') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inbox_folders.%s missing after migration 054", col)
		}
	}
	for _, idx := range []string{"idx_messages_mailbox", "idx_inbox_folders_role"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %s missing after migration 054", idx)
		}
	}
}

// TestLocalFolderCRUDAndProtectedRoles proves a custom folder can be created,
// renamed and deleted (when empty) on a domain inbox, that the protected system
// roles cannot be renamed or deleted, and that a non-empty custom folder refuses
// deletion.
func TestLocalFolderCRUDAndProtectedRoles(t *testing.T) {
	ctx := context.Background()
	s, p, box := localFolderStore(t)

	folders, err := s.EnsureSystemFolders(ctx, p.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, f := range folders {
		roles[f.Role] = true
		if !f.IsSystem {
			t.Fatalf("system folder %q is not marked system", f.Role)
		}
	}
	for _, want := range []string{model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleDrafts, model.FolderRoleArchive, model.FolderRoleOutbox, model.FolderRoleSpam, model.FolderRoleTrash} {
		if !roles[want] {
			t.Fatalf("missing seeded system role %q", want)
		}
	}
	// Seeding is idempotent.
	again, err := s.EnsureSystemFolders(ctx, p.AccountID, box.ID)
	if err != nil || len(again) != len(folders) {
		t.Fatalf("reseed changed folder set: %d vs %d (%v)", len(again), len(folders), err)
	}

	// Custom folder create/rename/delete.
	custom, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects", Name: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Role != model.FolderRoleFolder || custom.IsSystem {
		t.Fatalf("custom folder role=%q system=%v", custom.Role, custom.IsSystem)
	}
	renamed, err := s.RenameFolder(ctx, p, box.ID, custom.ID, "Projects 2026")
	if err != nil || renamed.Name != "Projects 2026" {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	if err := s.DeleteFolder(ctx, p, box.ID, custom.ID); err != nil {
		t.Fatalf("delete empty custom folder: %v", err)
	}

	// Protected system folders cannot be renamed or deleted.
	inboxFolder, err := s.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameFolder(ctx, p, box.ID, inboxFolder.ID, "Start"); !errors.Is(err, store.ErrFolderProtected) {
		t.Fatalf("rename system folder err=%v, want ErrFolderProtected", err)
	}
	if err := s.DeleteFolder(ctx, p, box.ID, inboxFolder.ID); !errors.Is(err, store.ErrFolderProtected) {
		t.Fatalf("delete system folder err=%v, want ErrFolderProtected", err)
	}

	// A non-empty custom folder refuses deletion, and a trashed message still
	// counts as content.
	nonEmpty, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Keep"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Keep"}); !errors.Is(err, store.ErrFolderNameConflict) {
		t.Fatalf("duplicate path err=%v, want ErrFolderNameConflict", err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "folder-1", "<f1@test>", "", nil, "filed", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MoveMessageToFolder(ctx, p, m.ID, nonEmpty.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ctx, p, box.ID, nonEmpty.ID); !errors.Is(err, store.ErrFolderNotEmpty) {
		t.Fatalf("delete non-empty folder err=%v, want ErrFolderNotEmpty", err)
	}
	// Trashing does not empty the folder: membership survives.
	if _, _, err := s.TrashMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFolder(ctx, p, box.ID, nonEmpty.ID); !errors.Is(err, store.ErrFolderNotEmpty) {
		t.Fatalf("delete folder with trashed message err=%v, want ErrFolderNotEmpty", err)
	}
}

// TestMoveToFolderSingleMembershipAndInboxExclusion proves a message belongs to
// exactly one folder, that moving it to a custom/archive folder removes it from
// the Inbox view, that the change is reversible, and that labels are independent
// of folders.
func TestMoveToFolderSingleMembershipAndInboxExclusion(t *testing.T) {
	ctx := context.Background()
	s, p, box := localFolderStore(t)
	archive, err := s.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleArchive)
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "mv-1", "<mv@test>", "", nil, "hello", "body"))
	if err != nil {
		t.Fatal(err)
	}
	// Freshly delivered mail is in the implicit Inbox bucket.
	if m.MailboxID != "" {
		t.Fatalf("new message mailbox_id=%q, want empty (implicit Inbox)", m.MailboxID)
	}
	// A label is independent of folder membership.
	if _, err := s.ReplaceMessageLabels(ctx, p, m.ID, []string{"important"}); err != nil {
		t.Fatal(err)
	}

	inboxFilter := store.MessageFilter{InboxID: box.ID, FolderScoped: true, InboxRole: model.FolderRoleInbox, Limit: 50}
	before, err := s.ListMessages(ctx, p, inboxFilter)
	if err != nil || len(before) != 1 {
		t.Fatalf("inbox before move: %d %v", len(before), err)
	}

	moved, ev, err := s.MoveMessageToFolder(ctx, p, m.ID, archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.MailboxID != archive.ID {
		t.Fatalf("moved mailbox_id=%q, want %q", moved.MailboxID, archive.ID)
	}
	if ev == nil || ev.Type != model.EventMessageFolderChanged {
		t.Fatalf("move event = %+v", ev)
	}
	// The Inbox view no longer shows it; the Archive folder view does.
	inboxAfter, err := s.ListMessages(ctx, p, inboxFilter)
	if err != nil || len(inboxAfter) != 0 {
		t.Fatalf("inbox after move: %d %v", len(inboxAfter), err)
	}
	archiveFilter := store.MessageFilter{InboxID: box.ID, MailboxID: archive.ID, FolderScoped: true}
	archiveList, err := s.ListMessages(ctx, p, archiveFilter)
	if err != nil || len(archiveList) != 1 || archiveList[0].ID != m.ID {
		t.Fatalf("archive view: %d %v", len(archiveList), err)
	}
	// Count, search and bulk enumeration agree with the filtered list.
	if n, err := s.CountMessages(ctx, p, inboxFilter); err != nil || n != 0 {
		t.Fatalf("count inbox after move = %d %v", n, err)
	}
	search, err := s.SearchMessagesFiltered(ctx, p, "hello", archiveFilter)
	if err != nil || len(search) != 1 {
		t.Fatalf("search archive: %d %v", len(search), err)
	}
	ids, err := s.AllMessageIDs(ctx, p, archiveFilter)
	if err != nil || len(ids) != 1 {
		t.Fatalf("all ids archive: %d %v", len(ids), err)
	}
	// Labels survive the folder move.
	got, err := s.GetMessage(ctx, p, m.ID)
	if err != nil || len(got.Labels) != 1 || got.Labels[0] != "important" {
		t.Fatalf("labels after move = %#v %v", got.Labels, err)
	}
	// Moving back to the implicit Inbox restores the Inbox view.
	if _, _, err := s.MoveMessageToFolder(ctx, p, m.ID, ""); err != nil {
		t.Fatal(err)
	}
	back, err := s.ListMessages(ctx, p, inboxFilter)
	if err != nil || len(back) != 1 {
		t.Fatalf("inbox after move back: %d %v", len(back), err)
	}
}

// TestSentAndInboxNotConflated proves the Sent view (direction=outbound) is not
// conflated with folder membership: an outbound message stays in Sent and does
// not appear in the Inbox view, and does not require a folder row.
func TestSentAndInboxNotConflated(t *testing.T) {
	ctx := context.Background()
	s, p, box := localFolderStore(t)
	if _, _, err := s.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "smtp", RFCMessageID: "<out1@test>",
		From: model.Address{Address: box.Address}, To: []string{"x@y.test"},
		Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	sent, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Direction: "outbound"})
	if err != nil || len(sent) != 1 {
		t.Fatalf("sent view: %d %v", len(sent), err)
	}
	inbox, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Direction: "inbound", FolderScoped: true, InboxRole: model.FolderRoleInbox})
	if err != nil || len(inbox) != 0 {
		t.Fatalf("inbox must not show outbound mail: %d %v", len(inbox), err)
	}
}

// TestAssistantPermissionsAndCrossInboxIsolation proves folder CRUD and moves
// require Assistant or Owner, and that a folder in another inbox cannot be a
// move destination.
func TestAssistantPermissionsAndCrossInboxIsolation(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box, other := b[0], b[1]
	// A read-only principal may not create or delete folders.
	_, key, err := s.CreateAPIKey(ctx, u.AccountID, "ro", false, map[string]string{box.ID: "read", other.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	ro, err := s.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(ctx, ro, box.ID, store.FolderCreate{Path: "Nope"}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read create folder err=%v, want ErrForbidden", err)
	}
	// The assistant on `other` may create a folder there but cannot move a
	// message into another inbox's folder.
	folder, err := s.CreateFolder(ctx, ro, other.ID, store.FolderCreate{Path: "Assist"})
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "iso-1", "<iso@test>", "", nil, "iso", "x"))
	if err != nil {
		t.Fatal(err)
	}
	// ro has no write role on box; the move is forbidden before the folder is
	// even considered.
	if _, _, err := s.MoveMessageToFolder(ctx, ro, m.ID, folder.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("cross-inbox move err=%v, want ErrForbidden", err)
	}
	// An owner moving into a folder that belongs to a different inbox is refused
	// as not found.
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, _, err := s.MoveMessageToFolder(ctx, admin, m.ID, folder.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("move into foreign folder err=%v, want ErrNotFound", err)
	}
}

// TestTrashRestorePreservesFolder proves trashing keeps a message's folder
// membership and restoring returns it to that same folder, and that only the
// Owner can permanently purge.
func TestTrashRestorePreservesFolder(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	custom, err := s.CreateFolder(ctx, admin, box.ID, store.FolderCreate{Path: "Later"})
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "tr-1", "<tr@test>", "", nil, "later", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MoveMessageToFolder(ctx, admin, m.ID, custom.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrashMessage(ctx, admin, m.ID); err != nil {
		t.Fatal(err)
	}
	trashed, err := s.GetMessage(ctx, admin, m.ID)
	if err != nil || trashed.MailboxID != custom.ID {
		t.Fatalf("trashed message folder=%q, want %q", trashed.MailboxID, custom.ID)
	}
	if _, _, err := s.RestoreMessage(ctx, admin, m.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetMessage(ctx, admin, m.ID)
	if err != nil || restored.MailboxID != custom.ID || restored.FolderPath != "Later" {
		t.Fatalf("restored message folder=%q path=%q, want Later", restored.MailboxID, restored.FolderPath)
	}
	// Purge is Owner-only.
	_, rkey, err := s.CreateAPIKey(ctx, u.AccountID, "assist", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	assist, err := s.APIKeyPrincipal(ctx, rkey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrashMessage(ctx, assist, m.ID); err != nil {
		t.Fatalf("assistant trash: %v", err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, assist, m.ID); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("assistant purge err=%v, want ErrForbidden", err)
	}
	if _, _, _, err := s.PurgeMessage(ctx, admin, m.ID); err != nil {
		t.Fatalf("owner purge: %v", err)
	}
	if _, err := s.GetMessage(ctx, admin, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("purged message still readable: %v", err)
	}
}

// TestFolderStatsAndBothKinds proves folder stats count only physically filed
// mail, and that custom folders work on a standalone inbox as well as a domain
// inbox.
func TestFolderStatsAndBothKinds(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	custom, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Stats"})
	if err != nil {
		t.Fatal(err)
	}
	m, _, _, err := s.CommitInbound(ctx, inbound(box, "stats-1", "<s1@test>", "", nil, "filed", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MoveMessageToFolder(ctx, p, m.ID, custom.ID); err != nil {
		t.Fatal(err)
	}
	stats, err := s.CountFolderMessages(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total[custom.ID] != 1 || stats.Unread[custom.ID] != 1 {
		t.Fatalf("custom folder stats = %+v", stats)
	}
	if stats.InboxTotal != 0 {
		t.Fatalf("implicit inbox total = %d, want 0", stats.InboxTotal)
	}

	// A standalone inbox supports the same custom folders and system seeding.
	standalone, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "sa@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	sf, err := s.EnsureSystemFolders(ctx, u.AccountID, standalone.ID)
	if err != nil || len(sf) == 0 {
		t.Fatalf("standalone system folders: %d %v", len(sf), err)
	}
	if _, err := s.CreateFolder(ctx, p, standalone.ID, store.FolderCreate{Path: "Local"}); err != nil {
		t.Fatalf("custom folder on standalone: %v", err)
	}
	if _, err := s.CreateFolder(ctx, p, standalone.ID, store.FolderCreate{Path: "Local/2026", Name: "2026", Parent: "Local"}); err != nil {
		t.Fatalf("nested custom folder on standalone: %v", err)
	}
}

// TestRenameFolderMovesSubtree proves renaming a folder rewrites its path and the
// path/parent_path of every descendant, preserving stable ids, and refuses a
// collision atomically.
func TestRenameFolderMovesSubtree(t *testing.T) {
	ctx := context.Background()
	s, p, box := localFolderStore(t)
	parent, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects/2026", Name: "2026", Parent: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	grand, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects/2026/Q1", Name: "Q1", Parent: "Projects/2026"})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := s.RenameFolder(ctx, p, box.ID, parent.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != parent.ID || renamed.Path != "Archive" {
		t.Fatalf("parent after rename: %+v", renamed)
	}
	// Descendants keep their ids but gain the new path prefix.
	gotChild, err := s.GetFolder(ctx, p.AccountID, box.ID, "Archive/2026")
	if err != nil {
		t.Fatal(err)
	}
	if gotChild.ID != child.ID || gotChild.ParentPath != "Archive" {
		t.Fatalf("child after rename: %+v", gotChild)
	}
	gotGrand, err := s.GetFolder(ctx, p.AccountID, box.ID, "Archive/2026/Q1")
	if err != nil {
		t.Fatal(err)
	}
	if gotGrand.ID != grand.ID || gotGrand.ParentPath != "Archive/2026" {
		t.Fatalf("grandchild after rename: %+v", gotGrand)
	}
	// A collision with an unrelated existing folder is refused.
	if _, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Sent2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameFolder(ctx, p, box.ID, renamed.ID, "Sent2"); !errors.Is(err, store.ErrFolderNameConflict) {
		t.Fatalf("colliding rename err=%v, want ErrFolderNameConflict", err)
	}
	if _, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Archive/2027", Name: "2027", Parent: "Archive"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameFolder(ctx, p, box.ID, gotChild.ID, "2027"); !errors.Is(err, store.ErrFolderNameConflict) {
		t.Fatalf("colliding child rename err=%v, want ErrFolderNameConflict", err)
	}
}

func TestFolderRemoteMetadata(t *testing.T) {
	ctx := context.Background()
	s, p, box := localFolderStore(t)
	folder, err := s.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Remote"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetFolderRemoteMetadata(ctx, p.AccountID, box.ID, folder.ID, `{"uid_validity":42}`); err != nil {
		t.Fatal(err)
	}
	meta, err := s.GetFolderRemoteMetadata(ctx, p.AccountID, box.ID, folder.ID)
	if err != nil || meta != `{"uid_validity":42}` {
		t.Fatalf("metadata = %q %v", meta, err)
	}
	if err := s.SetFolderRemoteMetadata(ctx, p.AccountID, box.ID, "missing", "{}"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("metadata on missing folder err=%v", err)
	}
}
