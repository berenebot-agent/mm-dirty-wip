package httpapp

import (
	"context"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// metadataSyncStatusView is the JSON projection of one inbox's live metadata
// sync, exposed by GET /ui/actions (and its bearer twin GET /v1/actions) as the
// `syncs` field. The action-queue popup renders one row per entry and the
// standalone mailbox/folder banner matches the entry for the inbox on screen,
// so this type is the UI's own wire contract rather than the service's internal
// shape.
//
// Indexed is the number of metadata rows cached so far; Total is the provider's
// known total, and 0 means the provider cannot report one. The UI renders
// indexed/total and a percentage when Total is known and just the indexed count
// otherwise, so an unknown denominator is never presented as 0%. UpdatedAt is
// when the status was last refreshed and Error is a short, safe description
// that accompanies a non-empty state ("error", "paused", "waiting").
type metadataSyncStatusView struct {
	InboxID   string    `json:"inbox_id"`
	Name      string    `json:"name,omitempty"`
	Address   string    `json:"address,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	State     string    `json:"state"`
	Phase     string    `json:"phase,omitempty"`
	Indexed   int64     `json:"indexed"`
	Total     int64     `json:"total"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// metadataSyncStatuses projects the remote mailbox service's live sync state
// for the principal's inboxes, one row per inbox and across every remote
// provider. It is best-effort and read-only: a read error yields an empty slice
// so the queue endpoint never fails because sync status is unavailable. The
// slice is always non-nil so `syncs` is a stable JSON array.
func (s *Server) metadataSyncStatuses(ctx context.Context, p model.Principal) []metadataSyncStatusView {
	out := []metadataSyncStatusView{}
	statuses, err := s.remoteMailbox().MetadataSyncStatuses(ctx, p)
	if err != nil {
		return out
	}
	for _, st := range statuses {
		out = append(out, metadataSyncStatusView{
			InboxID:   st.InboxID,
			Name:      st.Name,
			Address:   st.Address,
			Provider:  st.Provider,
			State:     st.State,
			Phase:     st.Phase,
			Indexed:   st.Indexed,
			Total:     st.Total,
			UpdatedAt: st.UpdatedAt,
			Error:     st.Error,
		})
	}
	return out
}
