package imap_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
)

func TestDiscoverFoldersPersonalRootIncludesInboxAndSiblings(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	// A normal personal account: INBOX plus siblings that are NOT children of
	// INBOX.
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Sent")
	fs.AddMailbox("Drafts")
	fs.AddMailbox("Trash")
	fs.AddMailbox("Archive")
	adapter := dialFake(t, fs, "user@example.com", "secret")

	folders, scope, err := adapter.DiscoverFolders(testContext(t), "")
	if err != nil {
		t.Fatalf("DiscoverFolders: %v", err)
	}
	if !scope.Personal {
		t.Errorf("expected personal scope, got %+v", scope)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f.Path] = true
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts", "Trash", "Archive"} {
		if !got[want] {
			t.Errorf("personal root must include %q; got %v", want, got)
		}
	}
	if !scope.InScope("Sent") {
		t.Error("Sent must be in scope for a personal root")
	}
	if !scope.InScope("INBOX") {
		t.Error("INBOX must be in scope for a personal root")
	}
}

func TestDiscoverFoldersArbitraryRootExactAndChildOnly(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	fs.AddMailbox("Archive")
	fs.AddMailbox("Archive/2026")
	fs.AddMailbox("Archived") // loose prefix must NOT match
	adapter := dialFake(t, fs, "user@example.com", "secret")

	folders, scope, err := adapter.DiscoverFolders(testContext(t), "Archive")
	if err != nil {
		t.Fatalf("DiscoverFolders: %v", err)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f.Path] = true
	}
	if !got["Archive"] || !got["Archive/2026"] {
		t.Errorf("expected Archive and Archive/2026, got %v", got)
	}
	if got["Archived"] {
		t.Error("loose prefix 'Archived' must not be in the Archive scope")
	}
	if got["INBOX"] {
		t.Error("INBOX must not be in an arbitrary Archive scope")
	}
	if !scope.InScope("Archive/2026") {
		t.Error("Archive/2026 must be in scope")
	}
	if scope.InScope("Archived") {
		t.Error("Archived must not be in scope")
	}
}

func TestDiscoverFoldersSpecialUseRoles(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")

	folders, _, err := adapter.DiscoverFolders(testContext(t), "")
	if err != nil {
		t.Fatalf("DiscoverFolders: %v", err)
	}
	if len(folders) == 0 {
		t.Fatal("expected at least INBOX")
	}
	inbox := findFolder(folders, "INBOX")
	if inbox == nil {
		t.Fatal("INBOX not found")
	}
	if inbox.Role != model.FolderRoleInbox {
		t.Errorf("INBOX role = %q, want inbox", inbox.Role)
	}
}

func TestInScopeRejectsSharedNamespace(t *testing.T) {
	scope := imapadapter.RootScope{Root: "Work", Delimiter: '/', Personal: false}
	if scope.InScope("Shared Work") {
		t.Error("a different shared namespace must not be in scope")
	}
	if !scope.InScope("Work") || !scope.InScope("Work/Projects") {
		t.Error("Work and its child must be in scope")
	}
	if scope.InScope("WorkX") {
		t.Error("WorkX is not a delimiter child of Work")
	}
}

func TestRootScopeEmptyPathNeverInScope(t *testing.T) {
	scope := imapadapter.RootScope{Root: "INBOX", Delimiter: '/'}
	if scope.InScope("") {
		t.Error("empty path must never be in scope")
	}
}

func findFolder(folders []imapadapter.RemoteFolder, path string) *imapadapter.RemoteFolder {
	for i := range folders {
		if folders[i].Path == path {
			return &folders[i]
		}
	}
	return nil
}
