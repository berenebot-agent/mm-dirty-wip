package httpapp

import (
	"context"
	"net/http"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
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

// uiStateDashboard returns per-inbox unread and pending-send counts plus, when
// includeLights is set, the per-domain inbound traffic light. It is keyed by
// inbox/domain id so the browser can update one cell without re-rendering the
// dashboard. includeLights is false for a mail-triggered refresh so those reads
// stay DNS-free; the browser asks for lights on load, reconnect and a
// receiver-health notification.
func (s *Server) uiStateDashboard(w http.ResponseWriter, r *http.Request, p model.Principal, includeLights bool) {
	ctx := r.Context()
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
	if unread == nil {
		unread = map[string]int{}
	}
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
	type domainLight struct {
		Light string `json:"light"`
		Title string `json:"title"`
	}
	var lights map[string]domainLight
	if includeLights {
		lights = map[string]domainLight{}
		if domains, err := s.Service.Store.ListDomains(ctx, p.AccountID); err == nil {
			for _, d := range domains {
				light, title := s.domainInboundLight(ctx, d.AccountID, d.ID, d.Name, d.ReceivingProvider)
				if light == "" {
					continue
				}
				lights[d.ID] = domainLight{Light: light, Title: title}
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"page":    "dashboard",
		"inboxes": counts,
		"domains": lights,
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
	unread, _ := s.Service.Store.UnreadCounts(ctx, p)
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
// reads the in-memory receiver statuses only: a receiver reports ready only
// after proving both domain authority and MX routing, so no published-MX lookup
// is needed and the read is cheap enough to compute on every snapshot. It
// returns an empty light for a domain that does not receive by Dial MX (those
// have no dashboard light) or when no manager is wired. A rotated-away key's
// authorization is excluded so a stale row cannot present as current readiness.
func (s *Server) domainInboundLight(ctx context.Context, accountID, domainID, domainName, receivingProvider string) (string, string) {
	if normalizeDomainProvider(receivingProvider) != "dialmx" || s.Service.DialMX == nil {
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
		v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: st.SMTPHostname}
		if !st.ExpiresAt.IsZero() {
			t := st.ExpiresAt
			v.ExpiresAt = &t
		}
		views = append(views, v)
	}
	return dialMXHealth(views, time.Now())
}
