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
// The fragment is bounded to one page; `before` preserves the user's current
// pagination position when older-history backfill completes. The
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
		before := strings.TrimSpace(r.URL.Query().Get("before"))
		switch folder {
		case "inbox", "sent", "spam", "trash", "label", "folder":
		default:
			http.Error(w, "not found", 404)
			return
		}
		var msgs []model.Message
		var hasMore bool
		var cursor, pagerURL string
		var err error
		if folder == "folder" {
			folders, ferr := s.Service.Store.ListFolders(r.Context(), p.AccountID, id)
			if ferr != nil {
				s.uiError(w, ferr, 400)
				return
			}
			found := false
			for i := range folders {
				if folders[i].ID == r.URL.Query().Get("folder_id") {
					found = true
					msgs, err = s.folderMessages(r, p, box, &folders[i], before)
					hasMore = len(msgs) > inboxPageSize
					if hasMore {
						msgs = msgs[:inboxPageSize]
					}
					if len(msgs) > 0 {
						cursor = msgs[len(msgs)-1].ID
						pagerURL = "/ui/inboxes/" + id + "/folder?folder=" + url.QueryEscape(folders[i].ID) + "&before=" + url.QueryEscape(cursor)
					}
					break
				}
			}
			if !found {
				http.Error(w, "folder not found", 404)
				return
			}
		} else {
			msgs, hasMore, cursor, pagerURL, err = s.buildMessageList(r, p, id, folder, label, before)
		}
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

// mailboxFilter maps a UI folder name and optional label to the store filter
// and base path shared by the mailbox list, its total count and the bulk-by-
// scope enumeration, so all three describe exactly the same set. ok is false
// for an unrecognised folder, which callers turn into a 404 rather than a
// guessed view.
func mailboxFilter(id, folder, label string) (f store.MessageFilter, basePath string, ok bool) {
	f = store.MessageFilter{InboxID: id}
	basePath = "/ui/inboxes/" + id
	switch folder {
	case "inbox":
		f.Direction = "inbound"
	case "sent":
		f.Direction = "outbound"
		basePath += "/sent"
	case "spam":
		f.SpamOnly = true
		basePath += "/spam"
	case "trash":
		f.Trashed = true
		f.IncludeSpam = true
		basePath += "/trash"
	case "label":
		if label == "" {
			return f, basePath, false
		}
		f.Direction = "inbound"
		f.Labels = []string{label}
		basePath += "/label"
	default:
		return f, basePath, false
	}
	return f, basePath, true
}

// buildMessageList runs the same query the mailbox renderer does for a folder
// and optional label. before is the keyset cursor (empty for the first page). For
// a standalone inbox it reads the cached remote index (reconciling on demand),
// so the mailbox Inbox/Sent/Spam/Trash views show remote mail through the same UI.
func (s *Server) buildMessageList(r *http.Request, p model.Principal, id, folder, label, before string) (msgs []model.Message, hasMore bool, cursor, pagerURL string, err error) {
	f, basePath, ok := mailboxFilter(id, folder, label)
	if !ok {
		return nil, false, "", "", store.ErrInvalidSearchQuery
	}
	if box, gerr := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id); gerr == nil && box.Kind == model.InboxKindStandalone && box.RemoteConfigured && box.Remote != nil {
		return s.buildRemoteMessageList(r, p, box, folder, before)
	}
	f.Before = before
	f.Limit = inboxPageSize + 1
	msgs, err = s.Service.Store.ListMessages(r.Context(), p, f)
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

// buildRemoteMessageList renders a standalone inbox's remote folder listing onto
// the common message list shape. folder selects the local view role ("inbox",
// "sent", "spam", "trash", "label"); the corresponding remote folder path comes
// from the inbox's role mapping, falling back to the selected namespace root for
// the Inbox. A label filter intersects with local label metadata.
func (s *Server) buildRemoteMessageList(r *http.Request, p model.Principal, box model.Inbox, folder, before string) (msgs []model.Message, hasMore bool, cursor, pagerURL string, err error) {
	root := strings.TrimSpace(box.Namespace)
	if root == "" {
		root = model.NamespaceINBOX
	}
	path := root
	basePath := "/ui/inboxes/" + box.ID
	switch folder {
	case "sent":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleSent); ok {
			path = f.Path
		}
		basePath += "/sent"
	case "spam":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleSpam); ok {
			path = f.Path
		}
		basePath += "/spam"
	case "trash":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleTrash); ok {
			path = f.Path
		}
		basePath += "/trash"
	case "inbox":
		if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, box.ID, model.FolderRoleInbox); ok {
			path = f.Path
		}
	}
	res, rerr := s.remoteMailbox().ListRemoteMessagesCached(r.Context(), p, box.ID, path, inboxPageSize+1, before)
	if rerr != nil {
		return nil, false, "", "", rerr
	}
	for _, v := range res.Items {
		msgs = append(msgs, remoteMessageToModel(v, &model.Folder{Path: path}))
	}
	hasMore = len(msgs) > inboxPageSize
	if hasMore {
		msgs = msgs[:inboxPageSize]
	}
	if len(msgs) > 0 {
		cursor = msgs[len(msgs)-1].ID
	}
	if cursor != "" {
		pagerURL = basePath + "?before=" + url.QueryEscape(cursor)
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
