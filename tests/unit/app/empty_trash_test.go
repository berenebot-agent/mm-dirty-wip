package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
)

// The first destructive call adds a new message AFTER discovery. It must survive
// the whole operation, even though it is in the provider's Trash before we finish.
type trashSnapshotSession struct {
	*fakeRemoteServer
	once sync.Once
}

func (s *trashSnapshotSession) DeleteMessage(ctx context.Context, loc imap.Locator) error {
	s.once.Do(func() { s.addMessage("Trash", "late", "<late@test>", "Late") })
	return s.fakeRemoteServer.DeleteMessage(ctx, loc)
}

func TestEmptyTrashDrainsUncachedIMAPAndFreezesTargets(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Trash", 100)
	installFake(t, rm, fake)
	ctx := context.Background()
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	// Add only after indexing: NONE of these targets has a cached metadata row.
	for i := 0; i < 501; i++ {
		fake.addMessage("Trash", "body", fmt.Sprintf("<trash-%d@test>", i), "Trash")
	}
	session := &trashSnapshotSession{fakeRemoteServer: fake}
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return session, nil })
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	a, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil || again.ID != a.ID {
		t.Fatalf("duplicate submission: %+v %v", again, err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	defer w.Stop()
	got, err := svc.WaitForInboxAction(ctx, p, a.ID, 20*time.Second)
	if err != nil || got.State != "done" || got.EmptyTrash == nil || got.EmptyTrash.Purged != 501 {
		t.Fatalf("operation: %+v %v", got, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deleteCalled != 501 || len(fake.messages) != 1 || fake.messages[0].messageID != "<late@test>" {
		t.Fatalf("snapshot purge: deleted=%d remaining=%+v", fake.deleteCalled, fake.messages)
	}
}

func TestEmptyTrashDiscoveryAndChildrenRecoverWithoutReplay(t *testing.T) {
	t.Parallel()
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	a, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.Store.ClaimMessageAction(ctx, "worker"); err != nil || !ok {
		t.Fatalf("claim parent: %v %v", ok, err)
	}
	if err := svc.Store.SaveTrashDiscovery(ctx, a.ID, "Trash", "next", 100, []store.TrashTarget{{Kind: "imap", ID: "1", Folder: "Trash", UID: 1, UIDValidity: 100}}, false); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 0 {
		t.Fatalf("purge queued before discovery finished: %d %v", n, err)
	}
	if err := svc.Store.RecoverMessageActions(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Store.GetMessageAction(ctx, p, a.ID)
	if err != nil || got.State != "pending" || got.EmptyTrash.Total != 1 {
		t.Fatalf("discovery recovery: %+v %v", got, err)
	}
	// Replaying the page must not duplicate its target.
	if err := svc.Store.SaveTrashDiscovery(ctx, a.ID, "Trash", "", 100, []store.TrashTarget{{Kind: "imap", ID: "1", Folder: "Trash", UID: 1, UIDValidity: 100}}, true); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 1 {
		t.Fatalf("queue children: %d %v", n, err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 0 {
		t.Fatalf("duplicate children: %d %v", n, err)
	}
	if err := svc.Store.SettleMessageAction(ctx, a.ID, "pending", "", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	child, ok, err := svc.Store.ClaimMessageAction(ctx, "worker")
	if err != nil || !ok || child.Input.TrashTarget == nil {
		t.Fatalf("claim child: %+v %v %v", child, ok, err)
	}
	if err := svc.Store.RecoverMessageActions(ctx); err != nil {
		t.Fatal(err)
	}
	child, err = svc.Store.GetMessageAction(ctx, p, child.ID)
	if err != nil || child.State != "ambiguous" {
		t.Fatalf("interrupted purge replayed: %+v %v", child, err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 0 {
		t.Fatalf("uncertain child re-enqueued: %d %v", n, err)
	}
	other := model.Principal{AccountID: "other", Admin: true}
	if _, err := svc.Store.GetMessageAction(ctx, other, a.ID); err != store.ErrNotFound {
		t.Fatalf("cross-account operation: %v", err)
	}
	reader := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, err := svc.Store.EnqueueEmptyTrash(ctx, reader, box.ID); err != store.ErrForbidden {
		t.Fatalf("read-only Empty Trash: %v", err)
	}
}

func TestEmptyTrashQueueBoundsAndProgress(t *testing.T) {
	t.Parallel()
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	a, err := svc.Store.EnqueueEmptyTrash(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targets []store.TrashTarget
	for i := 0; i < 501; i++ {
		targets = append(targets, store.TrashTarget{Kind: "local", ID: fmt.Sprint(i)})
	}
	if err := svc.Store.SaveTrashDiscovery(ctx, a.ID, "", "", 0, targets, true); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 100 {
		t.Fatalf("first batch: %d %v", n, err)
	}
	if n, err := svc.Store.QueueTrashBatch(ctx, a); err != nil || n != 0 {
		t.Fatalf("unbounded outstanding children: %d %v", n, err)
	}
	progress, err := svc.Store.EmptyTrashProgress(ctx, a.ID)
	if err != nil || progress.Total != 501 || progress.Queued != 100 || progress.Purged != 0 {
		t.Fatalf("progress: %+v %v", progress, err)
	}
}

func TestEmptyTrashGoogleExplicitlyUnsupported(t *testing.T) {
	t.Parallel()
	svc, box, rm, _ := googleLogEnv(t)
	p := model.Principal{AccountID: box.AccountID, Admin: true}
	a, err := svc.Store.EnqueueEmptyTrash(context.Background(), p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	defer w.Stop()
	got, err := svc.WaitForInboxAction(context.Background(), p, a.ID, 5*time.Second)
	if err != nil || got.State != "failed" || got.LastError != "Permanent deletion is not enabled for Google inboxes; use Gmail" {
		t.Fatalf("unsupported operation: %+v %v", got, err)
	}
}

func TestEmptyTrashMicrosoftEnumeratesBeforeDeleting(t *testing.T) {
	t.Parallel()
	svc, box, rm, _ := microsoftLogEnv(t)
	ctx := context.Background()
	// The inbox's seeded system Trash is the role's address; map it to the
	// provider's folder id so discovery has a path to walk.
	folders, err := svc.Store.EnsureSystemFolders(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	trashPath := ""
	for _, f := range folders {
		if f.Role == model.FolderRoleTrash {
			trashPath = f.Path
		}
	}
	if trashPath == "" {
		t.Fatal("no seeded Trash folder")
	}
	if _, err := svc.Store.ReconcileRemoteFolders(ctx, box.AccountID, box.ID, []store.RemoteFolderSync{{Path: trashPath, Name: "Deleted Items", Role: model.FolderRoleTrash, Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	remaining := map[string]bool{}
	for i := 0; i < 101; i++ {
		remaining[fmt.Sprint(i)] = true
	}
	discovered, deletes := false, 0
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, trashPath+"/messages"):
			if deletes != 0 {
				t.Error("enumeration continued after deletion started")
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("skip"))
			end := start + 100
			if end > 101 {
				end = 101
			}
			var messages []map[string]any
			for i := start; i < end; i++ {
				messages = append(messages, map[string]any{"id": fmt.Sprint(i), "parentFolderId": trashPath})
			}
			page := map[string]any{"value": messages}
			if end < 101 {
				page["@odata.nextLink"] = baseURL + r.URL.Path + "?skip=" + fmt.Sprint(end)
			} else {
				discovered = true
			}
			json.NewEncoder(w).Encode(page)
		case strings.HasSuffix(r.URL.Path, "/permanentDelete"):
			if !discovered {
				t.Error("purge started before the final enumeration page")
			}
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-2]
			if !remaining[id] {
				t.Errorf("duplicate purge of %s", id)
			}
			delete(remaining, id)
			remaining["late"] = true
			deletes++
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/messages/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			json.NewEncoder(w).Encode(map[string]any{"id": id, "parentFolderId": trashPath})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL
	rm.Microsoft.BaseURL, rm.Microsoft.HTTP = srv.URL, srv.Client()
	p := model.Principal{AccountID: box.AccountID, Admin: true}
	a, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	defer w.Stop()
	got, err := svc.WaitForInboxAction(ctx, p, a.ID, 20*time.Second)
	if err != nil || got.State != "done" || got.EmptyTrash.Purged != 101 {
		t.Fatalf("Microsoft Empty Trash: %+v %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 101 || len(remaining) != 1 || !remaining["late"] {
		t.Fatalf("deleted=%d remaining=%v", deletes, remaining)
	}
}
