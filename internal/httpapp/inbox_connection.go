package httpapp

import (
	"context"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// inboxConnectionState is the per-inbox provider connection health the dashboard
// and the inbox Identity tab surface as a traffic light. It is derived from
// durable state the remote mailbox service already maintains (a One binding's
// status, the metadata-sync snapshot and the remote index status); it never
// performs a live provider call, so a busy dashboard stays cheap.
//
// Connected is deliberately conservative: only a confirmed revoked/expired
// authorization or a missing connection reads as disconnected, not a transient
// network or retryable error (those stay amber "Sync problem" in the header and
// are not a reconnect prompt). State is "connected" or "disconnected".
type inboxConnectionState struct {
	State    string `json:"state"`
	Provider string `json:"provider,omitempty"`
	// Detail is a short, safe operator description shown as the flag/prompt text.
	Detail string `json:"detail,omitempty"`
	// Reconnect is the reconnect affordance: "one" opens the in-place API-key
	// dialog, "oauth" follows the provider's wizard flow, and "" has no action.
	Reconnect string `json:"reconnect,omitempty"`
}

// connectionStateConnected is the healthy state. Only a provider-connected
// standalone inbox carries a state at all; a domain inbox or an IMAP-only
// standalone inbox returns the zero value and renders no light.
const connectionStateConnected = "connected"

// connectionStateDisconnected marks a connection that needs the operator to
// reauthorize this inbox before mail will flow again.
const connectionStateDisconnected = "disconnected"

// inboxConnectionStates builds the connection health for every visible provider
// inbox. It returns a map keyed by inbox id; an inbox with no provider binding
// is absent (the template treats a missing entry as "no light").
func (s *Server) inboxConnectionStates(ctx context.Context, p model.Principal, boxes []model.Inbox) map[string]inboxConnectionState {
	out := make(map[string]inboxConnectionState, len(boxes))
	for _, box := range boxes {
		state, ok := s.inboxConnectionState(ctx, p.AccountID, box)
		if ok {
			out[box.ID] = state
		}
	}
	return out
}

// inboxConnectionState returns the connection health of one inbox. The second
// result is false when the inbox has no provider connection to report (a domain
// inbox, or a standalone inbox that is IMAP-only), so no traffic light renders.
func (s *Server) inboxConnectionState(ctx context.Context, accountID string, box model.Inbox) (inboxConnectionState, bool) {
	if box.Kind != model.InboxKindStandalone {
		return inboxConnectionState{}, false
	}
	rm := s.remoteMailbox()
	provider, label, reconnect := "", "", ""
	switch {
	case s.Service.Store.IsOne(ctx, accountID, box.ID):
		provider, label, reconnect = "one", "Gmail via One", "one"
	case rm.IsGoogle(ctx, accountID, box.ID):
		provider, label, reconnect = "google", "Google / Gmail", "oauth"
	case rm.IsMicrosoft(ctx, accountID, box.ID):
		provider, label, reconnect = "microsoft", "Microsoft / Outlook", "oauth"
	default:
		// A hand-configured IMAP connector is not an OAuth grant; there is no
		// reauthorize flow to offer, so it reports no connection light.
		return inboxConnectionState{}, false
	}
	snapshot, _ := s.Service.Store.MetadataSyncSnapshot(ctx, accountID, box.ID)
	detail, disconnected := disconnectedReason(ctx, s.Service.Store, accountID, box.ID, snapshot)
	if !disconnected {
		return inboxConnectionState{State: connectionStateConnected, Provider: provider, Reconnect: reconnect}, true
	}
	if detail == "" {
		detail = label + " account disconnected. Reconnect to resume syncing."
	}
	return inboxConnectionState{State: connectionStateDisconnected, Provider: provider, Detail: detail, Reconnect: reconnect}, true
}

// disconnectedReason decides whether an inbox's durable state confirms a
// revoked/expired authorization. It is separated from inboxConnectionState so
// the classification is unit-testable without an HTTP handler.
func disconnectedReason(ctx context.Context, st *store.Store, accountID, inboxID string, snapshot store.MetadataSyncSnapshot) (string, bool) {
	// A One binding's own status is authoritative: "attention"/"disconnected"
	// is the service's explicit verdict that the key or Gmail connection no
	// longer authorizes this inbox.
	if b, err := st.OneBinding(ctx, accountID, inboxID); err == nil {
		switch b.Status {
		case "attention", "disconnected":
			if strings.TrimSpace(b.LastError) != "" {
				return b.LastError, true
			}
			return "Gmail via One connection needs attention. Reconnect this inbox.", true
		}
	}
	// A metadata sync that failed on an auth/forbidden verdict carries the
	// service's reconnect message verbatim ("Reconnect this inbox to resume
	// metadata sync"). It is set only for ErrKindAuth/ErrKindForbidden, so a
	// retryable transport failure (which also marks the remote index errored)
	// does NOT read as a disconnect.
	if snapshot.State == "error" && reconnectMessage(snapshot.Error) {
		return snapshot.Error, true
	}
	return "", false
}

// reconnectMessage reports whether a durable status message is the service's
// explicit reconnect instruction rather than a transient retry notice.
func reconnectMessage(message string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	if m == "" {
		return false
	}
	return strings.Contains(m, "reconnect")
}
