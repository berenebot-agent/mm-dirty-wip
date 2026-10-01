package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/safepath"
	"github.com/dellarb/mailmoose/internal/store"
)

// workflowRetention is how long a terminal workflow job (and its retained raw
// MIME) is kept before the sweep removes it. It matches the outbound delivery
// log's 30-day floor.
const workflowRetention = 30 * 24 * time.Hour

// OutboxWorker delivers pending outbound messages in the background. It is a
// single goroutine that polls the outbox on an interval, claiming one due
// message at a time and delivering it. On startup it recovers claims left by a
// previous process and re-scans so pending messages resume.
type OutboxWorker struct {
	svc    *Service
	log    *slog.Logger
	stop   chan struct{}
	done   chan struct{}
	period time.Duration
	owner  string
}

func NewOutboxWorker(svc *Service, log *slog.Logger) *OutboxWorker {
	if log == nil {
		log = slog.Default()
	}
	return &OutboxWorker{svc: svc, log: log, stop: make(chan struct{}), done: make(chan struct{}), period: 5 * time.Second, owner: idgen.New("wrk")}
}

// Start launches the worker loop. It returns immediately.
func (w *OutboxWorker) Start() {
	go w.run()
}

// SetPeriod overrides the poll interval (used by tests).
func (w *OutboxWorker) SetPeriod(d time.Duration) { w.period = d }

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h := NewWebhookWorker(w.svc)
	for i := 0; i < 64 && !w.stopping(); i++ {
		err := h.RunOnce(ctx)
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
		{"deliverDue", w.deliverDue},
		{"deliverWorkflowDue", w.deliverWorkflowDue},
		{"deliverWebhooks", w.deliverWebhooks},
	} {
		_ = w.recoverUnit(step.name, step.fn)
	}
}

// maintain runs the infrequent maintenance sweeps: approval expiry, terminal
// workflow redaction/retention and MX receipt sweeping.
func (w *OutboxWorker) maintain() {
	for _, step := range []struct {
		name string
		fn   func()
	}{
		{"expireApprovals", w.expireApprovals},
		{"sweepWorkflows", w.sweepWorkflows},
		{"sweepMXReceipts", w.sweepMXReceipts},
		{"sweepClientDeliveryLog", w.sweepClientDeliveryLog},
	} {
		_ = w.recoverUnit(step.name, step.fn)
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

func (w *OutboxWorker) deliverWorkflowDue() {
	for {
		if w.stopping() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		workflowID, err := w.svc.Store.ClaimNextWorkflow(ctx, time.Now().UTC(), w.owner, 15*time.Minute)
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
			_ = w.svc.Store.ReleaseWorkflowClaim(ctx, workflowID, w.owner)
			cancel()
			w.log.Error("workflow account lookup", "workflow_id", workflowID, "error", err)
			continue
		}
		w.deliverOneWorkflow(ctx, accountID, workflowID)
		cancel()
	}
}

// deliverOneWorkflow delivers a single claimed workflow job. A panic is caught
// per job and recorded as a failed attempt, so a poison job backs off instead
// of exiting the process or being reclaimed and re-panicking in a tight loop.
func (w *OutboxWorker) deliverOneWorkflow(ctx context.Context, accountID, workflowID string) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("workflow delivery panic recovered", "workflow_id", workflowID, "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
			// Recording the outcome must never itself escape: it runs inside an
			// active recover, and a second panic here would still exit.
			w.safeFail("workflow delivery", func(outcomeCtx context.Context) {
				_ = w.svc.failWorkflowPanic(outcomeCtx, accountID, workflowID, fmt.Errorf("panic during workflow delivery: %T", x))
			})
		}
	}()
	if err := w.svc.DeliverWorkflow(ctx, accountID, workflowID, w.owner); err != nil {
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
	for {
		// Stop promptly on shutdown instead of draining a whole backlog first.
		if w.stopping() {
			return
		}
		// Each message gets its own deadline so one slow delivery cannot consume
		// the whole loop's budget.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		msgID, err := w.svc.Store.ClaimNextPending(ctx, time.Now().UTC(), w.owner, 15*time.Minute)
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
			_ = w.svc.Store.ReleaseMessageClaim(ctx, msgID, w.owner)
			cancel()
			w.log.Error("outbox account lookup", "message_id", msgID, "error", err)
			continue
		}
		w.deliverOneMessage(ctx, accountID, msgID)
		cancel()
	}
}

// deliverOneMessage delivers a single claimed message. A panic is caught per
// message and recorded as a failed attempt so a poison message backs off
// instead of exiting the process or being reclaimed and re-panicking hot.
func (w *OutboxWorker) deliverOneMessage(ctx context.Context, accountID, msgID string) {
	defer func() {
		if x := recover(); x != nil {
			w.log.Error("outbox delivery panic recovered", "message_id", msgID, "type", fmt.Sprintf("%T", x), "stack", string(debug.Stack()))
			// Recording the outcome must never itself escape: it runs inside an
			// active recover, and a second panic here would still exit.
			w.safeFail("outbox delivery", func(outcomeCtx context.Context) {
				_ = w.svc.failMessagePanic(outcomeCtx, accountID, msgID, fmt.Errorf("panic during delivery: %T", x))
			})
		}
	}()
	if err := w.svc.Deliver(ctx, accountID, msgID, w.owner); err != nil {
		w.log.Warn("outbox delivery failed", "message_id", msgID, "error", err)
	}
}
