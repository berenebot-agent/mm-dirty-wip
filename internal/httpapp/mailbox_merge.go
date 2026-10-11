package httpapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// This file implements the account-wide merged pagination for the common message
// and thread listings. A listing that spans the local store and several remote
// inboxes must present one globally date-sorted stream, not per-inbox
// concatenation, and a single opaque cursor must let a client resume without
// duplicates or gaps. The cursor records per-source progress (each source's own
// native cursor) plus the global stable key of the last item returned, so a later
// page can (a) ask each source for exactly the items strictly after its own
// progress and (b) drop anything not strictly older than the global key, which
// prevents an interleaved item from being returned twice across sources.

// commonCursor is the opaque pagination token for a merged account-wide listing.
// Min is the global stable key ("<RFC3339Nano>|<id>") of the last item returned;
// From maps each source ("local" or an inbox id) to that source's native cursor
// after its last returned item.
type commonCursor struct {
	Min  string            `json:"m,omitempty"`
	From map[string]string `json:"f,omitempty"`
	// Scan records a per-source scan continuation for a source that examined a
	// window without returning any match. It lets the next page resume scanning
	// deeper instead of re-reading the same non-matching window, and lets a match
	// deeper than one scan window eventually be reached. It is only ever set for
	// a source with no returned item in the page, so a deeper scan cursor can
	// never skip an item the merge dropped.
	Scan map[string]string `json:"s,omitempty"`
}

// encodeCommonCursor serializes a cursor to an opaque, URL-safe token.
func encodeCommonCursor(c commonCursor) string {
	if c.Min == "" && len(c.From) == 0 {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCommonCursor parses an opaque token. An unparseable token yields ok=false
// so the caller answers 400 rather than silently starting over.
func decodeCommonCursor(raw string) (commonCursor, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return commonCursor{}, true
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return commonCursor{}, false
	}
	var c commonCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return commonCursor{}, false
	}
	return c, true
}

// sourceCursor returns a source's native cursor from a decoded cursor.
func (c commonCursor) sourceCursor(source string) string {
	if c.From == nil {
		return ""
	}
	return c.From[source]
}

// scanCursor returns a source's scan-continuation cursor from a decoded cursor.
func (c commonCursor) scanCursor(source string) string {
	if c.Scan == nil {
		return ""
	}
	return c.Scan[source]
}

// stableKey builds the global stable key for a message: its effective timestamp
// (RFC3339Nano, UTC) and its id, joined so a lexicographic comparison orders by
// time then id. The timestamp is fixed-width where it matters (seconds and
// nanoseconds are zero-padded by RFC3339Nano's format) so keys sort correctly as
// strings only when the time component is equal-width; the merge therefore sorts
// on the parsed time and uses this key solely for cursor equality/ordering.
func stableKey(ts time.Time, id string) string {
	return ts.UTC().Format(time.RFC3339Nano) + "|" + id
}

// messageSortTime resolves the effective ordering time of a message: the
// received time when known, otherwise the created time. Remote and local
// messages are comparable because both carry a real timestamp.
func messageSortTime(m model.Message) time.Time {
	if m.ReceivedAt != nil {
		return m.ReceivedAt.UTC()
	}
	if !m.CreatedAt.IsZero() {
		return m.CreatedAt.UTC()
	}
	return time.Time{}
}

// mergedItem is one candidate in a merged, globally sorted page.
type mergedItem struct {
	msg    model.Message
	source string
	// cursor is the source's native cursor positioned AFTER this item, used to
	// resume that source on the next page when this item is the last returned.
	cursor string
	ts     time.Time
	key    string
}

// mergeMessages collects candidate items from the local store and every authorized
// remote inbox, then returns one globally date-sorted page and the next cursor.
// A remote source failure is recorded as a per-inbox failure and never fails the
// whole page; that source's cursor is left unadvanced so a retry re-reads it.
func (s *Server) mergeMessages(ctx context.Context, p model.Principal, f store.MessageFilter, folder string, cur commonCursor, limit int) ([]model.Message, string, []model.InboxFailure, error) {
	if limit <= 0 {
		limit = 1
	}
	entries := make([]mergedItem, 0, limit+1)
	var failures []model.InboxFailure

	// Local source.
	localFilter := f
	localFilter.Before = cur.sourceCursor("local")
	localFilter.Limit = limit + 1
	msgs, lerr := s.Service.Store.ListMessages(ctx, p, localFilter)
	if lerr != nil {
		return nil, "", nil, normalizeMailboxStoreError(lerr)
	}
	for _, m := range msgs {
		m = sanitizedMessage(m)
		entries = append(entries, mergedItem{msg: m, source: "local", cursor: m.ID, ts: messageSortTime(m), key: stableKey(messageSortTime(m), m.ID)})
	}

	// Remote sources: every inbox the principal may read. Spam, trashed and
	// direction are local-only concepts with no remote equivalent, so a query
	// using them is answered from the local store alone rather than silently
	// mixing in remote mail that cannot satisfy the filter.
	localOnly := f.SpamOnly || f.Trashed || f.Direction != ""
	boxes, berr := s.readableInboxes(ctx, p)
	if berr != nil {
		return nil, "", nil, berr
	}
	scanCursors := map[string]string{}
	for _, mb := range boxes {
		if localOnly || !mb.routed || !mb.remoteConfigured() {
			continue
		}
		s.demandDetection(ctx, []mailboxBackend{mb})
		// The remote metadata source cannot express from/to/unread/has_attachment/
		// label, so filter in-memory. Scan raw pages (resuming from the last raw
		// item) until enough matches are collected or the source is exhausted, so a
		// window dominated by non-matching rows never yields a short page that
		// hides matching mail behind it. A source that returns no match records a
		// scan continuation (scanCursors) so the next page resumes deeper instead
		// of re-reading the same window and so a match beyond the per-page scan
		// bound is still reached; a source that returns a match advances only to
		// that match (pageMerged), so no dropped match is skipped.
		scanBefore := cur.sourceCursor(mb.inbox.ID)
		if sc := cur.scanCursor(mb.inbox.ID); sc != "" {
			scanBefore = sc
		}
		collected := 0
		exhausted := false
		var rerr error
		for page := 0; page < remoteFilterScanMaxPages && collected <= limit; page++ {
			rawLimit := limit + 1
			if rawLimit < remoteFilterScanPage {
				rawLimit = remoteFilterScanPage
			}
			res, e := mb.remote.ListRemoteMessages(ctx, p, mb.inbox.ID, folder, rawLimit, scanBefore)
			if e != nil {
				rerr = e
				break
			}
			for _, v := range res.Items {
				m := remoteMessageToModel(v, &model.Folder{Path: v.FolderPath})
				if !matchMessageFilter(m, f) {
					continue
				}
				ts := messageSortTime(m)
				entries = append(entries, mergedItem{msg: m, source: mb.inbox.ID, cursor: remoteSourceCursor(v), ts: ts, key: stableKey(ts, m.ID)})
				collected++
				if collected > limit {
					break
				}
			}
			if len(res.Items) < rawLimit || res.NextCursor == "" || res.NextCursor == scanBefore {
				exhausted = true
				break
			}
			scanBefore = res.NextCursor
		}
		if rerr != nil {
			failures = append(failures, model.NewInboxFailure(mb.inbox.ID, rerr))
		} else if collected == 0 && !exhausted && scanBefore != "" {
			// No match in the scanned window and more rows remain: carry the scan
			// position so the next page continues past it.
			scanCursors[mb.inbox.ID] = scanBefore
		}
	}

	items, next := pageMerged(entries, cur, limit, scanCursors)
	return items, next, failures, nil
}

// remoteSourceCursor is the native resume cursor for a remote message: its
// opaque metadata id, which the store resolves to the row's (received_at, id)
// ordering tuple. It must be the id, not the received_at: a received_at is not
// unique (a burst of same-second mail shares one) and would drop same-second
// siblings on the next page, and it is also the value a UI "load older" link
// uses, so mixing the two would silently mis-resume.
func remoteSourceCursor(v app.RemoteMessageView) string {
	return v.ID
}

// pageMerged sorts candidates by (time desc, id desc), drops any item not
// strictly older than the cursor's global key, returns the page, and builds the
// next cursor. Incoming per-source progress is preserved so a source that
// contributed nothing to this page is not reset to its newest window on the next
// request (which would drop its older items after the global-key filter).
func pageMerged(entries []mergedItem, cur commonCursor, limit int, scan map[string]string) ([]model.Message, string) {
	minKey := cur.Min
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].ts.Equal(entries[j].ts) {
			return entries[i].ts.After(entries[j].ts)
		}
		return entries[i].msg.ID > entries[j].msg.ID
	})
	// Drop duplicates: anything not strictly older than the cursor key was
	// already delivered on a previous page.
	if minKey != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if keyBefore(e.key, minKey) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	page := entries
	truncated := false
	if len(page) > limit {
		page = page[:limit]
		truncated = true
	}
	out := make([]model.Message, 0, len(page))
	// Seed the next cursor with the incoming per-source progress, then advance
	// only the sources that actually contributed an item to this page. A source
	// that returned nothing keeps its prior position (or stays unstarted), so the
	// next request resumes it where it left off rather than from the newest item.
	next := commonCursor{Min: minKey, From: map[string]string{}, Scan: map[string]string{}}
	for src, c := range cur.From {
		if c != "" {
			next.From[src] = c
		}
	}
	advanced := map[string]bool{}
	for _, e := range page {
		out = append(out, e.msg)
		next.Min = e.key
		next.From[e.source] = e.cursor
		advanced[e.source] = true
	}
	for src, c := range scan {
		if c == "" || advanced[src] {
			continue
		}
		// Only a source with no returned item carries a scan continuation, so it
		// resumes deeper instead of re-reading the same non-matching window.
		next.Scan[src] = c
	}
	if !truncated && len(next.Scan) == 0 {
		// The last page: no cursor, so a client stops.
		return out, ""
	}
	return out, encodeCommonCursor(next)
}

// keyBefore reports whether key a is strictly OLDER than key b under the global
// descending order (later time first; on a tie, larger id first). "a before b"
// therefore means a.time < b.time, or equal times and a.id < b.id.
func keyBefore(a, b string) bool {
	at, aid := splitStableKey(a)
	bt, bid := splitStableKey(b)
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return aid < bid
}

// splitStableKey parses a "<RFC3339Nano>|<id>" key. A malformed key yields the
// zero time and the whole string, which sorts deterministically.
func splitStableKey(k string) (time.Time, string) {
	i := strings.IndexByte(k, '|')
	if i < 0 {
		return time.Time{}, k
	}
	t, err := time.Parse(time.RFC3339Nano, k[:i])
	if err != nil {
		return time.Time{}, k
	}
	return t, k[i+1:]
}

// mergeThreads merges the local thread listing with every authorized remote
// inbox's threads into one date-sorted page. Each source is asked for exactly the
// threads strictly after its own cursor (a keyset on the source's
// (last_message_at, id) ordering), so a merged page never loses a thread that
// sorts below the global key, and the per-source progress travels in the cursor.
func (s *Server) mergeThreads(ctx context.Context, p model.Principal, folder string, cur commonCursor, limit int) ([]model.Thread, string, []model.InboxFailure, error) {
	if limit <= 0 {
		limit = 1
	}
	type tentry struct {
		th  model.Thread
		key string
		ts  time.Time
		// cursor is the source's native keyset after this thread.
		cursor string
		source string
	}
	var entries []tentry
	local, lerr := s.Service.Store.ListThreadsBefore(ctx, p, "", limit+1, cur.sourceCursor("local"))
	if lerr != nil {
		return nil, "", nil, normalizeMailboxStoreError(lerr)
	}
	for _, th := range local {
		ts := th.LastMessageAt.UTC()
		entries = append(entries, tentry{th: th, ts: ts, key: stableKey(ts, th.ID), cursor: th.ID, source: "local"})
	}
	var failures []model.InboxFailure
	boxes, berr := s.readableInboxes(ctx, p)
	if berr != nil {
		return nil, "", nil, berr
	}
	for _, mb := range boxes {
		if !mb.routed || !mb.remoteConfigured() {
			continue
		}
		s.demandDetection(ctx, []mailboxBackend{mb})
		threads, rerr := mb.remote.ListRemoteThreads(ctx, p, mb.inbox.ID, folder, limit+1, cur.sourceCursor(mb.inbox.ID))
		if rerr != nil {
			failures = append(failures, model.NewInboxFailure(mb.inbox.ID, rerr))
			continue
		}
		for _, th := range threads {
			ts := th.LastMessageAt.UTC()
			entries = append(entries, tentry{th: model.Thread{ID: th.Key, InboxID: th.InboxID, Subject: th.Subject, MessageCount: th.MessageCount, LastMessageAt: th.LastMessageAt}, ts: ts, key: stableKey(ts, th.Key), cursor: th.Key, source: mb.inbox.ID})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].ts.Equal(entries[j].ts) {
			return entries[i].ts.After(entries[j].ts)
		}
		return entries[i].th.ID > entries[j].th.ID
	})
	if cur.Min != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if keyBefore(e.key, cur.Min) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	page := entries
	truncated := false
	if len(page) > limit {
		page = page[:limit]
		truncated = true
	}
	out := make([]model.Thread, 0, len(page))
	// Preserve incoming per-source progress so a source that contributed no
	// thread to this page resumes where it left off rather than from the top.
	next := commonCursor{Min: cur.Min, From: map[string]string{}}
	for src, c := range cur.From {
		if c != "" {
			next.From[src] = c
		}
	}
	for _, e := range page {
		out = append(out, e.th)
		next.Min = e.key
		next.From[e.source] = e.cursor
	}
	if !truncated {
		return out, "", failures, nil
	}
	return out, encodeCommonCursor(next), failures, nil
}

// mergeSearch merges a local FTS5 search with a live server search on every
// authorized remote inbox into one globally date-sorted page. The remote filters
// the server supports (from, to, subject, text, unread, label) are pushed down;
// has_attachment is applied locally on the cached metadata because the remote
// search does not filter on it. Each remote source is asked for exactly the UIDs
// strictly below its own resumable UID cursor (carried in the common cursor), so a
// later page resumes the server search instead of re-reading the newest window.
// Completeness is partial when a remote source participates or a failure occurs.
func (s *Server) mergeSearch(ctx context.Context, p model.Principal, q string, folder, from, to, subject, label string, unread, hasAttachment *bool, cur commonCursor, limit int) ([]model.Message, string, model.ListCompleteness, []model.InboxFailure, error) {
	if limit <= 0 {
		limit = 1
	}
	localFilter := store.MessageFilter{
		From:          from,
		To:            to,
		Unread:        unread,
		HasAttachment: hasAttachment,
		Labels:        nil,
		Before:        cur.sourceCursor("local"),
		Limit:         limit + 1,
	}
	if label != "" {
		localFilter.Labels = []string{label}
	}
	entries := make([]mergedItem, 0, limit+1)
	local, lerr := s.Service.Store.SearchMessagesFiltered(ctx, p, q, localFilter)
	if lerr != nil {
		return nil, "", model.CompletenessUnknown, nil, normalizeMailboxStoreError(lerr)
	}
	for _, m := range local {
		m = sanitizedMessage(m)
		ts := messageSortTime(m)
		entries = append(entries, mergedItem{msg: m, source: "local", cursor: m.ID, ts: ts, key: stableKey(ts, m.ID)})
	}
	var failures []model.InboxFailure
	completeness := model.CompletenessComplete
	boxes, berr := s.readableInboxes(ctx, p)
	if berr != nil {
		return nil, "", model.CompletenessUnknown, nil, berr
	}
	scanCursors := map[string]string{}
	for _, mb := range boxes {
		if !mb.routed || !mb.remoteConfigured() {
			continue
		}
		s.demandDetection(ctx, []mailboxBackend{mb})
		// Resume the live search strictly below the source's own UID cursor. The
		// has_attachment filter is applied in-memory (the server search does not
		// express it), so keep pulling the next raw search page until enough
		// matches are collected or the source is exhausted — never stop at the
		// first short filtered window, which would drop older matching mail.
		// Each item carries its own UID as its native cursor, so a page boundary
		// resumes strictly after the last *returned* match. A source that returns
		// no match carries a scan continuation so the next page continues deeper
		// instead of re-scanning the same window.
		scanUID := uint32(0)
		google := mb.remote.IsGoogle(ctx, p.AccountID, mb.inbox.ID)
		scanProvider := cur.sourceCursor(mb.inbox.ID)
		if raw := cur.scanCursor(mb.inbox.ID); raw != "" {
			scanProvider = raw
		}
		if raw := strings.TrimSpace(cur.sourceCursor(mb.inbox.ID)); raw != "" {
			if n, cerr := strconv.ParseUint(raw, 10, 32); cerr == nil {
				scanUID = uint32(n)
			}
		}
		if raw := strings.TrimSpace(cur.scanCursor(mb.inbox.ID)); raw != "" {
			if n, cerr := strconv.ParseUint(raw, 10, 32); cerr == nil {
				scanUID = uint32(n)
			}
		}
		rawLimit := limit + 1
		if rawLimit < remoteFilterScanPage {
			rawLimit = remoteFilterScanPage
		}
		collected := 0
		exhausted := false
		var rerr error
		for scan := 0; scan < remoteFilterScanMaxPages && collected <= limit; scan++ {
			res, e := mb.remote.SearchRemote(ctx, p, mb.inbox.ID, app.RemoteSearchQuery{
				FolderPath:     folder,
				From:           from,
				To:             to,
				Subject:        subject,
				Text:           q,
				Unread:         unread,
				Label:          label,
				Limit:          rawLimit,
				Cursor:         scanUID,
				ProviderCursor: scanProvider,
			})
			if e != nil {
				rerr = e
				break
			}
			if res.Completeness != model.CompletenessComplete {
				completeness = model.CompletenessPartial
			}
			for _, v := range res.Items {
				if hasAttachment != nil && v.HasAttach != *hasAttachment {
					continue
				}
				m := remoteMessageToModel(v, &model.Folder{Path: v.FolderPath})
				ts := messageSortTime(m)
				// The item's own UID is the resume point after it.
				cur := ""
				if v.UID != 0 {
					cur = strconv.FormatUint(uint64(v.UID), 10)
				}
				if google {
					cur = v.ProviderCursor
				}
				entries = append(entries, mergedItem{msg: m, source: mb.inbox.ID, cursor: cur, ts: ts, key: stableKey(ts, m.ID)})
				collected++
				if collected > limit {
					break
				}
			}
			if collected > limit {
				break
			}
			// The server search is newest-first; NextCursor is the lowest UID it
			// enumerated. If it did not advance (or was exhausted) stop scanning.
			if google {
				if res.ProviderCursor == "" || res.ProviderCursor == scanProvider {
					exhausted = true
					break
				}
				scanProvider = res.ProviderCursor
				continue
			}
			if res.NextCursor == 0 || res.NextCursor == scanUID {
				exhausted = true
				break
			}
			scanUID = res.NextCursor
		}
		if rerr != nil {
			failures = append(failures, model.NewInboxFailure(mb.inbox.ID, rerr))
			completeness = model.CompletenessPartial
		} else if collected == 0 && !exhausted && scanUID != 0 {
			scanCursors[mb.inbox.ID] = strconv.FormatUint(uint64(scanUID), 10)
		}
		if google && collected == 0 && !exhausted && scanProvider != "" {
			scanCursors[mb.inbox.ID] = scanProvider
		}
	}
	items, next := pageMerged(entries, cur, limit, scanCursors)
	if next != "" && len(failures) > 0 {
		completeness = model.CompletenessPartial
	}
	return items, next, completeness, failures, nil
}

// matchMessageFilter reports whether a projected message satisfies the common
// list filters. It is applied in-memory to remote messages (whose source cannot
// express every local filter) so from/to/unread/has_attachment/label are never
// silently ignored. Spam/trash/direction are local-only and are not matched here.
func matchMessageFilter(m model.Message, f store.MessageFilter) bool {
	if f.From != "" && !strings.Contains(strings.ToLower(m.From.Address), strings.ToLower(strings.TrimSpace(f.From))) {
		return false
	}
	if f.To != "" {
		q := strings.ToLower(strings.TrimSpace(f.To))
		hit := false
		for _, addr := range append(append([]string{}, m.To...), m.CC...) {
			if strings.Contains(strings.ToLower(addr), q) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if f.Unread != nil && m.Read == *f.Unread {
		// Unread=true wants unread; a read message fails.
		return false
	}
	if f.HasAttachment != nil && *f.HasAttachment && !m.HasAttachments {
		return false
	}
	for _, want := range f.Labels {
		found := false
		for _, have := range m.Labels {
			if strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(want)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// decodeCursorOrLegacy parses an opaque merged cursor, falling back to treating a
// raw value as a local source cursor. The fallback keeps a plain message-id
// "before" working for a local/legacy caller while the opaque token carries the
// per-source progress for the merged account-wide listing.
func decodeCursorOrLegacy(raw string) (commonCursor, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return commonCursor{}, true
	}
	if c, ok := decodeCommonCursor(raw); ok {
		return c, true
	}
	// Not a merged token: treat it as a local-only cursor. A value that decodes
	// as base64 but not as JSON lands here too, which is the intended fallback.
	return commonCursor{From: map[string]string{"local": raw}}, true
}
