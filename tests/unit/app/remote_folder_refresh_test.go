package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// recordingRefresher is a deterministic app.RemoteFolderRefresher that records
// the folder pulls it was asked to schedule, so a test can assert the write hooks
// request the right folder.
type recordingRefresher struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingRefresher) ScheduleRoleFolderRefresh(accountID, inboxID, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, inboxID+":"+role)
}

func (r *recordingRefresher) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// TestPullRoleFolderIndexesAppendedDraft proves the folder-scoped pull reads an
// already-indexed role folder and indexes a message that landed on the server
// after the last deep reconcile, without running a whole-inbox reconcile.
func TestPullRoleFolderIndexesAppendedDraft(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	// Establish the index first: the Drafts folder is cached and complete, so the
	// pull is exercising the genuinely stale case, not a first-time index.
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	if msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "Drafts"); err != nil || len(msgs) != 0 {
		t.Fatalf("Drafts not empty before append: %+v %v", msgs, err)
	}
	// The append lands on the server after the reconcile.
	fake.addMessage("Drafts", "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: placed\r\nMessage-ID: <placed@remote>\r\n\r\nbody", "<placed@remote>", "placed")

	if err := rm.PullRoleFolder(ctx, u.AccountID, box.ID, model.FolderRoleDrafts); err != nil {
		t.Fatalf("pull drafts: %v", err)
	}
	msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "Drafts")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].RFCMessageID != "<placed@remote>" {
		t.Fatalf("drafts folder not pulled: %+v", msgs)
	}
}

// TestPullRoleFolderIndexesSentCopy proves the same folder-scoped pull works for
// the Sent role, and that an unknown role is a no-op.
func TestPullRoleFolderIndexesSentCopy(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	fake.addMessage("Sent", "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: sent\r\nMessage-ID: <sentpull@remote>\r\n\r\nbody", "<sentpull@remote>", "sent")

	if err := rm.PullRoleFolder(ctx, u.AccountID, box.ID, model.FolderRoleSent); err != nil {
		t.Fatalf("pull sent: %v", err)
	}
	msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].RFCMessageID != "<sentpull@remote>" {
		t.Fatalf("sent folder not pulled: %+v", msgs)
	}
	// An unrelated role is ignored rather than treated as a folder pull.
	if err := rm.PullRoleFolder(ctx, u.AccountID, box.ID, model.FolderRoleTrash); err != nil {
		t.Fatalf("unrelated role: %v", err)
	}
}

// TestHandoffPublishRequestsDraftsPull proves a confirmed RemoteDraft handoff asks
// for an immediate Drafts pull, and that an ambiguous handoff does not.
func TestHandoffPublishRequestsDraftsPull(t *testing.T) {
	t.Parallel()
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	rec := &recordingRefresher{}
	svc.RemoteFolderRefresher = rec
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	svc.SetHandoffPublisher(&fakeHandoff{confirmed: true, remoteUID: 3})
	svc.PublishHandoffs(ctx)
	if got := rec.snapshot(); len(got) != 1 || got[0] != box.ID+":"+model.FolderRoleDrafts {
		t.Fatalf("refresh requests after published = %v want [%s:drafts]", got, box.ID)
	}
}

// TestHandoffAmbiguousDoesNotRequestPull proves an ambiguous handoff never asks
// for a Drafts pull: nothing was confirmed on the server, so a pull would be
// wasted work (and would not make an unconfirmed draft appear).
func TestHandoffAmbiguousDoesNotRequestPull(t *testing.T) {
	t.Parallel()
	svc, u, box, _ := remoteTestEnv(t)
	ctx := context.Background()
	rec := &recordingRefresher{}
	svc.RemoteFolderRefresher = rec
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// Unconfirmed append, lookup finds nothing and no other copy exists: the
	// handoff settles ambiguous, never published.
	svc.SetHandoffPublisher(&fakeHandoff{})
	svc.PublishHandoffs(ctx)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("ambiguous handoff requested a pull: %v", got)
	}
}

// TestSentCopyRequestsSentPull proves a confirmed sent copy asks for an immediate
// Sent pull, and a failed copy does not.
func TestSentCopyRequestsSentPull(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	rec := &recordingRefresher{}
	svc.RemoteFolderRefresher = rec
	raw := "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: Sent\r\nMessage-ID: <sentpull-copy@remote>\r\n\r\nbody"
	path := writeFrozenRaw(t, svc, raw)
	if _, err := svc.Store.EnqueueRemoteSentCopy(ctx, u.AccountID, box.ID, store.RemoteSentCopyInput{
		RFCMessageID: "<sentpull-copy@remote>",
		MessageIDHdr: "<sentpull-copy@remote>",
		RawPath:      path,
		SizeBytes:    int64(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
	installFake(t, rm, newFakeRemoteServer())
	rm.CopySentCopies(ctx)
	if got := rec.snapshot(); len(got) != 1 || got[0] != box.ID+":"+model.FolderRoleSent {
		t.Fatalf("refresh requests after sent copy = %v want [%s:sent]", got, box.ID)
	}
}

// TestFailedSentCopyDoesNotRequestPull proves a failed sent copy never asks for a
// Sent pull.
func TestFailedSentCopyDoesNotRequestPull(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	rec := &recordingRefresher{}
	svc.RemoteFolderRefresher = rec
	raw := "From: agent@remote.example\r\nTo: x@y.test\r\nSubject: Sent\r\nMessage-ID: <sentpull-fail@remote>\r\n\r\nbody"
	path := writeFrozenRaw(t, svc, raw)
	if _, err := svc.Store.EnqueueRemoteSentCopy(ctx, u.AccountID, box.ID, store.RemoteSentCopyInput{
		RFCMessageID: "<sentpull-fail@remote>",
		MessageIDHdr: "<sentpull-fail@remote>",
		RawPath:      path,
		SizeBytes:    int64(len(raw)),
	}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeRemoteServer()
	fake.appendFail = model.NewMailboxError(model.ErrKindForbidden, "refused", false, nil)
	installFake(t, rm, fake)
	rm.CopySentCopies(ctx)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("failed sent copy requested a pull: %v", got)
	}
}

// TestHandoffPublishPullsDraftsFolderEndToEnd proves the production wiring: a real
// RemoteDraft append through the installed publisher immediately pulls the Drafts
// folder, so the placed draft is visible without a manual or scheduled reconcile.
func TestHandoffPublishPullsDraftsFolderEndToEnd(t *testing.T) {
	t.Parallel()
	svc, u, box, rm := remoteTestEnv(t)
	ctx := context.Background()
	rm.InstallRemoteBridges()
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	svc.PublishHandoffs(ctx)

	// The pull is scheduled asynchronously; poll the cache with a bounded deadline.
	deadline := time.Now().Add(3 * time.Second)
	for {
		msgs, err := svc.Store.ListRemoteMessages(ctx, u.AccountID, box.ID, "Drafts")
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 1 && msgs[0].Draft {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("confirmed handoff did not pull the Drafts folder: %+v", msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
