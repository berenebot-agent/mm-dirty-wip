package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// TestRemoteDeleteFolderAlreadyGonePrunesRow proves a delete whose provider
// answer is "no such folder" (a retry after the first attempt landed, or an
// answer lost to a timeout) finishes locally instead of leaving a phantom
// folder that makes its parent report children it no longer has.
func TestRemoteDeleteFolderAlreadyGonePrunesRow(t *testing.T) {
	t.Parallel()
	_, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Parent", 100)
	fake.addFolder("Parent/Child", 100)
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	parentID, childID := folderIDs(t, rm, u.AccountID, box.ID, "Parent", "Parent/Child")

	// The child is already gone at the provider but still cached: the first
	// attempt's provider delete landed while its answer never reached us.
	fake.deleteFolder("Parent/Child")
	if err := rm.DeleteRemoteFolder(ctx, p, box.ID, childID); err != nil {
		t.Fatalf("delete of a folder already gone at the provider: %v", err)
	}
	if _, _, err := findFolder(rm, u.AccountID, box.ID, childID); !errors.Is(err, errNoFolder) {
		t.Fatal("the cached child row survived a confirmed provider delete")
	}
	// The parent is therefore deletable through the API without a manual refresh.
	if err := rm.DeleteRemoteFolder(ctx, p, box.ID, parentID); err != nil {
		t.Fatalf("delete parent after the child converged: %v", err)
	}
}

// TestRemoteDeleteFolderSurvivesCanceledRequest proves the local prune after a
// confirmed provider delete is not tied to the request that issued it: a client
// that gave up (or a gateway that timed out on the client side) must not leave
// the cached row behind.
func TestRemoteDeleteFolderSurvivesCanceledRequest(t *testing.T) {
	t.Parallel()
	_, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Gone", 100)
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(context.Background(), u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	_, folderID := folderIDs(t, rm, u.AccountID, box.ID, "", "Gone")

	ctx, cancel := context.WithCancel(context.Background())
	// The provider delete has landed; the request dies before the caller reads
	// the answer, exactly as a gateway timeout on the client side looks.
	fake.beforeDelete = cancel
	err := rm.DeleteRemoteFolder(ctx, p, box.ID, folderID)
	if err != nil {
		// The delete may be reported as failed; what matters is that it converged.
		t.Logf("delete reported: %v", err)
	}
	if _, _, ferr := findFolder(rm, u.AccountID, box.ID, folderID); ferr == nil {
		t.Fatal("the cached row survived because the request context was canceled")
	}
}

// folderIDs resolves folder ids by path; an empty path is skipped.
func folderIDs(t *testing.T, rm *app.RemoteMailboxService, accountID, inboxID, parent, child string) (string, string) {
	t.Helper()
	out := make([]string, 2)
	for i, path := range []string{parent, child} {
		if path == "" {
			continue
		}
		f, _, err := findFolder(rm, accountID, inboxID, path, true)
		if err != nil {
			t.Fatalf("folder %q: %v", path, err)
		}
		out[i] = f.ID
	}
	return out[0], out[1]
}

// findFolder resolves one folder by id (byPath false) or by path (byPath true).
var errNoFolder = errors.New("folder not in the cached tree")

func findFolder(rm *app.RemoteMailboxService, accountID, inboxID string, want string, byPath ...bool) (model.Folder, bool, error) {
	folders, err := rm.Service.Store.ListFolders(context.Background(), accountID, inboxID)
	if err != nil {
		return model.Folder{}, false, err
	}
	matchPath := len(byPath) > 0 && byPath[0]
	for _, f := range folders {
		if (matchPath && f.Path == want) || (!matchPath && f.ID == want) {
			return f, true, nil
		}
	}
	return model.Folder{}, false, errNoFolder
}
