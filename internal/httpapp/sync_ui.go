package httpapp

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// uiInboxSync is the square reload button's target: a quick, user-triggered sync
// of a standalone (IMAP) inbox against its live server. It is available to any
// principal with Read on the inbox, because it performs only header/arrival
// reads; it never mutates remote state and never deletes cached metadata.
//
// It runs the fast INBOX new-mail detection/quick-index and re-syncs the headers
// of the messages currently on screen (so new mail and read/flag changes show
// immediately), then redirects straight back to the same view. It deliberately
// does NOT schedule the deep reconcile: that pass rewrites cached rows and could
// transiently empty a folder, making recent mail vanish from the list while it
// ran. The deep reconcile runs on the worker's own cadence.
func (s *Server) uiInboxSync(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	box, err := s.Service.Store.GetInbox(r.Context(), p, id)
	if err != nil {
		http.Error(w, "inbox not found", 404)
		return
	}
	// Preserve the exact view so the redirect returns the user where they were.
	// The current view travels in the form body (hidden fields), so read it via
	// FormValue after parsing.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	folder := strings.TrimSpace(r.FormValue("folder"))
	label := strings.TrimSpace(r.FormValue("label"))
	before := strings.TrimSpace(r.FormValue("before"))
	folderID := strings.TrimSpace(r.FormValue("folder_id"))

	// A standalone, configured inbox is the only case with a remote server to
	// sync. A domain inbox has no remote; the button is not shown for one, but
	// guard anyway and simply return the user to their view.
	if box.Kind == model.InboxKindStandalone && box.RemoteConfigured && box.Remote != nil {
		// The reload button is deliberately QUICK and NON-DESTRUCTIVE: it runs
		// only the two upsert-only paths (new-mail detection/quick-index and an
		// on-screen header refresh). It must never trigger the deep reconcile
		// here: a background reconcile concurrently rewrites a folder's cached
		// rows (and can transiently empty it), which made recent mail vanish from
		// the list while it ran. The deep reconcile runs on its own cadence.
		//
		// Mark the pass as an explicit manual sync so the completion log reports
		// trigger=manual rather than the watcher's automatic cadence.
		ctx := app.WithManualRemoteSync(r.Context())
		if s.Service.RemoteDetection != nil {
			s.Service.RemoteDetection.DetectInbox(ctx, box)
		}
		ids := s.visibleRemoteMessageIDs(r, p, box, folder, label, before, folderID)
		s.remoteMailbox().RefreshRemoteView(r.Context(), p, id, ids)
	}
	http.Redirect(w, r, redirectInboxView(id, folder, label, before, folderID), 303)
}

// visibleRemoteMessageIDs resolves the ids of the cached remote messages on the
// current view's first page, so a quick sync can refresh exactly what the user
// sees. A custom-folder view resolves through folderMessages; every other view
// through buildMessageList. A resolution failure yields no ids (the deep
// reconcile still runs), never an error to the user.
func (s *Server) visibleRemoteMessageIDs(r *http.Request, p model.Principal, box model.Inbox, folder, label, before, folderID string) []string {
	var msgs []model.Message
	if folder == "folder" {
		folders, ferr := s.Service.Store.ListFolders(r.Context(), p.AccountID, box.ID)
		if ferr != nil {
			return nil
		}
		for i := range folders {
			if folders[i].ID != folderID {
				continue
			}
			page, merr := s.folderMessages(r, p, box, &folders[i], before)
			if merr != nil {
				return nil
			}
			msgs = page
			break
		}
	} else {
		page, _, _, _, lerr := s.buildMessageList(r, p, box.ID, folderOrInbox(folder), label, before)
		if lerr != nil {
			return nil
		}
		msgs = page
	}
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if strings.TrimSpace(m.ID) != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// folderOrInbox maps an empty folder query to the default "inbox" view, which is
// what renderMailbox uses when the page was loaded without an explicit folder.
func folderOrInbox(folder string) string {
	if folder == "" {
		return "inbox"
	}
	return folder
}

// redirectInboxView rebuilds the URL for the view the sync was triggered from, so
// the redirect preserves pagination and custom-folder selection.
func redirectInboxView(id, folder, label, before, folderID string) string {
	switch folder {
	case "", "inbox":
		base := "/ui/inboxes/" + id
		if before != "" {
			base += "?before=" + url.QueryEscape(before)
		}
		return base
	case "sent":
		return inboxViewURL(id+"/sent", before)
	case "spam":
		return inboxViewURL(id+"/spam", before)
	case "trash":
		return inboxViewURL(id+"/trash", before)
	case "drafts":
		return inboxViewURL(id+"/drafts", before)
	case "outbox":
		return inboxViewURL(id+"/outbox", before)
	case "label":
		return "/ui/inboxes/" + id + "/label?name=" + url.QueryEscape(label) + beforeParam(before)
	case "folder":
		u := "/ui/inboxes/" + id + "/folder?folder=" + url.QueryEscape(folderID)
		if before != "" {
			u += "&before=" + url.QueryEscape(before)
		}
		return u
	default:
		return "/ui/inboxes/" + id
	}
}

// inboxViewURL joins a base view path with an optional before cursor.
func inboxViewURL(path, before string) string {
	u := "/ui/inboxes/" + path
	if before != "" {
		u += "?before=" + url.QueryEscape(before)
	}
	return u
}

// beforeParam renders the pagination cursor as a query fragment (with the
// leading separator) or empty when there is none. It is for callers that have
// already opened a query string.
func beforeParam(before string) string {
	if before == "" {
		return ""
	}
	return "&before=" + url.QueryEscape(before)
}
