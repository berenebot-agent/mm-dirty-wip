package httpapp

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// uiInboxLive returns one live fragment of a mailbox page — the message list or
// the send-requests card — rendered with the same templates the full page uses.
// The browser swaps the fragment in place when a relevant event arrives, so the
// list stays current without a page reload and without disturbing scroll or an
// in-progress form.
//
// The fragment is bounded to the first page (`before` is never forwarded): a
// live swap only ever touches the top of a list the user is at the top of, so
// re-fetching a deep page would risk replacing content under the reader. The
// folder and optional label come from the query the page itself used, so the
// fragment can only ever describe the same view; an unrecognised folder is
// rejected rather than guessed.
func (s *Server) uiInboxLive(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	part := strings.TrimSpace(r.URL.Query().Get("part"))

	data := pageData{
		Page:      "inbox",
		Principal: p,
		CSRF:      csrf(r),
		Inbox:     &box,
	}

	switch part {
	case "requests":
		// The send-requests card only exists on the inbox folder.
		if folder != "inbox" {
			http.Error(w, "not found", 404)
			return
		}
		requests, err := s.buildSendRequests(r, p, id)
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		data.Folder = "inbox"
		data.SendRequests = requests
		s.renderFragment(w, r, "live-requests-card", data)
	case "list":
		switch folder {
		case "inbox", "sent", "spam", "trash", "label":
		default:
			http.Error(w, "not found", 404)
			return
		}
		msgs, hasMore, cursor, pagerURL, err := s.buildMessageList(r, p, id, folder, label, "")
		if err != nil {
			s.uiError(w, err, 400)
			return
		}
		data.Folder = folder
		data.ActiveLabel = label
		data.Messages = msgs
		data.HasMore = hasMore
		data.Before = cursor
		data.PagerURL = pagerURL
		s.renderFragment(w, r, "live-list-card", data)
	default:
		http.Error(w, "not found", 404)
	}
}

// buildMessageList runs the same query the mailbox renderer does for a folder
// and optional label. before is the keyset cursor (empty for the first page).
func (s *Server) buildMessageList(r *http.Request, p model.Principal, id, folder, label, before string) (msgs []model.Message, hasMore bool, cursor, pagerURL string, err error) {
	direction := "inbound"
	basePath := "/ui/inboxes/" + id
	spamOnly := false
	trashed := false
	switch folder {
	case "sent":
		direction = "outbound"
		basePath += "/sent"
	case "spam":
		spamOnly = true
		basePath += "/spam"
	case "trash":
		trashed = true
		basePath += "/trash"
	}
	var labels []string
	if label != "" {
		basePath += "/label"
		labels = []string{label}
	}
	msgs, err = s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{InboxID: id, Direction: direction, SpamOnly: spamOnly, Trashed: trashed, IncludeSpam: trashed, Labels: labels, Before: before, Limit: inboxPageSize + 1})
	if err != nil {
		return nil, false, "", "", err
	}
	hasMore = len(msgs) > inboxPageSize
	if hasMore {
		msgs = msgs[:inboxPageSize]
	}
	if len(msgs) > 0 {
		cursor = msgs[len(msgs)-1].ID
	}
	if cursor != "" {
		if label != "" {
			pagerURL = basePath + "?name=" + url.QueryEscape(label) + "&before=" + url.QueryEscape(cursor)
		} else {
			pagerURL = basePath + "?before=" + url.QueryEscape(cursor)
		}
	}
	return msgs, hasMore, cursor, pagerURL, nil
}

// buildSendRequests collects the pending send requests for an inbox, matching
// the inbox renderer so the live card and the page agree.
func (s *Server) buildSendRequests(r *http.Request, p model.Principal, id string) ([]SendRequestRow, error) {
	all, err := s.Service.Store.ListSendRequests(r.Context(), p, id, true, 20)
	if err != nil {
		return nil, err
	}
	var out []SendRequestRow
	for _, sr := range all {
		if sr.Status != model.SendRequestPending {
			continue
		}
		d, gerr := s.Service.Store.GetDraft(r.Context(), p, sr.DraftID)
		if gerr != nil {
			continue
		}
		atts, _ := s.Service.Store.ListDraftAttachments(r.Context(), p, d.ID)
		out = append(out, SendRequestRow{Request: sr, Draft: d, SizeBytes: draftDisplaySize(d, atts)})
	}
	return out, nil
}
