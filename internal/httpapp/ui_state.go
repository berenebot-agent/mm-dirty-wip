package httpapp

import (
	"context"
	"net/http"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// uiState is the lightweight, role-scoped snapshot the browser reconciles
// against when a durable event arrives. It deliberately returns only values
// that can change independently of the rendered page — counts, badges and the
// in-memory receiver traffic light — and never performs DNS lookups, so a busy
// inbox produces cheap, bounded reads rather than repeated fresh checks. The
// response is never cached.
func (s *Server) uiState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := principal(r)
	switch r.URL.Query().Get("page") {
	case "dashboard":
		// The inbound traffic light needs a bounded published-MX lookup per
		// Dial MX domain, so it is computed only when the caller asks (on first
		// load, reconnect, and on a receiver-health notification). An ordinary
		// mail event refreshes counts alone, keeping the common path DNS-free.
		s.uiStateDashboard(w, r, p, r.URL.Query().Get("lights") == "1")
	case "inbox":
		s.uiStateInbox(w, r, p)
	default:
		writeError(w, 400, "unknown page")
	}
}

// inboxUnreadCounts returns the merged per-inbox unread counts for a principal:
// the local messages-table counts plus, for every standalone (remote) inbox, the
// count from the cached remote index. Standalone mail never lands in the local
// messages table, so without the remote merge a standalone inbox would always
// report zero unread on the dashboard and the sidebar. A standalone id present in
// the remote result overrides any (necessarily zero) local count.
func (s *Server) inboxUnreadCounts(ctx context.Context, p model.Principal) map[string]int {
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
	if unread == nil {
		unread = map[string]int{}
	}
	if remote, err := s.Service.Store.RemoteInboxUnreadCounts(ctx, p); err == nil {
		for id, n := range remote {
			unread[id] = n
		}
	}
	return unread
}

// dashboardReceivingState is the live state of one dashboard Receiving cell: the
// aggregate traffic light (empty when the domain names no receiver target) and
// the provider label the cell shows. The label is what keeps a row honest when a
// receiving dialog stores a provider while the row is already on screen.
type dashboardReceivingState struct {
	Light string `json:"light"`
	Title string `json:"title"`
	Label string `json:"label"`
}

// uiStateDashboard returns per-inbox unread and pending-send counts plus, when
// includeLights is set, the per-domain inbound traffic light and receiving
// provider label. It is keyed by inbox/domain id so the browser can update one
// cell without re-rendering the dashboard. includeLights is false for a
// mail-triggered refresh so those reads stay DNS-free; the browser asks for
// lights on load, reconnect and a receiver-health notification.
func (s *Server) uiStateDashboard(w http.ResponseWriter, r *http.Request, p model.Principal, includeLights bool) {
	ctx := r.Context()
	unread := s.inboxUnreadCounts(ctx, p)
	pending, _ := s.Service.Store.PendingDraftCountsByInbox(ctx, p)
	if pending == nil {
		pending = map[string]int{}
	}
	type inboxCounts struct {
		Unread  int `json:"unread"`
		Pending int `json:"pending"`
	}
	counts := map[string]inboxCounts{}
	for id, n := range unread {
		c := counts[id]
		c.Unread = n
		counts[id] = c
	}
	for id, n := range pending {
		c := counts[id]
		c.Pending = n
		counts[id] = c
	}
	var lights map[string]dashboardReceivingState
	if includeLights {
		lights = map[string]dashboardReceivingState{}
		if domains, err := s.Service.Store.ListDomains(ctx, p.AccountID); err == nil {
			for _, d := range domains {
				// The label travels beside the light so the client patches both
				// together. It is carried even when no light is drawn, because a
				// receiving dialog stores the provider before any receiver can
				// report ready: a setup started and cancelled part-way leaves a
				// configured domain whose row would otherwise keep reading "Add".
				state := dashboardReceivingState{Label: s.receivingCellLabel(ctx, d)}
				state.Light, state.Title = s.domainInboundLight(ctx, d.AccountID, d.ID, d.Name, d.ReceivingProvider)
				if state.Light == "" && state.Label == "" {
					continue
				}
				lights[d.ID] = state
			}
		}
	}
	// The sending light is the DKIM published-record check for a Direct MX
	// domain that signs. It is scoped to the principal's account: the watcher
	// tracks every account's sending domains, so a foreign domain must never
	// leak into this snapshot.
	var sending map[string]domainLight
	if includeLights {
		sending = map[string]domainLight{}
		if domains, err := s.Service.Store.ListDirectMXSendingDomains(ctx); err == nil {
			for _, d := range domains {
				if d.AccountID != p.AccountID {
					continue
				}
				if light, title := s.dkimSendingLight(d.ID); light != "" {
					sending[d.ID] = domainLight{Light: light, Title: title}
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"page":    "dashboard",
		"inboxes": counts,
		"domains": lights,
		"sending": sending,
	})
}

// uiStateInbox returns the folder badges and label unread counts for one inbox,
// enforcing the same Read role the inbox pages require.
func (s *Server) uiStateInbox(w http.ResponseWriter, r *http.Request, p model.Principal) {
	ctx := r.Context()
	id := r.URL.Query().Get("inbox")
	if id == "" || !p.CanRead(id) {
		writeError(w, 404, "not found")
		return
	}
	// Confirm the inbox exists in this account: an account Admin's CanRead is
	// true for any id, so without this an unknown id would return empty counts
	// instead of a 404.
	if _, err := s.Service.Store.GetInbox(ctx, p, id); err != nil {
		writeError(w, 404, "not found")
		return
	}
	unread := s.inboxUnreadCounts(ctx, p)
	drafts, _ := s.Service.Store.CountDrafts(ctx, p, id)
	outbox, _ := s.Service.Store.CountOutbox(ctx, p, id)
	spam, _ := s.Service.Store.CountSpam(ctx, p, id)
	trash, _ := s.Service.Store.CountTrash(ctx, p, id)
	labels, _ := s.Service.Store.InboxLabelUnreadCounts(ctx, p, id)
	if labels == nil {
		labels = map[string]int{}
	}
	out := map[string]any{
		"page":   "inbox",
		"inbox":  id,
		"unread": unread[id],
		"drafts": drafts,
		"outbox": outbox,
		"spam":   spam,
		"trash":  trash,
		"labels": labels,
	}
	// A standalone inbox's counts come from the cached remote metadata index,
	// which the reconcile refreshes. Report staleness so a live snapshot never
	// presents a stale unread count as current: remote_status is the last index
	// state and remote_indexed_at is when it was last built (zero if never).
	box, gerr := s.Service.Store.GetInboxInternal(ctx, p.AccountID, id)
	if gerr == nil && box.Kind == model.InboxKindStandalone {
		if remoteLabels, e := s.Service.Store.RemoteInboxLabelUnreadCounts(ctx, p.AccountID, id); e == nil {
			out["labels"] = remoteLabels
		}
		out["remote"] = true
		out["remote_syncing"] = s.remoteMailbox().Refreshing(p.AccountID, id)
		if status, serr := s.Service.Store.GetRemoteIndexStatus(ctx, p.AccountID, id); serr == nil {
			out["remote_status"] = status.Status
			if !status.IndexedAt.IsZero() {
				out["remote_indexed_at"] = status.IndexedAt
			}
		} else {
			out["remote_status"] = ""
		}
	}
	writeJSON(w, 200, out)
}

// domainInboundLight computes a domain's aggregate inbound traffic light. It
// reads the in-memory receiver statuses (each receiver reports ready only after
// proving both domain authority and MX routing), and folds in the domain's
// shared "extra MX" verdict — the watcher's single value, not a fresh lookup — so
// an amber light and the status dialog's warning always agree. It returns an
// empty light for a domain that does not receive by Dial MX (those have no
// dashboard light), for a Dial MX domain that names no receiver target (an
// abandoned setup has nothing to report), or when no manager is wired. A
// rotated-away key's authorization is excluded so a stale row cannot present as
// current readiness.
func (s *Server) domainInboundLight(ctx context.Context, accountID, domainID, domainName, receivingProvider string) (string, string) {
	if normalizeDomainProvider(receivingProvider) != "dialmx" || s.Service.DialMX == nil {
		return "", ""
	}
	// A light only means something for a domain that actually names something to
	// deliver to. A Dial MX domain whose Tusk receiver set is empty — a Tusk
	// setup started and abandoned, or a manifest that advertised no receivers —
	// must not be painted red "mail delivery is blocked" for a setup that was never
	// finished.
	if !s.dialMXHasReceiverTargets(ctx, accountID, domainID) {
		return "", ""
	}
	keyID := ""
	if cred, err := s.Service.Store.GetDialMXCredential(ctx, accountID, domainID); err == nil {
		keyID = cred.KeyID
	}
	statuses := s.Service.DialMX.Status(domainName)
	views := make([]mxdialStatusView, 0, len(statuses))
	for _, st := range statuses {
		if keyID != "" && st.KeyID != "" && st.KeyID != keyID {
			continue
		}
		v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: st.SMTPHostname, HTTPCode: st.HTTPCode}
		if !st.ExpiresAt.IsZero() {
			t := st.ExpiresAt
			v.ExpiresAt = &t
		}
		views = append(views, v)
	}
	// Only a domain with an active receiver can be amber-on-extra-MX; a red
	// domain stays red. The verdict itself is the watcher's single shared value,
	// so the light and the status dialog never disagree.
	var unexpected []string
	if anyReady(views) {
		unexpected = s.dialMXExtraMX(domainID)
	}
	return dialMXHealth(views, unexpected, time.Now())
}

// anyReady reports whether at least one receiver row is a live, unexpired ready
// state, matching dialMXHealth's own readiness test.
func anyReady(statuses []mxdialStatusView) bool {
	now := time.Now()
	for _, st := range statuses {
		if st.State == mxdial.StatusActive && (st.ExpiresAt == nil || st.ExpiresAt.After(now)) {
			return true
		}
	}
	return false
}
