package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/dellarb/mammothmail/internal/idgen"
	"github.com/dellarb/mammothmail/internal/safepath"
	"github.com/dellarb/mammothmail/internal/store"
)

// workflowRetention is how long a terminal workflow job (and its retained raw
// MIME) is kept before the sweep removes it. It matches the outbound delivery
// log's 30-day floor.
const workflowRetention = 30 * 24 * time.Hour

// orphanSweepInterval is how often the raw-MIME orphan sweep walks the messages
// tree. A purge normally unlinks its own file, so this sweep is only a safety
// net for a failed unlink; once a day is ample and keeps the walk off the hot
// path.
const orphanSweepInterval = 24 * time.Hour

// orphanGracePeriod is how young a raw MIME file must be before the sweep will
// consider it. A message's file is moved to its final path just before the
// message row is committed, so a file can briefly exist with no referencing row;
// the grace period keeps the sweep clear of that window.
const orphanGracePeriod = time.Hour

// maxSendConcurrency bounds the outbox sender pool. Each in-flight delivery
// holds its reconstructed attachments in memory, so this caps peak memory as
// well as parallelism; it must match the OUTBOUND_CONCURRENCY ceiling.
const maxSendConcurrency = 32

// OutboxWorker delivers pending outbound messages in the background. It runs a
// bounded pool of sender goroutines that poll the outbox, each claiming one due
// message at a time and delivering it. On startup it recovers claims left by a
// previous process and re-scans so pending messages resume.
type OutboxWorker struct {
	svc    *Service
	log    *slog.Logger
	stop   chan struct{}
	done   chan struct{}
	wake   chan struct{}
	period time.Duration
	owner  string
	// sendConcurrency is the number of sender goroutines per delivery pass. It
	// is set before Start (from the config) and never changed concurrently.
	sendConcurrency int
	// claimLease is the claim lifetime used by both delivery loops. It must
	// cover a full provider attempt: a lease shorter than an in-flight send
	// lets a second claim (and a second send) happen while the first is still
	// transmitting.
	claimLease time.Duration
	// lastOrphanSweep gates the raw-MIME orphan sweep so the filesystem walk
	// runs once a day rather than on every maintenance pass. It is touched only
	// from the worker goroutine.
	lastOrphanSweep time.Time
}

func NewOutboxWorker(svc *Service, log *slog.Logger) *OutboxWorker {
	if log == nil {
		log = slog.Default()
	}
	concurrency := 1
	if svc != nil {
		concurrency = svc.Config.OutboundConcurrencyCount()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > maxSendConcurrency {
		concurrency = maxSendConcurrency
	}
	// The claim lease must be at least one attempt long plus headroom, so a
	// normal attempt can never be re-claimed while it is mid-send.
	lease := minOutboxClaimLease
	if svc != nil {
		if timeout := svc.Config.OutboundDeliveryAttemptTimeout(); lease < timeout+outboxClaimLeaseMargin {
			lease = timeout + outboxClaimLeaseMargin
			log.Info("outbox claim lease extended to cover the delivery attempt timeout", "lease", lease.String())
		}
	}
	return &OutboxWorker{svc: svc, log: log, stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1), period: 5 * time.Second, owner: idgen.New("wrk"), sendConcurrency: concurrency, claimLease: lease}
}

// The lease constants: the default mirrors the long-standing 15-minute lease;
// the margin sits on top of the per-attempt timeout when that timeout is
// configured larger than the default.
const (
	minOutboxClaimLease    = 15 * time.Minute
	outboxClaimLeaseMargin = time.Minute
)

// Start launches the worker loop. It returns immediately.
func (w *OutboxWorker) Start() {
	go w.run()
}

// SetPeriod overrides the poll interval (used by tests).
func (w *OutboxWorker) SetPeriod(d time.Duration) { w.period = d }

// Wake requests an immediate delivery pass without waiting for the next poll
// tick. It is safe to call before Start or concurrently from any goroutine; the
// capacity-one channel coalesces bursts into at most one queued request (and
// never blocks a caller). The periodic poll remains the backstop.
func (w *OutboxWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Stop signals the worker to stop promptly. The delivery loops observe the
// signal between messages (not only after a full backlog), so shutdown is fast
// even under load.
func (w *OutboxWorker) Stop() {
	close(w.stop)
	<-w.done
}

// maintenanceEvery is how many delivery ticks pass between maintenance sweeps.
// Delivery runs on the short poll interval while expiry/cleanup run far less
// often, so a large database is not churned every few seconds.
const maintenanceEvery = 12

func (w *OutboxWorker) run() {
	defer close(w.done)
	// Under the single-process model any outstanding claim is abandoned.
	if err := w.svc.Store.RecoverAbandonedClaims(context.Background()); err != nil {
		w.log.Error("outbox claim recovery", "error", err)
	}
	if err := w.svc.Store.RecoverAbandonedWorkflowClaims(context.Background()); err != nil {
		w.log.Error("workflow claim recovery", "error", err)
	}
	w.reconcileUncertainSends(true)
	if n, err := w.svc.Store.RecoverStaleIdempotency(context.Background(), time.Now().UTC()); err != nil {
		w.log.Error("stale idempotency recovery", "error", err)
	} else if n > 0 {
		w.log.Info("reclaimed stale idempotency reservations", "count", n)
	}
	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	// Re-scan on startup so pending messages from a previous process resume.
	w.deliver()
	w.maintain()
	for i := 1; ; i++ {
		select {
		case <-w.stop:
			return
		case <-w.wake:
			w.deliver()
			if i%maintenanceEvery == 0 {
				w.maintain()
			}
		case <-ticker.C:
			w.deliver()
			if i%maintenanceEvery == 0 {
				w.maintain()
			}
		}
	}
}

// deliver runs the per-tick delivery passes. Each step is independently
// panic-guarded so a fault in one poison message cannot exit the process (and
// with it every other inbox); the guard logs and the pass moves on.
func (w *OutboxWorker) deliverWebhooks() {
	h := NewWebhookWorker(w.svc)
	for i := 0; i < 64 && !w.stopping(); i++ {
		// Each client gets its own budget; one slow endpoint must not consume
		// the budget of every subsequent client in the pass.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := h.RunOnce(ctx)
		cancel()
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			w.log.Warn("webhook delivery failed", "error", err)
			return
		}
	}
}

func (w *OutboxWorker) deliver() {
	for _, step := range []struct {
		name string
		fn   func()
	}{
		// Handoff publication runs first: a RemoteDraft handoff's "draft ready"
		// notification (an outbound_workflow job) must never be delivered before
		// the draft it announces is actually published.
		{"publishHandoffs", w.publishHandoffs},
		{"deliverDue", w.deliverDue},
		{"deliverWorkflowDue", w.deliverWorkflowDue},
		{"deliverWebhooks", w.deliverWebhooks},
	} {
		_ = w.recoverUnit(step.name, step.fn)
	}
}

// publishHandoffs publishes queued RemoteDraft handoffs through the injected
// HandoffPublisher. When no publisher is installed it is a no-op and handoffs
// stay queued rather than being failed.
func (w *OutboxWorker) publishHandoffs() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w.svc.PublishHandoffs(ctx)
}

// maintain runs the infrequent maintenance sweeps: approval expiry, terminal
// workflow redaction/retention and MX receipt sweeping.
func (w *OutboxWorker) maintain() {
	for _, step := range []struct {
		name string
		fn   func()
	}{
		{"reconcileUncertainSends", func() { w.reconcileUncertainSends(false) }},
		{"expireApprovals", w.expireApprovals},
		{"sweepWorkflows", w.sweepWorkflows},
		{"sweepPendingFileCleanup", w.sweepPendingFileCleanup},
		{"sweepMXReceipts", w.sweepMXReceipts},
		{"sweepClientDeliveryLog", w.sweepClientDeliveryLog},
		{"trashDeliveredMail", w.trashDeliveredMail},
		{"purgeExpiredTrash", w.purgeExpiredTrash},
		{"sweepRemoteTerminal", w.sweepRemoteTerminal},
		{"sweepOrphanedMIME", w.sweepOrphanedMIME},
	} {
		_ = w.recoverUnit(step.name, step.fn)
	}
}

func (w *OutboxWorker) sweepPendingFileCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.svc.Store.SweepPendingFileCleanup(ctx); err != nil {
		w.log.Warn("pending file cleanup", "error", err)
	}
}

// reconcileUncertainSends parks abandoned provider handoffs without making
// them eligible for automatic resend.
func (w *OutboxWorker) reconcileUncertainSends(afterRestart bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messages, workflows, err := w.svc.Store.ReconcileUncertainSends(ctx, time.Now().UTC(), afterRestart)
	if err != nil {
		w.log.Error("uncertain delivery reconciliation", "error", err)
	} else if messages > 0 || workflows > 0 {
		w.log.Warn("unresolved handoffs need review", "messages", messages, "approval_notifications", workflows)
	}
}

// remoteTerminalRetention bounds how long a terminal remote sent-copy job and a
// terminal remote arrival are retained before their rows (and the copy's frozen
// raw MIME) are removed. It matches the workflow/client-log 30-day floor.
const remoteTerminalRetention = 30 * 24 * time.Hour

// sweepRemoteTerminal reclaims the durable remote state machines: it deletes
// terminal remote sent-copy jobs (unlinking each job's own frozen raw MIME) and
// terminal remote arrivals (and their action rows) past the retention window.
// Without this the remote_sent_copies / inbox_remote_arrivals tables and the
// frozen sentshare files would grow without bound.
func (w *OutboxWorker) sweepRemoteTerminal() {
	cutoff := time.Now().UTC().Add(-remoteTerminalRetention)
	paths, err := w.svc.Store.SweepRemoteSentCopies(context.Background(), cutoff)
	if err != nil {
		w.log.Error("remote sent-copy sweep", "error", err)
	} else {
		for _, p := range paths {
			if path, perr := safepath.Join(w.svc.Config.DataDir, p); perr == nil {
				_ = os.Remove(path)
			}
		}
	}
	if n, aerr := w.svc.Store.SweepRemoteArrivalRetention(context.Background(), cutoff); aerr != nil {
		w.log.Error("remote arrival sweep", "error", aerr)
	} else if n > 0 {
		w.log.Info("swept remote arrivals", "count", n)
	}
}

// clientDeliveryLogRetention bounds the per-client Webhook/Hermes delivery
// history. It matches the delivery log's 30-day floor; outstanding (pending)
// rows are never pruned.
const clientDeliveryLogRetention = 30 * 24 * time.Hour

// sweepClientDeliveryLog removes terminal Webhook and Hermes delivery-history
// rows older than the retention window, keeping the dashboard Log view bounded.
func (w *OutboxWorker) sweepClientDeliveryLog() {
	n, err := w.svc.Store.PruneClientDeliveryLog(context.Background(), clientDeliveryLogRetention, time.Now().UTC())
	if err != nil {
		w.log.Error("client delivery log sweep", "error", err)
		return
	}
	if n > 0 {
		w.log.Info("swept client delivery log", "count", n)
	}
}

// stopping reports whether Stop has been signalled, so a long delivery drain
// can yield promptly instead of working through an entire backlog first.
func (w *OutboxWorker) stopping() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// recoverUnit runs fn, converting a panic into a logged error instead of a
// process exit. It is the outer backstop for a whole pass; per-message delivery
// additionally records the affected unit as failed (deliverOneMessage /
// deliverOneWorkflow). The panic value is never logged, only its type and the
// goroutine stack, matching the HTTP recoverer's secret-safety rule (a stack
// holds frames, not request or credential values).
func (w *OutboxWorker) recoverUnit(name string, fn func()) (panicked error) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("worker panic recovered", "unit", name, "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
			panicked = fmt.Errorf("panic in %s: %T", name, x)
		}
	}()
	fn()
	return nil
}

// sweepMXReceipts removes MX delivery receipts past their retention horizon.
// Receipts are the retry dedup key; once expired a delivery is treated as new.
func (w *OutboxWorker) sweepMXReceipts() {
	n, err := w.svc.Store.SweepMXReceipts(context.Background(), time.Now().UTC())
	if err != nil {
		w.log.Error("mx receipt sweep", "error", err)
		return
	}
	if n > 0 {
		w.log.Info("swept expired mx receipts", "count", n)
	}
}

// trashDeliveredMail moves messages whose delivery auto-trash window has
// elapsed to Trash. It is the second half of the per-inbox delivery auto-action:
// RecordDelivery stamps messages.delivery_action_due_at when the inbox trigger
// is satisfied, and this sweep trashes those whose due instant has passed. A
// trashed message is still retained by the Trash model until the ordinary
// retention sweep purges it.
func (w *OutboxWorker) trashDeliveredMail() {
	events, err := w.svc.Store.TrashDeliveredDue(context.Background(), time.Now().UTC())
	if err != nil {
		w.log.Error("delivered-mail trash sweep", "error", err)
		return
	}
	for _, ev := range events {
		w.svc.Hub.Publish(ev)
	}
	if len(events) > 0 {
		w.log.Info("trashed delivered mail", "count", len(events))
	}
}

// purgeExpiredTrash permanently removes trashed messages older than their
// account's retention window and unlinks their raw files. Accounts with a
// retention of 0 are skipped, so their trash is retained until purged by hand.
func (w *OutboxWorker) purgeExpiredTrash() {
	paths, err := w.svc.Store.PurgeExpiredTrash(context.Background())
	if err != nil {
		w.log.Error("trash purge", "error", err)
		return
	}
	for _, p := range paths {
		if path, perr := safepath.Join(w.svc.Config.DataDir, p); perr == nil {
			_ = os.Remove(path)
		}
	}
	if len(paths) > 0 {
		w.log.Info("purged expired trash", "count", len(paths))
	}
}

// sweepOrphanedMIME removes raw MIME files that no message row references. A
// purge deletes its row and its file, but if the unlink fails the file is left
// with no row to find it by later, so nothing else would ever reclaim it. This
// sweep is that safety net.
//
// It is deliberately narrow: it walks only the messages tree (never the
// workflow, handoff or sentshare trees, which hold different artefacts), skips
// the staging .tmp directory (files there are live by definition), and skips any
// file younger than orphanGracePeriod, so it cannot race the moment between a
// file reaching its final path and its row being committed. When that leaves the
// count ambiguous it prefers to keep a file: a later sweep reclaims a real
// orphan, but a wrong unlink loses data.
func (w *OutboxWorker) sweepOrphanedMIME() {
	now := time.Now()
	if now.Sub(w.lastOrphanSweep) < orphanSweepInterval {
		return
	}
	w.lastOrphanSweep = now
	root := filepath.Join(w.svc.Config.DataDir, "messages")
	live, err := w.svc.Store.MessageRawPaths(context.Background())
	if err != nil {
		w.log.Error("orphan MIME sweep", "error", err)
		return
	}
	cutoff := now.Add(-orphanGracePeriod)
	removed := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A missing root (nothing stored yet) is not an error.
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			w.log.Warn("orphan MIME sweep walk", "path", path, "error", walkErr)
			return nil
		}
		if d.IsDir() {
			// Never descend into the staging area; its files are in flight.
			if d.Name() == ".tmp" {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			w.log.Warn("orphan MIME sweep stat", "path", path, "error", ierr)
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		rel, rerr := filepath.Rel(w.svc.Config.DataDir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, ok := live[rel]; ok {
			return nil
		}
		if rerr = os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			w.log.Warn("orphan MIME sweep remove", "path", rel, "error", rerr)
			return nil
		}
		removed++
		return nil
	})
	if err != nil {
		w.log.Error("orphan MIME sweep", "error", err)
		return
	}
	if removed > 0 {
		w.log.Info("removed orphaned raw MIME files", "count", removed)
	}
}

// sweepWorkflows redacts token markers from terminal workflow copies, then
// removes jobs (and their raw files) whose terminal state is older than the
// retention window.
func (w *OutboxWorker) sweepWorkflows() {
	w.redactWorkflows()
	paths, err := w.svc.Store.SweepWorkflows(context.Background(), time.Now().UTC().Add(-workflowRetention))
	if err != nil {
		w.log.Error("workflow sweep", "error", err)
		return
	}
	for _, p := range paths {
		if path, perr := safepath.Join(w.svc.Config.DataDir, p); perr == nil {
			_ = os.Remove(path)
		}
	}
}

// redactWorkflows removes approval control tokens from the stored bodies and
// raw MIME of terminal workflow jobs. The token is already dead once the
// request is terminal, but removing it from the retained copy is cheap
// defense-in-depth.
func (w *OutboxWorker) redactWorkflows() {
	jobs, err := w.svc.Store.TerminalUnredactedWorkflows(context.Background())
	if err != nil {
		w.log.Error("workflow redact list", "error", err)
		return
	}
	for _, job := range jobs {
		if err := w.svc.redactWorkflow(job); err != nil {
			w.log.Warn("workflow redact", "workflow_id", job.ID, "error", err)
		}
	}
}

// runSenders runs fn in sendConcurrency goroutines and waits for them all to
// finish. Each sender loops until no work is due, so the pool drains a backlog
// within the pass and does not accumulate goroutines between ticks. Each sender
// is independently panic-guarded so one faulting sender cannot take down the
// pass or the other senders.
func (w *OutboxWorker) runSenders(fn func()) {
	n := w.sendConcurrency
	if n < 1 {
		n = 1
	}
	if n == 1 {
		w.guardSender(fn)
		return
	}
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			w.guardSender(fn)
		}()
	}
	wg.Wait()
}

// guardSender contains a panic in one sender goroutine's pass so it cannot exit
// the process. The panic value is never logged, only its type and the stack,
// matching the worker's other recovery paths.
func (w *OutboxWorker) guardSender(fn func()) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("sender panic recovered", "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

func (w *OutboxWorker) deliverWorkflowDue() {
	w.runSenders(w.deliverWorkflowsLoop)
}

// deliverWorkflowsLoop claims and delivers due workflow jobs until none is due
// or the worker is stopping. It is run by each sender goroutine in the pool.
func (w *OutboxWorker) deliverWorkflowsLoop() {
	for {
		if w.stopping() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), w.svc.Config.OutboundDeliveryAttemptTimeout())
		workflowID, claim, err := w.svc.Store.ClaimNextWorkflow(ctx, time.Now().UTC(), w.owner, w.claimLease)
		if err != nil {
			cancel()
			w.log.Error("workflow claim", "error", err)
			return
		}
		if workflowID == "" {
			cancel()
			return
		}
		accountID, err := w.svc.AccountIDForWorkflow(ctx, workflowID)
		if err != nil {
			_ = w.svc.Store.ReleaseWorkflowClaim(ctx, workflowID, claim)
			cancel()
			w.log.Error("workflow account lookup", "workflow_id", workflowID, "error", err)
			continue
		}
		w.deliverOneWorkflow(ctx, accountID, workflowID, claim)
		cancel()
	}
}

// deliverOneWorkflow delivers a single claimed workflow job. A panic is caught
// per job and recorded as a failed attempt, so a poison job backs off instead
// of exiting the process or being reclaimed and re-panicking in a tight loop.
func (w *OutboxWorker) deliverOneWorkflow(ctx context.Context, accountID, workflowID, claim string) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("workflow delivery panic recovered", "workflow_id", workflowID, "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
			// Recording the outcome must never itself escape: it runs inside an
			// active recover, and a second panic here would still exit.
			w.safeFail("workflow delivery", func(outcomeCtx context.Context) {
				_ = w.svc.failWorkflowPanic(outcomeCtx, accountID, workflowID, claim, fmt.Errorf("panic during workflow delivery: %T", x))
			})
		}
	}()
	if err := w.svc.DeliverWorkflow(ctx, accountID, workflowID, claim); err != nil {
		w.log.Warn("workflow delivery failed", "workflow_id", workflowID, "error", err)
	}
}

// safeFail runs fail with a bounded, cancellation-independent context and
// absorbs any secondary panic, so a fault while recording a recovered panic
// cannot itself terminate the process.
func (w *OutboxWorker) safeFail(what string, fail func(context.Context)) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("panic while recording recovered failure", "unit", what, "type", fmt.Sprintf("%T", x))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fail(ctx)
}

// expireApprovals lapses external approval requests whose token has passed,
// unfreezing their drafts and publishing the expiry events.
func (w *OutboxWorker) expireApprovals() {
	events, err := w.svc.Store.ExpireApprovalRequests(context.Background(), time.Now().UTC())
	if err != nil {
		w.log.Error("approval expiry sweep", "error", err)
		return
	}
	for _, ev := range events {
		w.svc.Hub.Publish(ev)
	}
}

func (w *OutboxWorker) deliverDue() {
	w.runSenders(w.deliverMessagesLoop)
}

// deliverMessagesLoop claims and delivers due outbound messages until none is
// due or the worker is stopping. It is run by each sender goroutine in the
// pool; the store's claim keeps the senders from claiming the same message.
func (w *OutboxWorker) deliverMessagesLoop() {
	for {
		// Stop promptly on shutdown instead of draining a whole backlog first.
		if w.stopping() {
			return
		}
		// Each message gets its own deadline so one slow delivery cannot consume
		// the whole loop's budget. It is sized from the provider HTTP timeout so
		// the client's own timeout fires first, making an ambiguous send a
		// client timeout rather than a context cancellation.
		ctx, cancel := context.WithTimeout(context.Background(), w.svc.Config.OutboundDeliveryAttemptTimeout())
		msgID, claim, err := w.svc.Store.ClaimNextPending(ctx, time.Now().UTC(), w.owner, w.claimLease)
		if err != nil {
			cancel()
			w.log.Error("outbox claim", "error", err)
			return
		}
		if msgID == "" {
			cancel()
			return
		}
		accountID, err := w.svc.AccountIDForMessage(ctx, msgID)
		if err != nil {
			// Release the claim so the message is retried immediately rather
			// than idling until the lease expires.
			_ = w.svc.Store.ReleaseMessageClaim(ctx, msgID, claim)
			cancel()
			w.log.Error("outbox account lookup", "message_id", msgID, "error", err)
			continue
		}
		w.deliverOneMessage(ctx, accountID, msgID, claim)
		cancel()
	}
}

// deliverOneMessage delivers a single claimed message. A panic is caught per
// message and recorded as a failed attempt so a poison message backs off
// instead of exiting the process or being reclaimed and re-panicking hot.
func (w *OutboxWorker) deliverOneMessage(ctx context.Context, accountID, msgID, claim string) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("outbox delivery panic recovered", "message_id", msgID, "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
			// Recording the outcome must never itself escape: it runs inside an
			// active recover, and a second panic here would still exit.
			w.safeFail("outbox delivery", func(outcomeCtx context.Context) {
				_ = w.svc.failMessagePanic(outcomeCtx, accountID, msgID, claim, fmt.Errorf("panic during delivery: %T", x))
			})
		}
	}()
	if err := w.svc.Deliver(ctx, accountID, msgID, claim); err != nil {
		w.log.Warn("outbox delivery failed", "message_id", msgID, "error", err)
	}
}
