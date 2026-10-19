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
	// Label is the human provider name for this connection ("One",
	// "Google / Gmail", "Microsoft / Outlook"). It names the connection in the
	// prompt text, not on the reconnect control.
	Label string `json:"label,omitempty"`
	// ActionLabel is the exact text of the reconnect control ("Reconnect with
	// One" / "Reconnect Google / Gmail" / "Reconnect Microsoft / Outlook"),
	// rendered verbatim so each provider can name its own action.
	ActionLabel string `json:"action_label,omitempty"`
	// Detail is a short, safe operator description shown as the flag/prompt text.
	Detail string `json:"detail,omitempty"`
	// Reconnect is the reconnect affordance: "one" opens the in-place API-key
	// dialog, "google" and "microsoft" follow that provider's wizard flow,
	// "imap" opens the inbox's connector settings, and "" has no action. The
	// kind names the provider, so each one gets its own control rather than
	// sharing a generic OAuth one.
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

// connectionStatesFor reports the same per-inbox connection health for every
// inbox the principal can see, for the /ui/actions poll. The browser renders the
// reconnect banner the server renders from this payload, so an inbox whose
// authorization is rejected while its page is already open gets the prompt and
// its reconnect control without a page load. The map is never nil, so the JSON
// field is a stable object.
func (s *Server) connectionStatesFor(ctx context.Context, p model.Principal) map[string]inboxConnectionState {
	out := map[string]inboxConnectionState{}
	boxes, err := s.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return out
	}
	return s.inboxConnectionStates(ctx, p, boxes)
}

// inboxConnectionState returns the connection health of one inbox. The second
// result is false when the inbox has no connection to report (a domain inbox, or
// a standalone inbox with no remote connector), so no traffic light renders.
func (s *Server) inboxConnectionState(ctx context.Context, accountID string, box model.Inbox) (inboxConnectionState, bool) {
	if box.Kind != model.InboxKindStandalone {
		return inboxConnectionState{}, false
	}
	rm := s.remoteMailbox()
	provider, label, action, reconnect := "", "", "", ""
	switch {
	case s.Service.Store.IsOne(ctx, accountID, box.ID):
		provider, label, action, reconnect = "one", "One", "Reconnect with One", "one"
	case rm.IsGoogle(ctx, accountID, box.ID):
		provider, label, action, reconnect = "google", "Google / Gmail", "Reconnect Google / Gmail", "google"
	case rm.IsMicrosoft(ctx, accountID, box.ID):
		provider, label, action, reconnect = "microsoft", "Microsoft / Outlook", "Reconnect Microsoft / Outlook", "microsoft"
	case box.RemoteConfigured && box.Remote != nil:
		// A hand-configured connector has no OAuth grant, but a rejected
		// credential still needs the operator: the repair is the connector
		// settings page, where the password or app password is stored.
		provider, label, action, reconnect = "imap", "IMAP", "Reconnect IMAP", "imap"
	default:
		return inboxConnectionState{}, false
	}
	snapshot, _ := s.Service.Store.MetadataSyncSnapshot(ctx, accountID, box.ID)
	detail, disconnected := disconnectedReason(ctx, s.Service.Store, accountID, box.ID, snapshot)
	if !disconnected {
		return inboxConnectionState{State: connectionStateConnected, Provider: provider, Label: label, ActionLabel: action, Reconnect: reconnect}, true
	}
	if detail == "" {
		detail = label + " account disconnected. Reconnect to resume syncing."
	}
	return inboxConnectionState{State: connectionStateDisconnected, Provider: provider, Label: label, ActionLabel: action, Detail: detail, Reconnect: reconnect}, true
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
