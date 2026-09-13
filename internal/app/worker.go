package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gatehouse-mail/internal/idgen"
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

// Stop signals the worker to stop after the current delivery completes.
func (w *OutboxWorker) Stop() {
	close(w.stop)
	<-w.done
}

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
	w.expireApprovals()
	w.sweepWorkflows()
	w.sweepMXReceipts()
	w.deliverDue()
	w.deliverWorkflowDue()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.expireApprovals()
			w.sweepWorkflows()
			w.sweepMXReceipts()
			w.deliverDue()
			w.deliverWorkflowDue()
		}
	}
}

// sweepMXReceipts removes MX delivery receipts past their retention horizon.
// Receipts are the retry dedup key; once expired a delivery is treated as new.
func (w *OutboxWorker) sweepMXReceipts() {
	if !w.svc.Config.MXReceiveEnabled {
		return
	}
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
		_ = os.Remove(filepath.Join(w.svc.Config.DataDir, filepath.FromSlash(p)))
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
			cancel()
			w.log.Error("workflow account lookup", "workflow_id", workflowID, "error", err)
			continue
		}
		if err := w.svc.DeliverWorkflow(ctx, accountID, workflowID, w.owner); err != nil {
			w.log.Warn("workflow delivery failed", "workflow_id", workflowID, "error", err)
		}
		cancel()
	}
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
			cancel()
			w.log.Error("outbox account lookup", "message_id", msgID, "error", err)
			continue
		}
		if err := w.svc.Deliver(ctx, accountID, msgID, w.owner); err != nil {
			w.log.Warn("outbox delivery failed", "message_id", msgID, "error", err)
		}
		cancel()
	}
}
