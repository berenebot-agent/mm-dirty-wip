package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
)

// This file emits the operator-facing "remote sync" completion line for the
// standalone connector family (IMAP, Google/Gmail, Microsoft Graph). A sync is
// logged once when a detection or reconcile pass finishes, whether it was
// triggered by the periodic watcher, an on-demand client read, or an explicit
// manual refresh. The line names the provider, inbox, trigger, duration, new
// arrival count and completion status so an operator can see syncs running and
// tell a manual refresh from the automatic cadence.

// remoteSyncTriggerKey is the unexported context key that marks a sync as
// explicitly user-triggered. A missing value is "auto", so a context that is not
// deliberately marked (the watcher's periodic pass, an on-demand read, or the
// background deep reconcile that ScheduleRefresh runs off its own long-lived
// context) is always reported as automatic.
type remoteSyncTriggerKey struct{}

// WithManualRemoteSync marks ctx as an explicit, user-initiated sync (the UI
// reload button or the blocking refresh API). The marker does not survive a
// goroutine that builds its own context, which is intentional: ScheduleRefresh's
// background deep reconcile runs off the process context and must log as auto
// even when a manual action scheduled it.
func WithManualRemoteSync(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteSyncTriggerKey{}, true)
}

// remoteSyncTrigger reports "manual" for a user-triggered sync and "auto"
// otherwise.
func remoteSyncTrigger(ctx context.Context) string {
	if v, _ := ctx.Value(remoteSyncTriggerKey{}).(bool); v {
		return "manual"
	}
	return "auto"
}

// remoteSyncResult carries the outcome of one pass to the completion log.
type remoteSyncResult struct {
	started time.Time
	new     int
	status  string
	err     error
}

// syncStatus maps an index-completeness status onto the log's status value:
// complete stays "complete"; every other persisted status is "partial"; a
// missing/unknown status is "partial" rather than an empty field.
func syncStatus(status string) string {
	if status == store.RemoteIndexComplete {
		return "complete"
	}
	return "partial"
}

// remoteProviderLabel resolves the connector family label for the provider in
// use. It reads the durable connection rows, so the caller must pass a context
// that is not cancelled (the label is resolved after a timed-out pass may have
// cancelled the pass context).
func (m *RemoteMailboxService) remoteProviderLabel(ctx context.Context, accountID, inboxID string) string {
	if m.IsMicrosoft(ctx, accountID, inboxID) {
		return "microsoft"
	}
	if m.IsGoogle(ctx, accountID, inboxID) {
		return "gmail"
	}
	return "imap"
}

// logRemoteSync writes the single completion line for a pass. status is
// "complete", "partial" or "error"; an error is additionally rendered as an
// error attribute. It never blocks and never fails a pass.
func (m *RemoteMailboxService) logRemoteSync(ctx context.Context, accountID, inboxID string, r remoteSyncResult) {
	logger := m.Service.Log
	if logger == nil {
		logger = slog.Default()
	}
	// The pass context may have been cancelled (per-pass timeout, or a client
	// disconnect on the demand path); resolve the provider label and log under a
	// context that is never cancelled so a completed pass is always reported.
	ctx = context.WithoutCancel(ctx)
	attrs := []any{
		"provider", m.remoteProviderLabel(ctx, accountID, inboxID),
		"inbox_id", inboxID,
		"trigger", remoteSyncTrigger(ctx),
		"duration", time.Since(r.started).Round(time.Millisecond),
		"new", r.new,
		"status", r.status,
	}
	if r.err != nil {
		attrs = append(attrs, "error", r.err.Error())
	}
	logger.Info("remote sync", attrs...)
}
