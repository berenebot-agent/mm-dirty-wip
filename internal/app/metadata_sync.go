package app

import (
	"context"
	"errors"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/one"
)

// MetadataSyncStatus is inbox-wide metadata progress for the signed-in user's
// visible inboxes. Zero Total means unknown, not an empty mailbox. Running and
// queued are process-owned; restart never leaves a durable false spinner.
type MetadataSyncStatus struct {
	InboxID   string    `json:"inbox_id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	Phase     string    `json:"phase"`
	Indexed   int64     `json:"indexed"`
	Total     int64     `json:"total"`
	UpdatedAt time.Time `json:"updated_at"`
	Error     string    `json:"error,omitempty"`
}

func (m *RemoteMailboxService) MetadataSyncStatuses(ctx context.Context, p model.Principal) ([]MetadataSyncStatus, error) {
	boxes, err := m.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return nil, err
	}
	out := []MetadataSyncStatus{}
	for _, box := range boxes {
		if box.Kind != model.InboxKindStandalone || !box.RemoteConfigured {
			continue
		}
		snapshot, err := m.Service.Store.MetadataSyncSnapshot(ctx, p.AccountID, box.ID)
		if err != nil {
			return nil, err
		}
		index, err := m.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, box.ID)
		if err != nil {
			return nil, err
		}
		key := p.AccountID + ":" + box.ID
		m.refreshMu.Lock()
		// Running is the deep index build only: a reconcile in flight, or a
		// background refresh queued to run it. A detection pass is deliberately
		// excluded even though it bumps metadataChecking: a cheap arrival check
		// is not a metadata sync, and reporting it as one flashed the "Refreshing
		// mailbox metadata" banner and the header metadata spinner on every
		// folder open. metadataChecking still marks the action queue's activity;
		// it just no longer claims the index is being rebuilt.
		running, queued := m.reconciling[key] != nil, m.refreshing[key]
		waiting := m.metadataWaiting[key] > 0
		m.refreshMu.Unlock()
		if !running && !queued && index.Status == store.RemoteIndexComplete && (snapshot.State == "" || snapshot.State == "done") {
			continue
		}
		state, phase := snapshot.State, snapshot.Phase
		if phase == "" {
			phase = "initial"
		}
		if state == "" || state == "done" {
			state = "waiting"
		}
		if index.Status == store.RemoteIndexError && snapshot.Error == "" {
			state = "error"
			snapshot.Error = "Metadata sync needs attention"
		}
		if queued {
			state = "queued"
		}
		if running {
			state = "running"
		}
		if running && waiting {
			state = "waiting"
			snapshot.Error = "Waiting for provider request slot"
		}
		if index.Status == store.RemoteIndexComplete && (running || queued) {
			phase = "refresh"
		}
		provider := "IMAP"
		if m.Service.Store.IsOne(ctx, p.AccountID, box.ID) {
			provider = "One"
		} else if m.IsGoogle(ctx, p.AccountID, box.ID) {
			provider = "Gmail"
		} else if m.IsMicrosoft(ctx, p.AccountID, box.ID) {
			provider = "Outlook"
		}
		// Total is the provider's own count for the inbox. It is only withheld
		// when it is internally inconsistent (a pass indexed more rows than the
		// provider reports), because a smaller numerator than denominator would
		// render a nonsense percentage. A refresh pass of an already-complete
		// inbox still re-reads and re-saves a valid total, so it must keep its
		// percentage rather than being flattened to a bare "N indexed" count.
		if snapshot.Total < snapshot.Indexed {
			snapshot.Total = 0
		}
		if snapshot.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = index.IndexedAt
		}
		name := box.DisplayName
		if name == "" {
			name = box.Address
		}
		out = append(out, MetadataSyncStatus{InboxID: box.ID, Name: name, Address: box.Address, Provider: provider, State: state, Phase: phase, Indexed: snapshot.Indexed, Total: snapshot.Total, UpdatedAt: snapshot.UpdatedAt, Error: snapshot.Error})
	}
	return out, nil
}

func (m *RemoteMailboxService) beginMetadataSync(ctx context.Context, account, inbox string) string {
	phase := "initial"
	if index, err := m.Service.Store.GetRemoteIndexStatus(ctx, account, inbox); err == nil && index.Status == store.RemoteIndexComplete {
		phase = "refresh"
	} else if snapshot, err := m.Service.Store.MetadataSyncSnapshot(ctx, account, inbox); err == nil && snapshot.Phase == "refresh" && snapshot.State != "done" {
		phase = "refresh"
	}
	// Persist waiting, not running, so an interrupted process can resume honestly.
	_ = m.Service.Store.SaveMetadataSyncState(ctx, account, inbox, "waiting", phase, "")
	return phase
}

func (m *RemoteMailboxService) finishMetadataSync(ctx context.Context, account, inbox, phase string, result store.RemoteIndexStatus, err error) {
	state, message := "waiting", ""
	if result.Status == store.RemoteIndexComplete && err == nil {
		state = "done"
	}
	if err != nil {
		state, message = "error", "Metadata sync interrupted; will retry"
		if errors.Is(err, store.ErrRemoteMetadataQuota) {
			state, message = "paused", "Metadata storage limit reached"
		}
		var mb *model.MailboxError
		if errors.As(err, &mb) {
			switch mb.Kind {
			case model.ErrKindQuota:
				state, message = "paused", "Provider or metadata storage quota reached"
			case model.ErrKindRetryable:
				state, message = "waiting", "Waiting to retry provider request"
			case model.ErrKindAuth, model.ErrKindForbidden:
				message = "Reconnect this inbox to resume metadata sync"
			}
		}
	}
	// A provider may explicitly pause for storage capacity during a partial pass.
	if snapshot, e := m.Service.Store.MetadataSyncSnapshot(ctx, account, inbox); e == nil && (snapshot.State == "paused" || snapshot.State == "error") && err == nil && state != "done" {
		return
	}
	_ = m.Service.Store.SaveMetadataSyncState(ctx, account, inbox, state, phase, message)
}

func (m *RemoteMailboxService) pauseMetadataSync(ctx context.Context, account, inbox string) {
	phase := "initial"
	if snapshot, err := m.Service.Store.MetadataSyncSnapshot(ctx, account, inbox); err == nil && snapshot.Phase != "" {
		phase = snapshot.Phase
	}
	_ = m.Service.Store.SaveMetadataSyncState(ctx, account, inbox, "paused", phase, "Metadata storage limit reached")
}

func (m *RemoteMailboxService) metadataFolderError(ctx context.Context, account, inbox string) {
	snapshot, err := m.Service.Store.MetadataSyncSnapshot(ctx, account, inbox)
	if err != nil || snapshot.State == "paused" {
		return
	}
	phase := snapshot.Phase
	if phase == "" {
		phase = "initial"
	}
	_ = m.Service.Store.SaveMetadataSyncState(ctx, account, inbox, "error", phase, "Some folders could not be indexed; will retry")
}

func (m *RemoteMailboxService) recordMetadataFolderTotal(account, inbox, folder string, total int64) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	key := account + ":" + inbox
	if m.metadataTotals[key] != nil {
		m.metadataTotals[key][folder] = total
	}
}

// Track short arrival/delta metadata checks too, but never mark an idle watcher
// as downloading and never let an Inbox-only check complete the full backfill.
func (m *RemoteMailboxService) metadataCheck(account, inbox string) func() {
	key := account + ":" + inbox
	m.refreshMu.Lock()
	if m.metadataChecking == nil {
		m.metadataChecking = make(map[string]int)
	}
	m.metadataChecking[key]++
	m.refreshMu.Unlock()
	return func() {
		m.refreshMu.Lock()
		m.metadataChecking[key]--
		if m.metadataChecking[key] == 0 {
			delete(m.metadataChecking, key)
		}
		m.refreshMu.Unlock()
	}
}

func (m *RemoteMailboxService) withMetadataWait(ctx context.Context, account, inbox string) context.Context {
	key := account + ":" + inbox
	return one.WithWaitStatus(ctx, func(waiting bool) {
		m.refreshMu.Lock()
		defer m.refreshMu.Unlock()
		if m.metadataWaiting == nil {
			m.metadataWaiting = make(map[string]int)
		}
		if waiting {
			m.metadataWaiting[key]++
		} else {
			m.metadataWaiting[key]--
		}
		if m.metadataWaiting[key] <= 0 {
			delete(m.metadataWaiting, key)
		}
	})
}
