package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

// RemoteForwarder fetches a detected remote arrival's raw RFC5322 MIME to a
// transient temp file for a demand-based webhook/Hermes forward. It is the narrow
// seam the webhook and relay workers use so they never speak IMAP and never
// archive a body. The returned path must be removed with CleanupRemoteRaw.
type RemoteForwarder interface {
	FetchArrivalRaw(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) (path string, size int64, err error)
	CleanupRemoteRaw(path string)
}

// RemoteDetection is the on-demand detection surface the HTTP agent drives when a
// client polls for events. It forces a detection pass for one inbox (so a new
// arrival is durably recorded and published without waiting for an IDLE signal)
// and reports how many approvals are pending. It is installed once at startup by
// cmd/server onto the Service.
type RemoteDetection interface {
	// DetectInbox runs one remote-arrival detection pass for an inbox, recording
	// and publishing any genuinely new arrivals. It is safe to call concurrently
	// and is a no-op for a non-standalone or unconfigured inbox.
	DetectInbox(ctx context.Context, inbox model.Inbox)
	// PendingRemoteApprovals reports whether an inbox has an outstanding pending
	// email approval request, so a control message forces detection.
	PendingRemoteApprovals(ctx context.Context, inbox model.Inbox) bool
}

// FetchArrivalRaw implements RemoteForwarder. It streams the arrival's raw MIME to
// a transient file and never marks the message seen.
func (m *RemoteMailboxService) FetchArrivalRaw(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) (string, int64, error) {
	return m.fetchArrivalRawToTemp(ctx, inbox, arrival)
}

// RemoteWorker is the process-owned standalone-inbox watcher. For every
// remote-configured standalone inbox it runs a bounded, dedicated observation of
// the inbox's selected INBOX folder:
//
//   - It prefers a realtime IDLE watch on a dedicated connection (the adapter's
//     Watch), and falls back to bounded polling (the adapter's Poll) when the
//     server does not advertise IDLE or the watch cannot be established. Either
//     way it uses its own connection, so a slow/blocking IDLE never starves the
//     other inboxes.
//   - On every signal it re-runs the durable detection pass: it reads the live UID
//     set, compares it against the inbox's durable per-folder cursor, records
//     exactly one durable arrival per genuinely new message, and advances the
//     cursor. The first ever pass establishes the baseline WITHOUT emitting any
//     arrival, so enabling the watcher on an existing mailbox never floods the
//     event stream with old mail.
//   - A connection failure is a retry, never a skip: the cursor is not advanced
//     past unrecorded mail, so nothing is lost across a reconnect.
//
// The watcher owns only remote *events*; it never archives a body and never
// advances a live-read cursor. Proactive webhook/Hermes delivery is demand-based:
// an arrival is only fanned out to clients the inbox actually has.
//
// All state lives in the durable store, so a restart resumes from the cursor. The
// worker holds no package-level global; it is created once by cmd/server and
// stopped on shutdown.
type RemoteWorker struct {
	svc    *Service
	remote *RemoteMailboxService

	log    *slog.Logger
	stop   chan struct{}
	done   chan struct{}
	period time.Duration

	// maxInboxConnections bounds the number of inboxes watched concurrently. Each
	// watched inbox holds at most one dedicated connection, so this caps the
	// watcher's connection and memory footprint.
	maxInboxConnections int
	// pollInterval is the bounded fallback poll cadence when IDLE is unavailable.
	pollInterval time.Duration
	// watchBackoff bounds reconnect attempts after a watch/connection failure.
	watchBackoff time.Duration
	detectionMu  sync.Mutex
	detecting    map[string]bool
}

// Defaults for the remote watcher.
const (
	defaultRemoteWatchPeriod    = 30 * time.Second
	defaultRemotePollInterval   = 60 * time.Second
	defaultRemoteWatchBackoff   = 5 * time.Second
	defaultRemoteMaxConnections = 8
	// remoteArrivalPageLimit bounds how many header rows a single detection pass
	// fetches, so a huge folder cannot exhaust memory; a folder larger than this
	// is simply caught up over subsequent passes.
	remoteArrivalPageLimit = 500
	// remoteArrivalLease bounds a proactive fan-out claim.
	remoteArrivalLease = 2 * time.Minute
)

// NewRemoteWorker builds the watcher over the app service and its remote surface.
func NewRemoteWorker(svc *Service, rm *RemoteMailboxService, log *slog.Logger) *RemoteWorker {
	if log == nil {
		log = slog.Default()
	}
	return &RemoteWorker{
		svc:                 svc,
		remote:              rm,
		log:                 log,
		stop:                make(chan struct{}),
		done:                make(chan struct{}),
		period:              defaultRemoteWatchPeriod,
		maxInboxConnections: defaultRemoteMaxConnections,
		pollInterval:        defaultRemotePollInterval,
		watchBackoff:        defaultRemoteWatchBackoff,
		detecting:           make(map[string]bool),
	}
}

// SetPeriod overrides the reconciling poll period (used by tests).
func (w *RemoteWorker) SetPeriod(d time.Duration) { w.period = d }

// SetPollInterval overrides the fallback poll cadence when IDLE is unavailable.
func (w *RemoteWorker) SetPollInterval(d time.Duration) { w.pollInterval = d }

// SetMaxInboxConnections bounds how many inboxes are watched concurrently.
func (w *RemoteWorker) SetMaxInboxConnections(n int) {
	if n > 0 {
		w.maxInboxConnections = n
	}
}

// Start launches the worker. It returns immediately.
func (w *RemoteWorker) Start() { go w.run() }

// Stop signals the worker to stop and waits for every watcher goroutine to drain.
// In-flight passes observe the stop signal between messages so shutdown is prompt
// and no goroutine is leaked.
func (w *RemoteWorker) Stop() {
	close(w.stop)
	<-w.done
}

func (w *RemoteWorker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// run reconciles the set of watchers against the configured standalone inboxes on
// each tick, and drains them all on stop. A watcher is a long-lived goroutine plus
// its dedicated connection; the reconcile pass starts one for each new inbox and
// cancels one for each removed/disabled inbox.
func (w *RemoteWorker) run() {
	defer close(w.done)
	// Recover claims left by a previous process so arrivals resume.
	if err := w.svc.Store.RecoverAbandonedRemoteArrivalClaims(context.Background()); err != nil {
		w.log.Error("remote arrival claim recovery", "error", err)
	}
	// Recover abandoned sent-copy claims so a copy interrupted by a restart
	// resumes instead of idling until its lease expires.
	if err := w.svc.Store.RecoverAbandonedRemoteSentCopyClaims(context.Background()); err != nil {
		w.log.Error("remote sent-copy claim recovery", "error", err)
	}

	type watcher struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	active := map[string]*watcher{}

	reconcile := func() {
		inboxes, err := w.svc.Store.ListDetectableRemoteInboxes(context.Background())
		if err != nil {
			w.log.Error("remote watcher reconcile", "error", err)
			return
		}
		want := map[string]model.Inbox{}
		for _, ib := range inboxes {
			want[ib.ID] = ib
			if status, err := w.svc.Store.GetRemoteIndexStatus(context.Background(), ib.AccountID, ib.ID); err == nil && (status.Status != store.RemoteIndexComplete || time.Since(status.IndexedAt) > time.Minute) {
				w.remote.ScheduleRefresh(ib.AccountID, ib.ID)
			}
		}
		// Stop watchers for inboxes that are gone or disabled.
		for id, wt := range active {
			if _, ok := want[id]; !ok {
				wt.cancel()
				delete(active, id)
			}
		}
		// Start watchers for new inboxes, bounded by maxInboxConnections.
		for id, ib := range want {
			if _, ok := active[id]; ok {
				continue
			}
			if len(active) >= w.maxInboxConnections {
				break
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			active[id] = &watcher{cancel: cancel, done: done}
			go func(ib model.Inbox) {
				defer close(done)
				w.watchInbox(ctx, ib)
			}(ib)
		}
	}

	// On startup, drain any arrivals left pending by a previous process, then
	// reconcile and run one full detection pass over every inbox so a fresh start
	// catches up immediately.
	w.drainRemoteArrivals()
	w.reconcileRemoteActions()
	reconcile()
	w.detectAll()

	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			for _, wt := range active {
				wt.cancel()
			}
			for _, wt := range active {
				<-wt.done
			}
			return
		case <-ticker.C:
			reconcile()
			w.drainRemoteArrivals()
			w.reconcileRemoteActions()
			w.detectPendingApprovalInboxes()
			// Drive the durable sent-copy queue (copies of sent mail into a
			// standalone inbox's remote Sent folder). It is independent of SMTP
			// success and never re-sends.
			if w.remote != nil {
				w.remote.CopyPendingSentCopies(context.Background())
			}
		}
	}
}

// watchInbox observes one inbox's INBOX folder until ctx is cancelled. It prefers a
// realtime IDLE watch on a dedicated session; when the server does not advertise
// IDLE (or the watch fails) it falls back to bounded polling. A connection failure
// backs off and retries; it never advances the cursor past unrecorded mail, so no
// arrival is lost.
func (w *RemoteWorker) watchInbox(ctx context.Context, inbox model.Inbox) {
	folder := w.inboxFolder(inbox)
	// Run one detection pass up front so a fresh watcher reports mail that
	// arrived while the process was down.
	w.detectInbox(ctx, inbox)
	for !w.stopping() {
		if err := w.runWatchSession(ctx, inbox, folder); err != nil {
			if ctx.Err() != nil || w.stopping() {
				return
			}
			w.log.Debug("remote watch retry", "inbox_id", inbox.ID, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-time.After(w.watchBackoff):
			}
			continue
		}
		return
	}
}

// runWatchSession opens a dedicated session and runs either an IDLE watch or a
// poll loop, invoking detection on every signal. It returns nil only on a clean
// context cancellation.
func (w *RemoteWorker) runWatchSession(ctx context.Context, inbox model.Inbox, folder string) error {
	sess, err := w.remote.openRemoteSession(ctx, inbox)
	if err != nil {
		return err
	}
	defer sess.Close()

	if watcher, ok := sess.(watchSession); ok && watcher.IdleCapable() {
		return w.runIdleWatch(ctx, inbox, folder, watcher)
	}
	return w.runPollWatch(ctx, inbox, watcherPoll(sess))
}

// watchSession is the optional IDLE surface of a remote session. The production
// adapter implements it; a test fake may or may not.
type watchSession interface {
	IdleCapable() bool
	Watch(ctx context.Context, folder string, buffer int) (<-chan imap.Notification, func(), <-chan error, error)
}

// watchSessionPoll is the optional poll surface of a remote session.
type watchSessionPoll interface {
	Poll(ctx context.Context, folder string, interval time.Duration, buffer int) (<-chan imap.Notification, func(), <-chan error, error)
}

// watcherPoll returns the poll surface if present.
func watcherPoll(sess RemoteSession) watchSessionPoll {
	if p, ok := sess.(watchSessionPoll); ok {
		return p
	}
	return nil
}

// runIdleWatch runs the realtime IDLE watch, running detection on each signal and
// reconnecting (returning) on a watch error.
func (w *RemoteWorker) runIdleWatch(ctx context.Context, inbox model.Inbox, folder string, watcher watchSession) error {
	notify, stop, errCh, err := watcher.Watch(ctx, folder, 16)
	if err != nil {
		return err
	}
	defer stop()
	// Coalesce bursts: a server may push several mailbox updates in quick
	// succession; detection is idempotent by cursor so extra passes are cheap, but
	// a small debounce keeps them bounded.
	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-errCh:
			return errors.New("remote watch ended")
		case _, ok := <-notify:
			if !ok {
				return errors.New("remote watch closed")
			}
			debounce.Reset(250 * time.Millisecond)
		case <-debounce.C:
			w.detectInbox(ctx, inbox)
		}
	}
}

// runPollWatch runs the bounded polling fallback when IDLE is unavailable.
func (w *RemoteWorker) runPollWatch(ctx context.Context, inbox model.Inbox, poller watchSessionPoll) error {
	if poller == nil {
		// The adapter has no poll surface: fall back to a plain ticker that runs
		// detection through a fresh session each period.
		ticker := time.NewTicker(w.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-w.stop:
				return nil
			case <-ticker.C:
				w.detectInbox(ctx, inbox)
			}
		}
	}
	notify, stop, errCh, err := poller.Poll(ctx, inboxFolder(inbox), w.pollInterval, 8)
	if err != nil {
		return err
	}
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-errCh:
			return errors.New("remote poll ended")
		case _, ok := <-notify:
			if !ok {
				return errors.New("remote poll closed")
			}
			w.detectInbox(ctx, inbox)
		}
	}
}

// DetectInbox runs one detection pass for an inbox. It is the on-demand entry
// point the HTTP agent calls when a client polls for events, so a new arrival is
// detected without waiting for an IDLE signal. It is a no-op for a non-standalone
// or unconfigured inbox.
func (w *RemoteWorker) DetectInbox(ctx context.Context, inbox model.Inbox) {
	if inbox.Kind != model.InboxKindStandalone || inbox.AccountID == "" || inbox.ID == "" {
		return
	}
	// Durable detection belongs to the worker: a polling client disconnect must
	// not cancel persistence after a successful IMAP fetch. Bound the pass and
	// still honour process shutdown.
	pass, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-w.stop:
			cancel()
		case <-done:
		}
	}()
	defer close(done)
	w.detectInbox(pass, inbox)
}

// PendingRemoteApprovals reports whether an inbox has an outstanding pending email
// approval request.
func (w *RemoteWorker) PendingRemoteApprovals(ctx context.Context, inbox model.Inbox) bool {
	return w.inboxHasPendingApproval(ctx, inbox)
}

// detectPendingApprovalInboxes forces a detection pass for every standalone inbox
// with an outstanding pending email approval request, so a control reply whose IDLE
// signal was missed (or whose server does not push) is still detected and actioned
// promptly. It is bounded and a no-op when no approval is pending.
func (w *RemoteWorker) detectPendingApprovalInboxes() {
	inboxes, err := w.svc.Store.ListDetectableRemoteInboxes(context.Background())
	if err != nil {
		return
	}
	for _, ib := range inboxes {
		if w.stopping() {
			return
		}
		if w.inboxHasPendingApproval(context.Background(), ib) {
			w.detectInbox(context.Background(), ib)
		}
	}
}

// ReconcileRemoteActionsForTest runs the delayed auto-trash sweep once. It exists so
// a test can drive the sweep deterministically without waiting for the ticker.
func (w *RemoteWorker) ReconcileRemoteActionsForTest(ctx context.Context) {
	w.reconcileRemoteActions()
}

// detectAll runs one detection pass over every configured inbox. It is used on
// startup (and in tests) so a fresh process catches up without waiting for an IDLE
// signal. It runs the passes concurrently, bounded by maxInboxConnections.
func (w *RemoteWorker) detectAll() {
	inboxes, err := w.svc.Store.ListDetectableRemoteInboxes(context.Background())
	if err != nil {
		w.log.Error("remote detection list", "error", err)
		return
	}
	sem := make(chan struct{}, w.maxInboxConnections)
	var wg sync.WaitGroup
	for _, ib := range inboxes {
		if w.stopping() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ib model.Inbox) {
			defer wg.Done()
			defer func() { <-sem }()
			w.detectInbox(context.Background(), ib)
		}(ib)
	}
	wg.Wait()
}

// inboxFolder resolves the folder a watcher observes for an inbox: its selected
// namespace when set, else the default INBOX. A standalone inbox's scope is itself
// the INBOX-and-children root; the watcher only ever detects new arrivals in the
// selected root folder, never its children.
func (w *RemoteWorker) inboxFolder(inbox model.Inbox) string {
	f := strings.TrimSpace(inbox.Namespace)
	if f == "" {
		f = model.NamespaceINBOX
	}
	return f
}

// inboxFolder is a package helper so both the worker and its watch sessions agree.
func inboxFolder(inbox model.Inbox) string {
	f := strings.TrimSpace(inbox.Namespace)
	if f == "" {
		f = model.NamespaceINBOX
	}
	return f
}

// detectInbox runs one durable detection pass for one inbox. It opens a session,
// reads the live UID set of the selected folder, and:
//
//   - on the first ever pass (no cursor) establishes the baseline to the current
//     max UID WITHOUT emitting arrivals, so enabling the watcher on an existing
//     mailbox never floods.
//   - otherwise, records one durable arrival per UID strictly greater than the
//     cursor's last_uid, then advances the cursor. UIDs are processed in ascending
//     order and the cursor is only advanced to the highest UID whose arrival rows
//     have all been persisted, so a mid-pass failure re-detects rather than skips.
//
// A connection failure returns without touching the cursor.
func (w *RemoteWorker) detectInbox(ctx context.Context, inbox model.Inbox) {
	if w.stopping() {
		return
	}
	key := inbox.AccountID + ":" + inbox.ID
	w.detectionMu.Lock()
	if w.detecting[key] {
		w.detectionMu.Unlock()
		return
	}
	w.detecting[key] = true
	w.detectionMu.Unlock()
	defer func() {
		w.detectionMu.Lock()
		delete(w.detecting, key)
		w.detectionMu.Unlock()
	}()
	folder := w.inboxFolder(inbox)
	sess, err := w.remote.openRemoteSession(ctx, inbox)
	if err != nil {
		w.log.Debug("remote detection open", "inbox_id", inbox.ID, "error", err)
		return
	}
	defer sess.Close()

	// Resolve the live folder and its UIDVALIDITY. A folder outside the inbox
	// scope is never watched.
	validity, verr := remoteFolderUIDValidity(ctx, sess, folder)
	if verr != nil {
		w.log.Debug("remote detection validity", "inbox_id", inbox.ID, "error", verr)
		return
	}

	cursor, ok, cerr := w.svc.Store.GetRemoteCursor(ctx, inbox.AccountID, inbox.ID, folder)
	if cerr != nil {
		w.log.Error("remote detection cursor", "inbox_id", inbox.ID, "error", cerr)
		return
	}
	if !ok || !cursor.BaselineDone || (cursor.UIDValidity != 0 && cursor.UIDValidity != validity) {
		// First observation (or a UIDVALIDITY reset): establish the notification
		// and detection baseline to the current maximum UID and emit nothing. A
		// bounded, newest-first, single-UID search yields the folder maximum
		// directly; the default ascending order would be truncated to the OLDEST
		// page on a folder larger than one page, yielding a baseline far below
		// the true maximum and permanently suppressing detection.
		res, serr := sess.Search(ctx, folder, imap.SearchQuery{NewestFirst: true, Limit: 1})
		if serr != nil {
			w.log.Debug("remote detection baseline search", "inbox_id", inbox.ID, "error", serr)
			return
		}
		maxUID := uint32(0)
		if len(res.UIDs) > 0 {
			maxUID = res.UIDs[0]
		}
		if err := w.svc.Store.EstablishRemoteBaseline(ctx, inbox.AccountID, inbox.ID, folder, validity, maxUID); err != nil {
			w.log.Error("remote baseline establish", "inbox_id", inbox.ID, "error", err)
			return
		}
		// Record the notification baseline flag once so a later pass never treats
		// the pre-existing backlog as notifiable.
		if set, berr := w.svc.Store.RemoteNotifyBaselineSet(ctx, inbox.AccountID, inbox.ID); berr == nil && !set {
			if merr := w.svc.Store.MarkRemoteNotifyBaselineSet(ctx, inbox.AccountID, inbox.ID); merr != nil {
				w.log.Warn("remote baseline flag", "inbox_id", inbox.ID, "error", merr)
			}
		}
		return
	}

	// Incremental: the oldest new UIDs strictly above the cursor, fetched with an
	// IMAP-native, bounded "(cursor+1):*" search so a detector never has to load
	// the whole folder (and never files the wrong UIDs when the folder is larger
	// than one search page). The cursor only advances to a UID whose arrival rows
	// are all persisted, so a mid-pass failure re-detects rather than skips.
	res, err := sess.Search(ctx, folder, imap.SearchQuery{AfterUID: cursor.LastUID, Limit: remoteArrivalPageLimit})
	if err != nil {
		w.log.Debug("remote detection search", "inbox_id", inbox.ID, "error", err)
		return
	}
	newUIDs := res.UIDs
	if len(newUIDs) == 0 {
		return
	}
	// Defensive: a session that ignores Limit must not blow the per-pass bound.
	if len(newUIDs) > remoteArrivalPageLimit {
		newUIDs = newUIDs[:remoteArrivalPageLimit]
	}
	headers, liveValidity, herr := sess.ListHeaders(ctx, folder, newUIDs, len(newUIDs))
	if herr != nil {
		w.log.Debug("remote detection headers", "inbox_id", inbox.ID, "error", herr)
		return
	}
	// A UIDVALIDITY change between the SEARCH and the FETCH means the UIDs no
	// longer name the messages the search found; do not record arrivals under a
	// stale generation. The cursor is left unadvanced so the next pass re-baselines.
	if liveValidity != 0 && liveValidity != validity {
		w.log.Debug("remote detection generation changed mid-pass", "inbox_id", inbox.ID, "want", validity, "live", liveValidity)
		return
	}
	byUID := map[uint32]imap.MessageHeader{}
	for _, h := range headers {
		byUID[h.UID] = h
	}
	highestRecorded := cursor.LastUID
	for _, uid := range newUIDs {
		h, ok := byUID[uid]
		if !ok {
			// The message vanished between the search and the header fetch
			// (expunged). It is safe to advance past it: there is nothing to
			// deliver and re-detecting it forever would stall the mailbox.
			highestRecorded = uid
			continue
		}
		arrival, inserted, aerr := w.svc.Store.RecordRemoteArrival(ctx, inbox.AccountID, inbox.ID, store.RemoteArrivalInput{
			FolderPath:   folder,
			UIDValidity:  validity,
			UID:          uid,
			RFCMessageID: h.MessageID,
			FromName:     h.From.Name,
			FromAddress:  h.From.Address,
			Subject:      h.Subject,
			SizeBytes:    h.Size,
			InternalDate: internalDatePtr(h),
		})
		if aerr != nil {
			w.log.Error("remote arrival record", "inbox_id", inbox.ID, "uid", uid, "error", aerr)
			// Do not advance past an unrecorded UID: a later pass retries it.
			break
		}
		if inserted {
			w.onNewArrival(ctx, inbox, arrival)
		}
		highestRecorded = uid
	}
	if highestRecorded > cursor.LastUID {
		if err := w.svc.Store.AdvanceRemoteCursor(ctx, inbox.AccountID, inbox.ID, folder, validity, highestRecorded); err != nil {
			w.log.Error("remote cursor advance", "inbox_id", inbox.ID, "error", err)
		}
	}
}

// onNewArrival handles a freshly-recorded arrival. It classifies the arrival and,
// for ordinary notifiable mail, persists the durable remote event (which the
// existing webhook and Hermes workers consume on demand) and publishes it to the
// hub so SSE/long-poll wake. Proactive delivery is never triggered by an ordinary
// live read. Classification fails closed: an arrival that cannot be conclusively
// classified is left pending and never published as ordinary mail.
func (w *RemoteWorker) onNewArrival(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) {
	// Pending-email-approval detection must happen before any proactive delivery:
	// an approval control message is consumed through the shared remote-control
	// handler (token + From validated there) and is never forwarded.
	control, retry, err := w.classifyArrival(ctx, inbox, arrival)
	if err != nil {
		w.log.Warn("remote arrival classify", "inbox_id", inbox.ID, "arrival", arrival.ID, "error", err)
	}
	if retry {
		// Leave the arrival pending for a later pass; do not publish it as ordinary
		// mail. Bound the retries so a permanently unreadable body cannot pin the
		// queue forever.
		if arrival.Attempts >= maxArrivalClassifyAttempts {
			w.settle(ctx, inbox, arrival, store.RemoteArrivalFailed, "control classification did not complete")
		}
		return
	}
	if control {
		w.settle(ctx, inbox, arrival, store.RemoteArrivalSkipped, "approval control mail")
		return
	}
	// A handoff's own notification (or the frozen handoff draft echoed back) is
	// excluded from proactive delivery by its correlation Message-ID; it is never
	// mistaken for fresh mail. The lookup is durable and cheap.
	if w.isHandoffNotification(ctx, inbox, arrival) {
		_ = w.svc.Store.MarkRemoteNotified(ctx, inbox.AccountID, inbox.ID, arrival.FolderPath, arrival.UIDValidity, arrival.UID)
		w.settle(ctx, inbox, arrival, store.RemoteArrivalSkipped, "handoff notification")
		return
	}
	// The proactive allowlist gates the notification, not live reads: a message
	// from a disallowed sender is still readable live, but it never raises a
	// proactive notification. Live-read suppression is intentionally not applied.
	if !inbox.AllowsInbound(arrival.FromAddress) {
		_ = w.svc.Store.MarkRemoteNotified(ctx, inbox.AccountID, inbox.ID, arrival.FolderPath, arrival.UIDValidity, arrival.UID)
		w.settle(ctx, inbox, arrival, store.RemoteArrivalSkipped, "sender not in proactive allowlist")
		return
	}
	// Record the notification baseline BEFORE publishing, so a crash after the
	// event is persisted but before it is fanned out never re-notifies the same
	// message on restart.
	if err := w.svc.Store.MarkRemoteNotified(ctx, inbox.AccountID, inbox.ID, arrival.FolderPath, arrival.UIDValidity, arrival.UID); err != nil {
		w.log.Warn("remote notify baseline", "arrival", arrival.ID, "error", err)
	}
	// The arrival event is what the generic SSE/long-poll surface replays, and
	// what the demand-based webhook/Hermes workers consume. It is persisted before
	// any realtime publication, so the durable truth exists first.
	ev, emitted, err := w.svc.Store.RecordRemoteArrivalEvent(ctx, inbox.AccountID, inbox.ID, arrival)
	if err != nil {
		w.log.Error("remote arrival event", "inbox_id", inbox.ID, "arrival", arrival.ID, "error", err)
		w.settle(ctx, inbox, arrival, store.RemoteArrivalFailed, "event persistence failed")
		return
	}
	if emitted {
		w.svc.Hub.Publish(ev)
	}
	w.settle(ctx, inbox, arrival, store.RemoteArrivalDelivered, "")
	// Enqueue the durable auto-action (mark read on ACK + delayed trash) for an
	// arrival that was accepted. The action state is independent of the fan-out.
	w.ensureRemoteAction(ctx, arrival)
}

// settle marks a just-recorded arrival's proactive state. The arrival may have
// been claimed by a concurrent pass; a conflict is not an error here.
func (w *RemoteWorker) settle(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival, state, reason string) {
	if err := w.svc.Store.SettleRemoteArrival(ctx, inbox.AccountID, arrival.ID, state, reason); err != nil && !errors.Is(err, store.ErrConflict) {
		w.log.Warn("remote arrival settle", "arrival", arrival.ID, "state", state, "error", err)
	}
}

// isHandoffNotification reports whether an arrival is a RemoteDraft handoff's own
// notification (or the frozen handoff draft echoed back), which is never forwarded
// proactively. The frozen handoff draft's Message-ID is recorded, so a cheap
// durable lookup catches it first. The notification MIME generated by the handoff
// carries the non-secret correlation header (X-MailMoose-Handoff-ID) but a
// distinct Message-ID, so when the Message-ID lookup misses the raw body is
// fetched once and the header checked: the header travels with the mail, so the
// exclusion is durable across restarts and does not depend on a stored
// notification id.
func (w *RemoteWorker) isHandoffNotification(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) bool {
	id := strings.TrimSpace(arrival.RFCMessageID)
	if id == "" {
		return false
	}
	// The frozen handoff draft's Message-ID is recorded, as is the generated
	// notification email's Message-ID. Either one coming back is excluded by a
	// durable lookup, so no live body fetch is needed and the exclusion survives a
	// restart. An IMAP ENVELOPE strips the angle brackets, while the stored
	// handoff Message-IDs are bracketed, so test both the wire form and its
	// bracketed normalization.
	candidates := []string{id, ensureMessageID(id)}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := w.svc.Store.HandoffByMessageID(ctx, inbox.AccountID, c); err == nil {
			return true
		}
		if _, err := w.svc.Store.HandoffByNotificationMessageID(ctx, inbox.AccountID, c); err == nil {
			return true
		}
	}
	return false
}

// classifyArrival reports whether an arrival is a pending-email-approval control
// message. When the inbox has an outstanding approval request and the arrival
// looks like a control subject/reply, it is forced through the shared remote
// control handler. A handoff's own notification (carrying the handoff header) is
// excluded and is never a control. Ordinary mail is classified false.
//
// It fails closed. When it cannot conclusively classify the message (the body
// could not be fetched or parsed, or the control handler had a transient error) it
// returns retry=true with control possibly set: the caller must NOT publish the
// message as ordinary mail and must leave the arrival pending for a later pass.
// This guarantees a control message whose body fetch failed is never forwarded to
// an agent, and a genuine approval is retried rather than dropped.
func (w *RemoteWorker) classifyArrival(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) (control bool, retry bool, err error) {
	subject := strings.TrimSpace(arrival.Subject)
	controlSubject := looksLikeControl(subject)
	if !controlSubject {
		// A reply-form control message can only be recognised from the body, which
		// requires a live fetch. Only do that when the inbox actually has an
		// outstanding approval request: otherwise the body fetch is pure waste.
		if !w.inboxHasPendingApproval(ctx, inbox) {
			return false, false, nil
		}
	}
	raw, ok := w.fetchArrivalRaw(ctx, inbox, arrival)
	if !ok {
		// The body could not be read. A control subject is definitely control mail
		// that must not be forwarded; for a non-control subject we cannot prove the
		// message is ordinary. Defer either way.
		return controlSubject, true, nil
	}
	parsed, perr := mailparse.ParseBytes(raw, w.svc.mimeLimits())
	if perr != nil {
		return controlSubject, true, nil
	}
	if !looksLikeControl(parsed.Subject) && !looksLikeControlReply(parsed) {
		return false, false, nil
	}
	if HasHandoffHeader(raw) {
		return false, false, nil
	}
	// Force detection of a pending email approval: the shared handler validates
	// the live token and the nominated approver From address before acting, and
	// consumes the control mail so it never becomes a message. A consumed control
	// message returns transport.ErrInboundIgnored, which is success, not failure.
	if herr := w.svc.HandleRemoteApprovalControl(ctx, inbox, parsed, raw); herr != nil {
		if errors.Is(herr, transport.ErrInboundIgnored) {
			return true, false, nil
		}
		return true, true, fmt.Errorf("remote approval control: %w", herr)
	}
	return true, false, nil
}

// maxArrivalClassifyAttempts bounds how many times a pending arrival is retried
// for control classification before it is terminally failed, so a permanently
// unreadable body cannot pin the pending queue forever.
const maxArrivalClassifyAttempts = 8

// fetchArrivalRaw fetches an arrival's raw MIME to a transient temp file and reads
// it back. It never marks the message seen (BODY.PEEK). The temp file is removed.
func (w *RemoteWorker) fetchArrivalRaw(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) ([]byte, bool) {
	sess, err := w.remote.openRemoteSession(ctx, inbox)
	if err != nil {
		return nil, false
	}
	defer sess.Close()
	var buf limitedBuffer
	loc := imap.Locator{FolderPath: arrival.FolderPath, UIDValidity: arrival.UIDValidity, UID: arrival.UID, MessageID: arrival.RFCMessageID}
	lw := &limitedWriter{w: &buf, limit: w.remote.remoteBodyLimit()}
	if err := sess.FetchRawMIME(ctx, loc, lw); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// inboxHasPendingApproval reports whether an inbox has any outstanding send
// request awaiting an email decision.
func (w *RemoteWorker) inboxHasPendingApproval(ctx context.Context, inbox model.Inbox) bool {
	n, err := w.svc.Store.CountPendingSendRequests(ctx, inbox.AccountID, inbox.ID)
	if err != nil {
		return false
	}
	return n > 0
}

// drainRemoteArrivals re-drives any arrival left pending by a crash: an arrival is
// only recorded (pending) before its event is persisted, so a resumed pass either
// persists the event or terminally skips it. It is demand-based: an arrival with no
// configured webhook/Hermes client settles with no network call.
func (w *RemoteWorker) drainRemoteArrivals() {
	for i := 0; i < 256 && !w.stopping(); i++ {
		arrival, ok, err := w.svc.Store.ClaimNextRemoteArrival(context.Background(), time.Now().UTC(), "remote", remoteArrivalLease)
		if err != nil {
			w.log.Error("remote arrival claim", "error", err)
			return
		}
		if !ok {
			return
		}
		// A claimed arrival is one whose durable event was not persisted before a
		// crash. Re-drive it through the normal classification path.
		if arrival.Control {
			w.settle(context.Background(), model.Inbox{AccountID: arrival.AccountID, ID: arrival.InboxID}, arrival, store.RemoteArrivalSkipped, "approval control mail")
			continue
		}
		w.recoverArrival(context.Background(), arrival)
	}
}

// recoverArrival re-drives a claimed (crash-recovered) arrival through the same
// classification path a fresh arrival takes, so recovery cannot bypass the
// control-mail, handoff-exclusion or allow-list checks. It resolves the owning
// inbox and delegates to onNewArrival.
func (w *RemoteWorker) recoverArrival(ctx context.Context, arrival store.RemoteArrival) {
	inbox, err := w.svc.Store.GetInboxInternal(ctx, arrival.AccountID, arrival.InboxID)
	if err != nil {
		// The inbox is gone: nothing can be delivered. Settle terminally so the
		// arrival does not pin the drain.
		w.settle(ctx, model.Inbox{AccountID: arrival.AccountID, ID: arrival.InboxID}, arrival, store.RemoteArrivalFailed, "inbox unavailable")
		return
	}
	w.onNewArrival(ctx, inbox, arrival)
}

// reconcileRemoteActions applies the delayed auto-trash action for every remote
// arrival whose window has elapsed.
func (w *RemoteWorker) reconcileRemoteActions() {
	due, err := w.svc.Store.ListRemoteActionsDueForTrash(context.Background(), time.Now().UTC(), 128)
	if err != nil {
		w.log.Error("remote action list", "error", err)
		return
	}
	for _, a := range due {
		if w.stopping() {
			return
		}
		w.applyRemoteTrash(context.Background(), a)
	}
}

// applyRemoteTrash moves a remote arrival's message to the inbox's remote Trash
// folder. A failure is retried (the action stays due); it never marks the message
// locally deleted (there is no local message row for a remote arrival).
func (w *RemoteWorker) applyRemoteTrash(ctx context.Context, a store.RemoteAction) {
	inbox, err := w.svc.Store.GetInboxInternal(ctx, a.AccountID, a.InboxID)
	if err != nil {
		// The inbox is gone (or unreadable). Record the reason so the skipped
		// action is auditable, then mark done so it does not pin the sweep.
		_ = w.svc.Store.RecordRemoteActionError(ctx, a.AccountID, a.ArrivalID, "inbox unavailable for auto-trash")
		w.log.Warn("remote auto-trash skipped: inbox unavailable", "arrival", a.ArrivalID, "inbox_id", a.InboxID, "error", err)
		_ = w.svc.Store.MarkRemoteActionTrashDone(ctx, a.AccountID, a.ArrivalID)
		return
	}
	trash, terr := w.svc.Store.GetSystemFolder(ctx, a.AccountID, a.InboxID, model.FolderRoleTrash)
	if terr != nil || strings.TrimSpace(trash.Path) == "" {
		// No remote Trash folder: the action cannot be applied. Record the
		// reason, then mark done so it does not pin the sweep forever.
		_ = w.svc.Store.RecordRemoteActionError(ctx, a.AccountID, a.ArrivalID, "no remote Trash folder mapped for auto-trash")
		w.log.Warn("remote auto-trash skipped: no Trash folder", "arrival", a.ArrivalID, "inbox_id", a.InboxID)
		_ = w.svc.Store.MarkRemoteActionTrashDone(ctx, a.AccountID, a.ArrivalID)
		return
	}
	sess, oerr := w.remote.openRemoteSession(ctx, inbox)
	if oerr != nil {
		_ = w.svc.Store.RecordRemoteActionError(ctx, a.AccountID, a.ArrivalID, "remote connection unavailable")
		return
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: a.FolderPath, UIDValidity: a.UIDValidity, UID: a.UID}
	if _, merr := sess.MoveMessage(ctx, loc, trash.Path); merr != nil {
		if errors.Is(merr, imap.ErrNotFound) {
			// The message is already gone (server-expired or moved): the action is
			// satisfied.
			_ = w.svc.Store.MarkRemoteActionTrashDone(ctx, a.AccountID, a.ArrivalID)
			return
		}
		_ = w.svc.Store.RecordRemoteActionError(ctx, a.AccountID, a.ArrivalID, "remote move failed")
		return
	}
	_ = w.svc.Store.MarkRemoteActionTrashDone(ctx, a.AccountID, a.ArrivalID)
}

// ensureRemoteAction creates the durable auto-action for an arrival, stamping the
// delayed trash instant from the inbox's auto-trash window when configured.
func (w *RemoteWorker) ensureRemoteAction(ctx context.Context, arrival store.RemoteArrival) {
	inbox, err := w.svc.Store.GetInboxInternal(ctx, arrival.AccountID, arrival.InboxID)
	if err != nil {
		return
	}
	if !inbox.AutoMarkReadOnDelivery && inbox.AutoTrashAfterDeliveryHours == nil {
		return
	}
	var trashAfter time.Duration
	if inbox.AutoTrashAfterDeliveryHours != nil {
		trashAfter = time.Duration(*inbox.AutoTrashAfterDeliveryHours) * time.Hour
	}
	if _, err := w.svc.Store.EnsureRemoteAction(ctx, arrival.AccountID, arrival, trashAfter); err != nil {
		w.log.Warn("remote action ensure", "arrival", arrival.ID, "error", err)
	}
}

// MarkRemoteArrivalRead implements the ACK mark-read auto-action for a remote
// arrival: it sets \Seen on the live server exactly once. It is exported so the
// HTTP agent's ACK handler can drive it after a generic delivery acknowledgement.
func (w *RemoteWorker) MarkRemoteArrivalRead(ctx context.Context, accountID, inboxID, arrivalID string) error {
	arrival, err := w.svc.Store.GetRemoteArrival(ctx, accountID, arrivalID)
	if err != nil {
		return err
	}
	if arrival.InboxID != inboxID {
		return store.ErrForbidden
	}
	inbox, err := w.svc.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return err
	}
	sess, err := w.remote.openRemoteSession(ctx, inbox)
	if err != nil {
		return err
	}
	defer sess.Close()
	loc := imap.Locator{FolderPath: arrival.FolderPath, UIDValidity: arrival.UIDValidity, UID: arrival.UID, MessageID: arrival.RFCMessageID}
	if _, err := sess.SetFlags(ctx, loc, []string{imap.FlagSeen}, nil); err != nil {
		if errors.Is(err, imap.ErrNotFound) {
			_ = w.svc.Store.SetRemoteActionMarkReadDone(ctx, accountID, arrivalID)
			return nil
		}
		return err
	}
	return w.svc.Store.SetRemoteActionMarkReadDone(ctx, accountID, arrivalID)
}

// remoteFolderUIDValidity resolves a folder's live UIDVALIDITY through the
// adapter's EnsureFolderExists (an EXAMINE under the hood).
func remoteFolderUIDValidity(ctx context.Context, sess RemoteSession, folder string) (uint32, error) {
	return sess.EnsureFolderExists(ctx, folder)
}

// openRemoteSession resolves and dials a live session for a standalone inbox
// without a principal. The caller must Close it. It is the watcher's connection
// entry point and never marks any message seen.
func (m *RemoteMailboxService) openRemoteSession(ctx context.Context, inbox model.Inbox) (RemoteSession, error) {
	r, err := m.ResolveRemote(ctx, inbox.AccountID, inbox.ID)
	if err != nil {
		return nil, err
	}
	if r.Secrets.IMAPPassword == "" {
		return nil, model.NewMailboxError(model.ErrKindUnavailable, "remote connector has no password configured", false, ErrRemoteNotBound)
	}
	sess, err := m.dial(ctx, m.imapConfig(r))
	if err != nil {
		return nil, normalizeRemoteError(err)
	}
	return sess, nil
}

// fetchArrivalRawToTemp streams a remote arrival's raw RFC5322 MIME to a transient
// temp file and returns its path and size. It is the demand-based forward path's
// body fetch: it is only called when a webhook client is configured for raw
// forwarding, never by a live read. The caller must CleanupRemoteRaw. The message
// is never marked seen (BODY.PEEK).
func (m *RemoteMailboxService) fetchArrivalRawToTemp(ctx context.Context, inbox model.Inbox, arrival store.RemoteArrival) (string, int64, error) {
	sess, err := m.openRemoteSession(ctx, inbox)
	if err != nil {
		return "", 0, err
	}
	defer sess.Close()
	dir := filepath.Join(m.Service.Config.DataDir, "remote", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	f, err := os.CreateTemp(dir, "forward-*.eml")
	if err != nil {
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	lw := &limitedWriter{w: f, limit: m.remoteBodyLimit()}
	loc := imap.Locator{FolderPath: arrival.FolderPath, UIDValidity: arrival.UIDValidity, UID: arrival.UID, MessageID: arrival.RFCMessageID}
	if ferr := sess.FetchRawMIME(ctx, loc, lw); ferr != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", 0, normalizeRemoteError(ferr)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
	return f.Name(), lw.n, nil
}

// internalDatePtr returns the arrival's internal date when the adapter supplied one.
func internalDatePtr(h imap.MessageHeader) *time.Time {
	if h.InternalDate.IsZero() {
		if h.Date.IsZero() {
			return nil
		}
		t := h.Date.UTC()
		return &t
	}
	t := h.InternalDate.UTC()
	return &t
}

// limitedBuffer is a small in-memory io.Writer used to read back a transient remote
// body. The body is bounded by the remote size cap before it is buffered here.
type limitedBuffer struct {
	buf []byte
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf }
