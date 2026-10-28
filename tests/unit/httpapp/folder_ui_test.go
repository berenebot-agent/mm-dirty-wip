package httpapp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestFolderBulkActionStaysInFolder proves a bulk action taken from a custom
// folder returns the user to that folder, not the Inbox, and still applies the
// action to the selected message.
func TestFolderBulkActionStaysInFolder(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, admin, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	msg := seedInbound(t, svc, box, "fold-bulk", "<fold-bulk@test>", "Folded", "body")
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, admin, msg.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk",
		"_csrf="+csrf+"&folder=folder&folder_id="+folder.ID+"&scope=page&action=read&ids="+msg.ID)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("folder bulk read = %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/folder?folder="+folder.ID) {
		t.Fatalf("folder bulk action redirected to %q; want the folder view", loc)
	}
	got, err := svc.Store.GetMessage(ctx, admin, msg.ID)
	if err != nil || !got.Read {
		t.Fatalf("folder message not marked read: %+v %v", got, err)
	}
}

// TestFolderBulkSelectAllLocal proves "select all in this folder" on a domain
// inbox re-derives the set from the folder, not the whole inbox and not an
// error. Before the fix mailboxFilter rejected the "folder" view with a 400.
func TestFolderBulkSelectAllLocal(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, admin, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	inFolder := []model.Message{
		seedInbound(t, svc, box, "fold-a", "<fold-a@test>", "A", "body"),
		seedInbound(t, svc, box, "fold-b", "<fold-b@test>", "B", "body"),
	}
	for _, m := range inFolder {
		if _, _, err := svc.Store.MoveMessageToFolder(ctx, admin, m.ID, folder.ID); err != nil {
			t.Fatal(err)
		}
	}
	stays := seedInbound(t, svc, box, "fold-inbox", "<fold-inbox@test>", "Inbox", "body")
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk",
		"_csrf="+csrf+"&folder=folder&folder_id="+folder.ID+"&scope=all&action=read")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("folder scope=all read = %d body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/folder?folder="+folder.ID) {
		t.Fatalf("folder scope=all redirected to %q", loc)
	}
	for _, m := range inFolder {
		got, err := svc.Store.GetMessage(ctx, admin, m.ID)
		if err != nil || !got.Read {
			t.Fatalf("folder message %s not marked read: %+v %v", m.ID, got, err)
		}
	}
	other, err := svc.Store.GetMessage(ctx, admin, stays.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Read {
		t.Fatal("a message outside the folder was changed by scope=all")
	}
}

// TestFolderBulkSelectAllRemote proves "select all in this folder" on a
// standalone (Microsoft/Gmail/IMAP) inbox acts on the custom folder's remote
// messages, not the namespace root (the Inbox). The regression acted on the
// Inbox's messages instead.
func TestFolderBulkSelectAllRemote(t *testing.T) {
	t.Parallel()
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	ctx := context.Background()
	fake.add("Archive", "From: a@b.test\r\nSubject: Archived 1\r\nMessage-ID: <arc1@remote>\r\n\r\nbody", "<arc1@remote>", "Archived 1")
	fake.add("Archive", "From: a@b.test\r\nSubject: Archived 2\r\nMessage-ID: <arc2@remote>\r\n\r\nbody", "<arc2@remote>", "Archived 2")
	fake.add("INBOX", "From: a@b.test\r\nSubject: Inbox one\r\nMessage-ID: <in1@remote>\r\n\r\nbody", "<in1@remote>", "Inbox one")
	refreshRemote(t, h, standalone.ID, adminKey(t, svc, u))

	folders, err := svc.Store.ListFolders(ctx, u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	var archiveID string
	for _, f := range folders {
		if f.Path == "Archive" {
			archiveID = f.ID
		}
	}
	if archiveID == "" {
		t.Fatalf("Archive folder not indexed: %+v", folders)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/bulk",
		"_csrf="+csrf+"&folder=folder&folder_id="+archiveID+"&scope=all&action=read")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("remote folder scope=all read = %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/folder?folder="+archiveID) {
		t.Fatalf("remote folder scope=all redirected to %q", loc)
	}
	// Exactly the two Archive messages were acted on; the Inbox message was not.
	if !strings.Contains(loc, "2+messages+will+be+marked+read") {
		t.Fatalf("remote folder scope=all notice = %q; want 2 messages (the folder's), not the Inbox's", loc)
	}
}

// TestFolderRenameRedirectsToFolder proves renaming a folder returns the user to
// the renamed folder rather than the Inbox.
func TestFolderRenameRedirectsToFolder(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, admin, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/folders/"+folder.ID+"/rename", "_csrf="+csrf+"&name=Renamed")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("folder rename = %d body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "/folder?folder="+folder.ID) {
		t.Fatalf("folder rename redirected to %q; want the folder view", loc)
	}
	folders, err := svc.Store.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var renamed bool
	for _, f := range folders {
		if f.ID == folder.ID && f.Name == "Renamed" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("folder not renamed: %+v", folders)
	}
}

// TestToolbarIconButtonsAreUniformWidth guards the toolbar sizing rule. The
// square icon buttons in a mailbox toolbar used to be pinned by a later
// `.bulkbar .icon-btn{width:30px}` rule that had the same specificity as
// `.inboxbar .icon-btn`, so it won on source order and the buttons shrank to
// slivers in the custom-folder view (whose row also carries the Move select and
// button). The rule is gone and the sizing rule now pins the width, a min-width
// and a no-shrink flex basis so Inbox and folder views match.
func TestToolbarIconButtonsAreUniformWidth(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	folder, err := svc.Store.CreateFolder(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	const want = `.inboxbar .icon-btn{height:36px;width:36px;min-width:36px;padding:0;flex:0 0 auto;box-sizing:border-box}`
	for _, path := range []string{"/ui/inboxes/" + box.ID, "/ui/inboxes/" + box.ID + "/folder?folder=" + folder.ID} {
		body := uiGet(t, h, cookie, path).Body.String()
		if !strings.Contains(body, want) {
			t.Fatalf("%s missing the uniform toolbar icon-button rule", path)
		}
		if strings.Contains(body, ".bulkbar .icon-btn") {
			t.Fatalf("%s still ships the conflicting .bulkbar .icon-btn rule", path)
		}
	}
}

// TestSidebarFolderManagementMarkup proves a mailbox that owns physical
// folders keeps the native folder management controls, and that the additive
// label controls stay in their own section.
func TestSidebarFolderManagementMarkup(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.folders["Projects"] = 3
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()

	for _, want := range []string{
		`data-folder-new`,
		`data-folder-manage`,
		`id="folder-new-dialog"`,
		`id="folder-manage-dialog"`,
		`<span>Folders</span>`,
		`<span>Custom folders</span>`,
		// The manage dialog keeps Delete on the same actions row as
		// Cancel/Rename, left-aligned via .dialog-actions>form: the delete
		// form is the row's first child and Rename submits the external
		// rename form through its form attribute.
		`<div class="dialog-actions"><form method="post" action="" data-folder-delete-form>`,
		`<button form="folder-rename-form">Rename</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("folder sidebar missing %q", want)
		}
	}
	// A raw IMAP mailbox has no additive labels, so it offers no label
	// control at all.
	if strings.Contains(page, "New label") {
		t.Fatal("IMAP sidebar still offers a label control")
	}
	if strings.Contains(page, "Manage folders") || strings.Contains(page, "navfolders") {
		t.Fatal("folder sidebar still renders the old Manage folders block")
	}
}

// TestDomainSidebarHasNoFolderControls proves a domain mailbox has system
// folders only: it classifies with labels, so it offers no custom folder
// section and no folder management controls.
func TestDomainSidebarHasNoFolderControls(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := svc.Store.CreateFolder(context.Background(), admin, box.ID, store.FolderCreate{Path: "Projects"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()

	// The system views sit under a Folders heading for every inbox type.
	if !strings.Contains(page, `<span>Folders</span>`) {
		t.Fatal("the sidebar must head the system views with Folders")
	}
	for _, unwanted := range []string{`<span>Custom folders</span>`, `data-folder-new`, `data-folder-manage`, `id="folder-new-dialog"`} {
		if strings.Contains(page, unwanted) {
			t.Fatalf("domain sidebar still offers the folder control %q", unwanted)
		}
	}
	// Its additive labels are still there, under their own heading.
	if !strings.Contains(page, `<span>Labels</span>`) {
		t.Fatal("domain sidebar must list its labels")
	}
}
