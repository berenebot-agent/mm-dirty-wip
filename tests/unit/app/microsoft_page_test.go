package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// deltaGate is an http.RoundTripper that holds the first Microsoft Graph delta
// request in flight until the test releases it. It makes the resync/sweep
// overlap deterministic: the page has been fetched but not yet applied when the
// test bumps the folder's generation.
type deltaGate struct {
	base    http.RoundTripper
	entered chan string
	release chan struct{}
	once    sync.Once
}

func (g *deltaGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/messages/delta") {
		blocked := false
		g.once.Do(func() { blocked = true })
		if blocked {
			g.entered <- r.URL.Path
			<-g.release
		}
	}
	return g.base.RoundTrip(r)
}

// TestMicrosoftDeltaResyncOverlapDropsStalePage proves that a resync which lands
// while a delta page is in flight discards the WHOLE page: the in-flight page's
// metadata and arrival are not written, the folder's freshly reset cursor is not
// overwritten, and the pass reports the index incomplete instead of complete. The
// follow-up pass then rebuilds the folder from the reset baseline.
func TestMicrosoftDeltaResyncOverlapDropsStalePage(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := microsoftLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	// Baseline: an empty folder establishes the cursor and the initialised sweep
	// without emitting a historical arrival. The gate is installed afterwards so
	// it blocks only the page the resync will overlap.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	if cursor, initialized, _, err := svc.Store.MicrosoftDelta(ctx, box.ID, "inbox-id"); err != nil || cursor == "" || !initialized {
		t.Fatalf("baseline delta = %q initialized=%v (%v)", cursor, initialized, err)
	}
	gate := &deltaGate{base: rm.Microsoft.HTTP.Transport, entered: make(chan string, 1), release: make(chan struct{})}
	rm.Microsoft.HTTP = &http.Client{Transport: gate}

	// The next page carries a genuinely new arrival, but the fetch is held open
	// while a resync resets the folder underneath it.
	fixture.add("VictimID", "inbox-id", "Victim")
	done := make(chan error, 1)
	go func() {
		_, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID)
		done <- err
	}()
	<-gate.entered
	if err := svc.Store.StartMicrosoftResync(ctx, box.ID, "inbox-id"); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("overlapped reconcile: %v", err)
	}

	// Nothing from the superseded page landed.
	if _, err := svc.Store.MicrosoftLocalID(ctx, box.AccountID, box.ID, "VictimID"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale page cached metadata: %v", err)
	}
	rows, err := svc.Store.ListRemoteMessages(ctx, box.AccountID, box.ID, "inbox-id")
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale page left cached rows: %+v (%v)", rows, err)
	}
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 0 {
		t.Fatalf("stale page recorded an arrival: %+v (%v)", arrivals, err)
	}
	// The resync's reset cursor is intact: the stale page did not overwrite it.
	cursor, initialized, gen, err := svc.Store.MicrosoftDelta(ctx, box.ID, "inbox-id")
	if err != nil || cursor != "" || initialized || gen == "" {
		t.Fatalf("stale page overwrote the reset delta row: cursor=%q initialized=%v gen=%q (%v)", cursor, initialized, gen, err)
	}
	// And the pass must not claim a complete index it has not built.
	status, err := svc.Store.GetRemoteIndexStatus(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status == store.RemoteIndexComplete {
		t.Fatal("stale page reported the index complete")
	}
	last := sink.syncRecords()
	if len(last) == 0 || last[len(last)-1]["new"] != "0" || last[len(last)-1]["status"] != "partial" {
		t.Fatalf("stale pass log = %#v, want new=0 status=partial", last)
	}

	// The follow-up pass rebuilds from the reset baseline: the message is cached
	// as a baseline arrival-less row and the folder is complete again.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.Store.ListRemoteMessages(ctx, box.AccountID, box.ID, "microsoft:VictimID")
	if err != nil || len(rows) != 1 || rows[0].Subject != "Victim" {
		t.Fatalf("follow-up pass did not rebuild the folder: %+v (%v)", rows, err)
	}
	arrivals, err = svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 0 {
		t.Fatalf("baseline rebuild emitted a historical arrival: %+v (%v)", arrivals, err)
	}
	status, err = svc.Store.GetRemoteIndexStatus(ctx, box.AccountID, box.ID)
	if err != nil || status.Status != store.RemoteIndexComplete {
		t.Fatalf("follow-up status = %q (%v), want complete", status.Status, err)
	}
	if c, initialized, _, _ := svc.Store.MicrosoftDelta(ctx, box.ID, "inbox-id"); c == "" || !initialized {
		t.Fatalf("follow-up did not re-establish the cursor: %q initialized=%v", c, initialized)
	}
}

// TestMicrosoftDeltaPageArrivalIsPersistedAtomically proves the ordinary
// (non-overlapping) pass still records the arrival, caches the metadata with the
// message's labels and reports the index complete: the guarded page transaction
// is a safety boundary, not a second effect path.
func TestMicrosoftDeltaPageArrivalIsPersistedAtomically(t *testing.T) {
	t.Parallel()
	svc, box, rm, fixture := microsoftLogEnv(t)
	ctx := context.Background()
	sink := newSyncLogSink()
	svc.Log = slog.New(sink)

	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	fixture.add("FreshID", "inbox-id", "Fresh")
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}

	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 1 || arrivals[0].FolderPath != "microsoft:FreshID" {
		t.Fatalf("arrival not persisted: %+v (%v)", arrivals, err)
	}
	id, err := svc.Store.MicrosoftLocalID(ctx, box.AccountID, box.ID, "FreshID")
	if err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetRemoteMessage(ctx, box.AccountID, box.ID, id)
	if err != nil || row.Subject != "Fresh" {
		t.Fatalf("metadata not cached with the arrival: %+v (%v)", row, err)
	}
	status, err := svc.Store.GetRemoteIndexStatus(ctx, box.AccountID, box.ID)
	if err != nil || status.Status != store.RemoteIndexComplete {
		t.Fatalf("status = %q (%v), want complete", status.Status, err)
	}
	recs := sink.syncRecords()
	if len(recs) != 2 || recs[1]["new"] != "1" || recs[1]["status"] != "complete" {
		t.Fatalf("arrival pass log = %#v, want new=1 complete", recs)
	}
	// A quiet repeat re-detects nothing: the arrival and its detected mark
	// committed together, so a replayed page emits no second event.
	if _, err := rm.ReconcileRemote(ctx, box.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	arrivals, _ = svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 100)
	if len(arrivals) != 1 {
		t.Fatalf("repeat pass duplicated the arrival: %+v", arrivals)
	}
}
