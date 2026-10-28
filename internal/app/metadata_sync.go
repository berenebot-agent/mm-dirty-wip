package app

import (
	"context"
	"errors"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// MetadataSyncStatus is inbox-wide metadata progress for the signed-in user's
// visible inboxes. Zero Total means unknown, not an empty mailbox. Running and
// queued are process-owned; restart never leaves a durable false spinner.
type MetadataSyncStatus struct {
	InboxID  string `json:"inbox_id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Provider string `json:"provider"`
	State    string `json:"state"`
	Phase    string `json:"phase"`
	Indexed  int64  `json:"indexed"`
	Total    int64  `json:"total"`
	// EstimatedTotal marks Total as approximate: Gmail/One report a count over
	// every label while Mammoth Mail indexes a folder subset, so the denominator is
	// an estimate and the UI shows it as such.
	EstimatedTotal bool      `json:"estimated_total,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	Error          string    `json:"error,omitempty"`
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
		// Total is the provider's own count for the inbox; Indexed is the rows
		// we have actually cached. For Gmail/One these are different populations
		// (the provider counts every label; we index a folder subset), so the
		// denominator is an estimate and the UI marks it as such.
		//
		// Normalise so the pair can never render a nonsense fraction, but only
		// when the provider actually reported a total:
		//  - raise the denominator to meet the numerator, so a pass that indexed
		//    more rows than the provider reported shows N/N rather than N/M with
		//    M<N ("110/100" becomes "110/110");
		//  - when the index is complete, snap the denominator to the indexed
		//    count so the final paint reads M/M instead of a sub-100 plateau.
		// Total 0 still means "unknown" and is left for the UI's bare count.
		if snapshot.Total > 0 {
			if index.Status == store.RemoteIndexComplete || snapshot.Total < snapshot.Indexed {
				snapshot.Total = snapshot.Indexed
			}
		}
		if snapshot.UpdatedAt.IsZero() {
			snapshot.UpdatedAt = index.IndexedAt
		}
		name := box.DisplayName
		if name == "" {
			name = box.Address
		}
		out = append(out, MetadataSyncStatus{InboxID: box.ID, Name: name, Address: box.Address, Provider: provider, State: state, Phase: phase, Indexed: snapshot.Indexed, Total: snapshot.Total, EstimatedTotal: snapshot.Total > 0 && (provider == "One" || provider == "Gmail"), UpdatedAt: snapshot.UpdatedAt, Error: snapshot.Error})
	}
	return out, nil
}

// reconnectInstruction is the durable operator message recorded when a provider
// rejects the inbox's authorization. The UI reads it verbatim as the inbox's
// reconnect prompt, so metadata sync and the cheap detection passes share it.
const reconnectInstruction = "Reconnect this inbox to resume metadata sync"

// reconnectRequired reports an authorization failure that only the operator can
// clear by reconnecting the inbox; a retryable error is not one.
func reconnectRequired(err error) bool {
	var mb *model.MailboxError
	return errors.As(err, &mb) && (mb.Kind == model.ErrKindAuth || mb.Kind == model.ErrKindForbidden)
}

// recordReconnectVerdict makes an auth failure from a cheap detection pass the
// same durable verdict a failed metadata sync records. Without it the reconnect
// banner waits for the next deep reconcile (15 minutes by default) even though
// the poll has been failing every minute, so a freshly broken inbox still looks
// healthy. A later successful reconcile overwrites this state.
func (m *RemoteMailboxService) recordReconnectVerdict(ctx context.Context, account, inbox string) {
	if snapshot, err := m.Service.Store.MetadataSyncSnapshot(ctx, account, inbox); err == nil && snapshot.State == "error" {
		return
	}
	phase := "initial"
	if index, err := m.Service.Store.GetRemoteIndexStatus(ctx, account, inbox); err == nil && index.Status == store.RemoteIndexComplete {
		phase = "refresh"
	}
	_ = m.Service.Store.SaveMetadataSyncState(ctx, account, inbox, "error", phase, reconnectInstruction)
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
				message = reconnectInstruction
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
