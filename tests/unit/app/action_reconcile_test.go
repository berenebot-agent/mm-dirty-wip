package app_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/imap"
)

// lostResponseSession performs the purge at the server, then reports a
// retryable failure, as when the connection drops before the reply arrives.
type lostResponseSession struct {
	*fakeRemoteServer
	apply        bool
	down         bool
	deleteCalled atomic.Int32
}

func (s *lostResponseSession) DeleteMessage(ctx context.Context, loc imap.Locator) error {
	s.deleteCalled.Add(1)
	if s.apply {
		_ = s.fakeRemoteServer.DeleteMessage(ctx, loc)
	}
	return model.NewMailboxError(model.ErrKindUnavailable, "connection lost", true, nil)
}

func (s *lostResponseSession) FetchHeader(ctx context.Context, loc imap.Locator) (imap.MessageHeader, error) {
	if s.down {
		return imap.MessageHeader{}, model.NewMailboxError(model.ErrKindUnavailable, "connection lost", true, nil)
	}
	return s.fakeRemoteServer.FetchHeader(ctx, loc)
}

func ambiguousTrashEnv(t *testing.T, apply bool) (*app.Service, *app.ActionWorker, *lostResponseSession, model.Principal, store.MessageAction) {
	t.Helper()
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Trash", 100)
	installFake(t, rm, fake)
	ctx := context.Background()
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	fake.addMessage("Trash", "body", "<gone@test>", "Gone")
	session := &lostResponseSession{fakeRemoteServer: fake, apply: apply}
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return session, nil })
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	a, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := app.NewActionWorker(svc, rm)
	w.Start()
	t.Cleanup(w.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := svc.Store.GetMessageAction(ctx, p, a.ID)
		if err == nil && got.State == "ambiguous" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation never became uncertain: %+v %v", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return svc, w, session, p, a
}

func makeAmbiguousDue(t *testing.T, svc *app.Service, p model.Principal, parent string) {
	t.Helper()
	ctx := context.Background()
	acts, err := svc.Store.ListMessageActions(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if a.State == "ambiguous" {
			if err := svc.Store.DeferAmbiguousRecheck(ctx, a.ID, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A purge that took effect at the server but lost its reply settles on its own,
// and the Empty Trash parent finishes without anyone verifying it by hand.
func TestRecheckAmbiguousPurgeAppliedAtServerCompletes(t *testing.T) {
	t.Parallel()
	svc, w, _, p, parent := ambiguousTrashEnv(t, true)
	makeAmbiguousDue(t, svc, p, parent.ID)
	w.RecheckFailed()
	got, err := svc.WaitForInboxAction(context.Background(), p, parent.ID, 10*time.Second)
	if err != nil || got.State != "done" || got.EmptyTrash == nil || got.EmptyTrash.Purged != 1 {
		t.Fatalf("applied purge not reconciled: %+v %v", got, err)
	}
}

// A purge that did not take effect is re-run by its exact UID, never skipped.
func TestRecheckAmbiguousPurgeStillPresentIsRetried(t *testing.T) {
	t.Parallel()
	svc, w, session, p, parent := ambiguousTrashEnv(t, false)
	makeAmbiguousDue(t, svc, p, parent.ID)
	w.RecheckFailed()
	deadline := time.Now().Add(10 * time.Second)
	for {
		calls := session.deleteCalled.Load()
		if calls == 0 && time.Now().After(deadline) {
			t.Fatal("present message was never retried")
		}
		got, _ := svc.Store.GetMessageAction(context.Background(), p, parent.ID)
		if got.State != "ambiguous" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	session.fakeRemoteServer.mu.Lock()
	defer session.fakeRemoteServer.mu.Unlock()
	if len(session.fakeRemoteServer.messages) != 1 {
		t.Fatalf("message must still exist: %+v", session.fakeRemoteServer.messages)
	}
}

// An unreachable provider decides nothing: the action stays uncertain and no
// destructive call is made.
func TestRecheckAmbiguousProviderDownDecidesNothing(t *testing.T) {
	t.Parallel()
	svc, w, session, p, parent := ambiguousTrashEnv(t, true)
	session.down = true
	makeAmbiguousDue(t, svc, p, parent.ID)
	session.fakeRemoteServer.mu.Lock()
	before := session.fakeRemoteServer.deleteCalled
	session.fakeRemoteServer.mu.Unlock()
	w.RecheckFailed()
	got, err := svc.Store.GetMessageAction(context.Background(), p, parent.ID)
	if err != nil || got.State != "ambiguous" {
		t.Fatalf("must stay uncertain: %+v %v", got, err)
	}
	session.fakeRemoteServer.mu.Lock()
	defer session.fakeRemoteServer.mu.Unlock()
	if session.fakeRemoteServer.deleteCalled != before {
		t.Fatalf("destructive call made while provider unreachable")
	}
}
