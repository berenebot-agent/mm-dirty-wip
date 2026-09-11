package app

import (
	"context"
	"log/slog"
	"time"

	"gatehouse-mail/internal/idgen"
)

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
	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	// Re-scan on startup so pending messages from a previous process resume.
	w.expireApprovals()
	w.deliverDue()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.expireApprovals()
			w.deliverDue()
		}
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
