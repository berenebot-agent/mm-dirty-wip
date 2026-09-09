package app

import (
	"context"
	"log/slog"
	"time"
)

// OutboxWorker delivers pending outbound messages in the background. It is a
// single goroutine that polls the outbox on an interval, claiming one due
// message at a time and delivering it. On startup it re-scans so messages left
// pending by a previous process are resumed.
type OutboxWorker struct {
	svc    *Service
	log    *slog.Logger
	stop   chan struct{}
	done   chan struct{}
	period time.Duration
}

func NewOutboxWorker(svc *Service, log *slog.Logger) *OutboxWorker {
	if log == nil {
		log = slog.Default()
	}
	return &OutboxWorker{svc: svc, log: log, stop: make(chan struct{}), done: make(chan struct{}), period: 5 * time.Second}
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
	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	// Re-scan on startup so pending messages from a previous process resume.
	w.deliverDue()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.deliverDue()
		}
	}
}

func (w *OutboxWorker) deliverDue() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		msgID, err := w.svc.Store.ClaimNextPending(ctx, time.Now().UTC())
		if err != nil {
			w.log.Error("outbox claim", "error", err)
			return
		}
		if msgID == "" {
			return
		}
		accountID, err := w.svc.AccountIDForMessage(ctx, msgID)
		if err != nil {
			w.log.Error("outbox account lookup", "message_id", msgID, "error", err)
			continue
		}
		if err := w.svc.Deliver(ctx, accountID, msgID); err != nil {
			w.log.Warn("outbox delivery failed", "message_id", msgID, "error", err)
		}
	}
}
