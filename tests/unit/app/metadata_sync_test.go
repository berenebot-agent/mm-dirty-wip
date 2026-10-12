package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

func TestMetadataSyncRunningAndRestartSafe(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	initial, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(initial) != 1 || initial[0].State != "waiting" || initial[0].Phase != "initial" {
		t.Fatalf("initial: %+v %v", initial, err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWorker()
	rm.SetRemoteDialer(func(ctx context.Context, _ imap.Config) (app.RemoteSession, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("raw secret provider error must never appear")
	})
	go func() { defer close(finished); _, _ = rm.ReconcileRemote(ctx, u.AccountID, box.ID) }()
	<-started
	active, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(active) != 1 || active[0].State != "running" {
		t.Fatalf("running: %+v %v", active, err)
	}
	// Another process view uses only durable state, not this process's worker.
	restarted := app.NewRemoteMailboxService(svc)
	defer restarted.Stop()
	stale, err := restarted.MetadataSyncStatuses(ctx, p)
	if err != nil || len(stale) != 1 || stale[0].State == "running" {
		t.Fatalf("restart claims active worker: %+v %v", stale, err)
	}
	releaseWorker()
	<-finished
	failed, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(failed) != 1 || failed[0].State != "error" || failed[0].Error != "Metadata sync interrupted; will retry" {
		t.Fatalf("safe error: %+v %v", failed, err)
	}
	for _, principal := range []model.Principal{{AccountID: u.AccountID}, {AccountID: "other-account", Admin: true}} {
		items, err := rm.MetadataSyncStatuses(ctx, principal)
		if err != nil || len(items) != 0 {
			t.Fatalf("status leaked: %+v %v", items, err)
		}
	}
	readable, err := rm.MetadataSyncStatuses(ctx, model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}})
	if err != nil || len(readable) != 1 || readable[0].InboxID != box.ID {
		t.Fatalf("scoped reader lost progress: %+v %v", readable, err)
	}
}

func TestMetadataSyncIMAPCountTotalResumeAndCompletion(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	fake := newFakeRemoteServer()
	for i := 0; i < app.DefaultRemoteReconcileLimit+10; i++ {
		fake.addMessage("INBOX", "body", fmt.Sprintf("<%d@test>", i), "subject")
	}
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	items, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(items) != 1 || items[0].Indexed == 0 || items[0].Total != int64(app.DefaultRemoteReconcileLimit+10) || items[0].Indexed >= items[0].Total || items[0].State != "waiting" {
		t.Fatalf("partial: %+v %v", items, err)
	}
	restarted := app.NewRemoteMailboxService(svc)
	defer restarted.Stop()
	resumed, err := restarted.MetadataSyncStatuses(ctx, p)
	if err != nil || len(resumed) != 1 || resumed[0].Indexed != items[0].Indexed || resumed[0].Total != items[0].Total {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	done, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(done) != 0 {
		t.Fatalf("completed remains visible: %+v %v", done, err)
	}
	if err := svc.Store.SaveMetadataSyncState(ctx, u.AccountID, box.ID, "paused", "initial", "Metadata storage limit reached"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetRemoteIndexStatus(ctx, u.AccountID, box.ID, store.RemoteIndexPartial, ""); err != nil {
		t.Fatal(err)
	}
	paused, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(paused) != 1 || paused[0].State != "paused" {
		t.Fatalf("paused: %+v %v", paused, err)
	}
}

func TestMetadataSyncOneCountsUniqueMessagesAndUnknownTotal(t *testing.T) {
	svc, u, _, rm := remoteTestEnv(t)
	f := onefixture.New(t)
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	ctx := context.Background()
	setup, err := rm.PrepareOneImport(ctx, p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	results, err := rm.ImportOne(ctx, p, setup, []string{onefixture.ConnectionID}, nil)
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("import: %+v %v", results, err)
	}
	box := results[0].InboxID
	if _, err := rm.ReconcileRemote(ctx, u.AccountID, box); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetRemoteIndexStatus(ctx, u.AccountID, box, store.RemoteIndexPartial, ""); err != nil {
		t.Fatal(err)
	}
	items, err := rm.MetadataSyncStatuses(ctx, p)
	if err != nil || len(items) != 1 || items[0].Provider != "One" || items[0].Indexed != 1 || items[0].Total != 0 {
		t.Fatalf("one count: %+v %v", items, err)
	}
}

func TestMetadataSyncNativeGoogleCountsAndCompletion(t *testing.T) {
	svc, box, rm, f := googleLogEnv(t)
	ctx := context.Background()
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	f.add("native-google-message")
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.Store.MetadataSyncSnapshot(ctx, box.AccountID, box.ID)
	if err != nil || snapshot.Indexed != 1 || snapshot.State != "done" {
		t.Fatalf("native Gmail snapshot: %+v %v", snapshot, err)
	}
	if err := svc.Store.SetRemoteIndexStatus(ctx, box.AccountID, box.ID, store.RemoteIndexPartial, ""); err != nil {
		t.Fatal(err)
	}
	items, err := rm.MetadataSyncStatuses(ctx, model.Principal{AccountID: box.AccountID, Admin: true})
	if err != nil || len(items) != 1 || items[0].Provider != "Gmail" || items[0].Indexed != 1 || items[0].Total != 0 {
		t.Fatalf("native Gmail status: %+v %v", items, err)
	}
}

func TestMetadataSyncMicrosoftTotalAndCompletion(t *testing.T) {
	svc, box, rm, f := microsoftLogEnv(t)
	ctx := context.Background()
	f.add("ms-metadata-one", "inbox-id", "one")
	f.add("ms-metadata-two", "inbox-id", "two")
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.Store.MetadataSyncSnapshot(ctx, box.AccountID, box.ID)
	if err != nil || snapshot.Indexed != 2 || snapshot.Total != 2 || snapshot.State != "done" {
		t.Fatalf("Microsoft totals: %+v %v", snapshot, err)
	}
	items, err := rm.MetadataSyncStatuses(ctx, model.Principal{AccountID: box.AccountID, Admin: true})
	if err != nil || len(items) != 0 {
		t.Fatalf("Microsoft completion remains: %+v %v", items, err)
	}
	if err := svc.Store.SetRemoteIndexStatus(ctx, box.AccountID, box.ID, store.RemoteIndexPartial, ""); err != nil {
		t.Fatal(err)
	}
	items, err = rm.MetadataSyncStatuses(ctx, model.Principal{AccountID: box.AccountID, Admin: true})
	if err != nil || len(items) != 1 || items[0].Provider != "Outlook" {
		t.Fatalf("Microsoft status: %+v %v", items, err)
	}
}

func TestMetadataSyncQueuedAndCoalesced(t *testing.T) {
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "pw", "")
	ctx := context.Background()
	boxes := []model.Inbox{box}
	for i := 0; i < 2; i++ {
		b, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: fmt.Sprintf("queued%d@example.test", i), Remote: &model.RemoteConnection{Host: "imap.example.test", Username: "fixture", Security: model.RemoteSecurityPlain, Port: 143}})
		if err != nil {
			t.Fatal(err)
		}
		configureSecrets(t, rm, u, b, "pw", "")
		boxes = append(boxes, b)
	}
	started, release := make(chan struct{}, 3), make(chan struct{})
	defer close(release)
	rm.SetRemoteDialer(func(ctx context.Context, _ imap.Config) (app.RemoteSession, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	})
	for _, b := range boxes {
		rm.ScheduleRefresh(u.AccountID, b.ID)
		rm.ScheduleRefresh(u.AccountID, b.ID)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("queued work never started")
		}
	}
	items, err := rm.MetadataSyncStatuses(ctx, model.Principal{AccountID: u.AccountID, Admin: true})
	if err != nil || len(items) != 3 {
		t.Fatalf("queue: %+v %v", items, err)
	}
	running, queued := 0, 0
	for _, item := range items {
		if item.State == "running" {
			running++
		}
		if item.State == "queued" {
			queued++
		}
	}
	if running != 2 || queued != 1 {
		t.Fatalf("slots/coalescing: %+v", items)
	}
	rm.Stop()
}
