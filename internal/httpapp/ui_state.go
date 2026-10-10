package httpapp

import (
	"context"
	"net/http"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
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
	writeJSON(w, 200, map[string]any{
		"page":   "inbox",
		"inbox":  id,
		"unread": unread[id],
		"drafts": drafts,
		"outbox": outbox,
		"spam":   spam,
		"trash":  trash,
		"labels": labels,
	})
}

// domainInboundLight computes a domain's aggregate inbound traffic light. It
// reads the in-memory receiver statuses and runs the same published-MX check
// the rendered dashboard uses, so the live light and the freshly rendered one
// agree. It returns an empty light for a domain that does not receive by Dial
// MX (those have no dashboard light) or when no manager is wired. A
// rotated-away key's authorization is excluded so a stale row cannot present as
// current readiness.
//
// The published-MX lookup is the only costly step, so the whole result is cached
// briefly (lightCacheTTL). The cache key folds in the key and receiver hostnames
// the light depends on, so a key rotation or a receiving-configuration change
// misses the cache instead of serving a stale light. This keeps a burst of
// snapshot calls (a reconnect, several tabs, a flapping receiver) from each
// running a fresh lookup per domain. The setup dialog's own checks do not use
// this cache and remain live.
func (s *Server) domainInboundLight(ctx context.Context, accountID, domainID, domainName, receivingProvider string) (string, string) {
	if normalizeDomainProvider(receivingProvider) != "dialmx" || s.Service.DialMX == nil {
		return "", ""
	}
	keyID := ""
	if cred, err := s.Service.Store.GetDialMXCredential(ctx, accountID, domainID); err == nil {
		keyID = cred.KeyID
	}
	var mxExpected []dialMXMXInstruction
	if cfg, err := s.Service.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, "dialmx"); err == nil {
		if values, derr := s.Service.DecryptDomainReceivingConfig(cfg); derr == nil {
			for _, r := range app.AntlerReceiversFromConfig(values) {
				mxExpected = append(mxExpected, dialMXMXInstruction{Hostname: r.SMTPHostname, Priority: r.MXPriority})
			}
		}
	}
	cacheKey := domainID + "\x00" + keyID + "\x00" + formatMXExpected(mxExpected)
	now := time.Now()
	if light, title, ok := s.lightCacheGet(cacheKey, now); ok {
		return light, title
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
	mxChecked := false
	var mxMatched []string
	if len(mxExpected) > 0 {
		mxChecked = true
		mxMatched = s.dns.checkMX(domainName, mxExpected).Matched
	}
	light, title := dialMXHealth(views, mxChecked, mxMatched, now)
	s.lightCachePut(cacheKey, light, title, now.Add(lightCacheTTL))
	return light, title
}

// lightCacheTTL bounds how long a live dashboard light is reused. It is far
// below the reconnect cadence a single tab produces, so a real change is still
// seen within one snapshot while a burst collapses onto one lookup per domain.
const lightCacheTTL = 10 * time.Second

func (s *Server) lightCacheGet(key string, now time.Time) (string, string, bool) {
	s.lightMu.Lock()
	defer s.lightMu.Unlock()
	e, ok := s.lightCache[key]
	if !ok || now.After(e.expires) {
		return "", "", false
	}
	return e.light, e.title, true
}

func (s *Server) lightCachePut(key, light, title string, expires time.Time) {
	s.lightMu.Lock()
	defer s.lightMu.Unlock()
	if s.lightCache == nil {
		s.lightCache = map[string]lightCacheEntry{}
	}
	if len(s.lightCache) >= 512 {
		delete(s.lightCache, key)
		return
	}
	s.lightCache[key] = lightCacheEntry{light: light, title: title, expires: expires}
}
