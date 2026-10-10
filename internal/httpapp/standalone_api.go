package httpapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/limits"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// This file is the common standalone/remote API surface. Its routes dispatch on
// the inbox kind through mailboxBackend, so a domain inbox and a standalone
// inbox are read through one set of endpoints that return the shared list
// envelope (items, next_cursor, completeness, errors). It preserves parity with
// the existing /v1/messages read/content/raw/attachments routes and the
// /ui/messages/{id} page, which keep their shape.
//
// Every route here enforces the same per-inbox role ladder the rest of the API
// uses; a remote read never triggers a write and a state change requires
// Assistant or Owner.

// commonEnvelope is the shared paginated list shape every common listing route
// returns. Items holds the page; NextCursor is an opaque token for the next page
// (empty on the last page); Completeness describes the result set as a whole and
// is orthogonal to pagination; Errors carries per-inbox failures so one failing
// mailbox in a fan-out does not fail the whole request.
type commonEnvelope struct {
	Items        any                    `json:"items"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Completeness model.ListCompleteness `json:"completeness"`
	Errors       []model.InboxFailure   `json:"errors,omitempty"`
}

// newEnvelope builds an envelope, defaulting a nil item slice to an empty one so
// the JSON always carries an array.
func newEnvelope(items any, cursor string, completeness model.ListCompleteness, failures []model.InboxFailure) commonEnvelope {
	return commonEnvelope{Items: items, NextCursor: cursor, Completeness: completeness, Errors: failures}
}

// folderListResponse is one folder row in the folder listing.
type folderListResponse struct {
	ID           string    `json:"id"`
	InboxID      string    `json:"inbox_id"`
	Path         string    `json:"path"`
	Name         string    `json:"name"`
	ParentPath   string    `json:"parent_path,omitempty"`
	Role         string    `json:"role"`
	MessageCount int64     `json:"message_count"`
	UnreadCount  int64     `json:"unread_count"`
	Selectable   bool      `json:"selectable"`
	IsSystem     bool      `json:"is_system,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func folderResponse(f model.Folder) folderListResponse {
	return folderListResponse{
		ID: f.ID, InboxID: f.InboxID, Path: f.Path, Name: f.Name, ParentPath: f.ParentPath,
		Role: f.Role, MessageCount: f.MessageCount, UnreadCount: f.UnreadCount,
		Selectable: f.Selectable, IsSystem: f.IsSystem, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
	}
}

// remoteMessageResponse is the read projection of a cached remote message,
// mirroring the app RemoteMessageView but with the HTML body sanitized and never
// the raw bytes.
type remoteMessageResponse struct {
	ID           string        `json:"id"`
	InboxID      string        `json:"inbox_id"`
	FolderPath   string        `json:"folder_path"`
	UIDValidity  uint32        `json:"uid_validity"`
	UID          uint32        `json:"uid"`
	MessageID    string        `json:"message_id,omitempty"`
	InReplyTo    string        `json:"in_reply_to,omitempty"`
	References   []string      `json:"references,omitempty"`
	ThreadID     string        `json:"thread_id,omitempty"`
	From         model.Address `json:"from"`
	To           []string      `json:"to,omitempty"`
	CC           []string      `json:"cc,omitempty"`
	Subject      string        `json:"subject"`
	Snippet      string        `json:"snippet,omitempty"`
	Text         string        `json:"text,omitempty"`
	HTML         string        `json:"html,omitempty"`
	SizeBytes    int64         `json:"size_bytes"`
	HasAttach    bool          `json:"has_attachments"`
	Read         bool          `json:"read"`
	Flagged      bool          `json:"flagged"`
	Answered     bool          `json:"answered"`
	Draft        bool          `json:"draft"`
	Labels       []string      `json:"labels,omitempty"`
	ReceivedAt   *string       `json:"received_at,omitempty"`
	SentAt       *string       `json:"sent_at,omitempty"`
	InternalDate *string       `json:"internal_date,omitempty"`
	IndexedAt    time.Time     `json:"indexed_at,omitempty"`
}

// hydrateRemoteBody fetches a remote message's raw MIME transiently, parses it,
// and returns its plain text and sanitized HTML. The body is never archived: it
// is fetched live per read, matching the local single-message read. There is no
// offline/degraded mode for the body: a fetch or parse failure is returned so
// the caller answers 503 (the connector is unreachable) rather than silently
// serving only cached metadata.
func (s *Server) hydrateRemoteBody(ctx context.Context, p model.Principal, mb mailboxBackend, messageID string) (string, string, error) {
	path, _, err := mb.remote.FetchRemoteRaw(ctx, p, mb.inbox.ID, messageID)
	if err != nil {
		return "", "", err
	}
	defer mb.remote.CleanupRemoteRaw(path)
	parsed, perr := mailparse.ParseFile(path)
	if perr != nil {
		return "", "", model.NewMailboxError(model.ErrKindUnavailable, "the remote message body could not be parsed", true, perr)
	}
	return parsed.Text, htmlsanitize.Sanitize(parsed.HTML), nil
}

func remoteMessageResponseOf(v app.RemoteMessageView) remoteMessageResponse {
	return remoteMessageResponse{
		ID: v.ID, InboxID: v.InboxID, FolderPath: v.FolderPath, UIDValidity: v.UIDValidity, UID: v.UID,
		MessageID: v.RFCMessageID, InReplyTo: v.InReplyTo, References: v.References, ThreadID: v.ThreadKey,
		From: v.From, To: v.To, CC: v.CC, Subject: v.Subject, Snippet: v.Snippet, SizeBytes: v.SizeBytes,
		HasAttach: v.HasAttach, Read: v.Read, Flagged: v.Flagged, Answered: v.Answered, Draft: v.Draft,
		Labels: v.Labels, ReceivedAt: v.ReceivedAt, SentAt: v.SentAt, InternalDate: v.InternalDate, IndexedAt: v.IndexedAt,
	}
}

// remoteMessageResponseWithBody is remoteMessageResponseOf plus the live body:
// the single-message read fetches and parses the MIME (never archived) so the
// common GET returns the same text/html a local message read does. List reads
// stay metadata-only.
func (s *Server) remoteMessageResponseWithBody(ctx context.Context, p model.Principal, mb mailboxBackend, v app.RemoteMessageView) (remoteMessageResponse, error) {
	out := remoteMessageResponseOf(v)
	text, html, err := s.hydrateRemoteBody(ctx, p, mb, v.ID)
	if err != nil {
		return remoteMessageResponse{}, err
	}
	out.Text, out.HTML = text, html
	return out, nil
}

// remoteThreadResponse is one thread row in a remote thread listing.
type remoteThreadResponse struct {
	ID            string    `json:"id"`
	InboxID       string    `json:"inbox_id"`
	Subject       string    `json:"subject"`
	MessageCount  int       `json:"message_count"`
	UnreadCount   int       `json:"unread_count"`
	LastMessageAt time.Time `json:"last_message_at"`
}

// ---- Folders ----

// apiInboxFolders lists an inbox's folders (domain or standalone) in hierarchical
// order. A standalone inbox additionally reconciles its remote folder tree on
// demand so the first call after setup returns the live folders.
func (s *Server) apiInboxFolders(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	if mb.routed {
		if mb.remoteConfigured() {
			if _, rerr := mb.remote.ReconcileRemote(r.Context(), p.AccountID, mb.inbox.ID); rerr != nil {
				// A connection failure still returns whatever folder tree is
				// cached, with the failure recorded in errors, so a client can
				// render the last-known structure rather than nothing.
				folders, lerr := s.Service.Store.ListFolders(r.Context(), p.AccountID, mb.inbox.ID)
				if lerr != nil {
					mapStoreError(w, lerr)
					return
				}
				out := make([]folderListResponse, 0, len(folders))
				for _, f := range folders {
					out = append(out, folderResponse(f))
				}
				writeJSON(w, 200, newEnvelope(out, "", model.CompletenessUnknown, []model.InboxFailure{model.NewInboxFailure(mb.inbox.ID, rerr)}))
				return
			}
		}
		folders, err := s.Service.Store.ListFolders(r.Context(), p.AccountID, mb.inbox.ID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		out := make([]folderListResponse, 0, len(folders))
		for _, f := range folders {
			out = append(out, folderResponse(f))
		}
		writeJSON(w, 200, newEnvelope(out, "", model.CompletenessComplete, nil))
		return
	}
	// A domain inbox uses the local folder tree. Seed the protected system
	// folders on first access so the folder model is uniform across kinds.
	folders, err := s.Service.Store.EnsureSystemFolders(r.Context(), p.AccountID, mb.inbox.ID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	out := make([]folderListResponse, 0, len(folders))
	for _, f := range folders {
		out = append(out, folderResponse(f))
	}
	writeJSON(w, 200, newEnvelope(out, "", model.CompletenessComplete, nil))
}

// apiInboxFolderWrite creates, renames or deletes a folder. Creation is a POST on
// the collection; rename and delete are PATCH/DELETE on one folder. A standalone
// inbox routes the mutation to the live remote server; a domain inbox mutates the
// local folder tree.
func (s *Server) apiInboxFoldersWrite(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	var in struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		writeError(w, 400, "path is required")
		return
	}
	if mb.routed {
		folder, cerr := mb.remote.CreateRemoteFolder(r.Context(), p, mb.inbox.ID, path, in.Name)
		if cerr != nil {
			mapMailboxError(w, cerr)
			return
		}
		writeJSON(w, 201, folderResponse(folder))
		return
	}
	folder, cerr := s.Service.Store.CreateFolder(r.Context(), p, mb.inbox.ID, store.FolderCreate{Path: path, Name: in.Name})
	if cerr != nil {
		mapStoreError(w, cerr)
		return
	}
	writeJSON(w, 201, folderResponse(folder))
}

// apiInboxFolderItem renames or deletes one folder.
func (s *Server) apiInboxFolderItem(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	folderID := r.PathValue("folderId")
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Name) == "" {
			writeError(w, 400, "name is required")
			return
		}
		if mb.routed {
			folder, rerr := mb.remote.RenameRemoteFolder(r.Context(), p, mb.inbox.ID, folderID, in.Name)
			if rerr != nil {
				mapMailboxError(w, rerr)
				return
			}
			writeJSON(w, 200, folderResponse(folder))
			return
		}
		folder, rerr := s.Service.Store.RenameFolder(r.Context(), p, mb.inbox.ID, folderID, in.Name)
		if rerr != nil {
			mapStoreError(w, rerr)
			return
		}
		writeJSON(w, 200, folderResponse(folder))
	case http.MethodDelete:
		if mb.routed {
			if derr := mb.remote.DeleteRemoteFolder(r.Context(), p, mb.inbox.ID, folderID); derr != nil {
				mapMailboxError(w, derr)
				return
			}
			w.WriteHeader(204)
			return
		}
		if derr := s.Service.Store.DeleteFolder(r.Context(), p, mb.inbox.ID, folderID); derr != nil {
			mapStoreError(w, derr)
			return
		}
		w.WriteHeader(204)
	}
}

// ---- Messages ----

// apiInboxMessages lists an inbox's messages as the shared envelope. A domain
// inbox reads the local store; a standalone inbox reads the cached remote index
// (reconciling on demand) and returns remote message projections. folder selects
// a folder path (standalone) or folder id (local); when empty the inbox's root/
// Inbox is used.
func (s *Server) apiInboxMessages(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	before := strings.TrimSpace(r.URL.Query().Get("before"))
	if !mb.routed {
		f := store.MessageFilter{
			InboxID:   mb.inbox.ID,
			Direction: r.URL.Query().Get("direction"),
			From:      r.URL.Query().Get("from"),
			To:        r.URL.Query().Get("to"),
			Labels:    r.URL.Query()["label"],
			Before:    before,
			Limit:     limit + 1,
		}
		if folder := strings.TrimSpace(r.URL.Query().Get("folder")); folder != "" {
			f.FolderScoped = true
			f.MailboxID = folder
		}
		msgs, lerr := s.Service.Store.ListMessages(r.Context(), p, f)
		if lerr != nil {
			mapStoreError(w, lerr)
			return
		}
		cursor := ""
		if len(msgs) > limit {
			msgs = msgs[:limit]
			if len(msgs) > 0 {
				cursor = msgs[len(msgs)-1].ID
			}
		}
		writeJSON(w, 200, newEnvelope(sanitizedMessages(msgs), cursor, model.CompletenessComplete, nil))
		return
	}
	res, rerr := mb.remote.ListRemoteMessages(r.Context(), p, mb.inbox.ID, r.URL.Query().Get("folder"), limit, before)
	if rerr != nil {
		mapMailboxError(w, rerr)
		return
	}
	items := make([]remoteMessageResponse, 0, len(res.Items))
	for _, v := range res.Items {
		items = append(items, remoteMessageResponseOf(v))
	}
	writeJSON(w, 200, newEnvelope(items, res.NextCursor, res.Completeness, nil))
}

// apiInboxMessage returns one message. For a standalone inbox it is the cached
// remote projection with a best-effort live header refresh; the body is fetched
// separately by the content route.
func (s *Server) apiInboxMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	messageID := r.PathValue("messageId")
	if !mb.routed {
		// The local store's messages are addressed by their own id; scoping the
		// read to the inbox prevents a cross-inbox id from being served.
		m, lerr := s.Service.Store.GetMessage(r.Context(), p, messageID)
		if lerr != nil || m.InboxID != mb.inbox.ID {
			writeError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, sanitizedMessage(m))
		return
	}
	v, rerr := mb.remote.GetRemoteMessage(r.Context(), p, mb.inbox.ID, messageID)
	if rerr != nil {
		mapMailboxError(w, rerr)
		return
	}
	resp, herr := s.remoteMessageResponseWithBody(r.Context(), p, mb, v)
	if herr != nil {
		mapMailboxError(w, herr)
		return
	}
	writeJSON(w, 200, resp)
}

// apiInboxMessageContent streams the raw RFC5322 MIME of a message. A local
// message reads its stored raw file; a remote message streams the body live to a
// transient temp file and never archives it. The response is always an
// attachment download.
func (s *Server) apiInboxMessageContent(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	messageID := r.PathValue("messageId")
	if !mb.routed {
		m, lerr := s.Service.Store.GetMessage(r.Context(), p, messageID)
		if lerr != nil || m.InboxID != mb.inbox.ID {
			writeError(w, 404, "not found")
			return
		}
		path, perr := s.dataPath(m.RawPath)
		if perr != nil {
			writeError(w, 500, "internal error")
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeRawFilename(m.RFCMessageID)}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
		return
	}
	path, size, ferr := mb.remote.FetchRemoteRaw(r.Context(), p, mb.inbox.ID, messageID)
	if ferr != nil {
		mapMailboxError(w, ferr)
		return
	}
	defer mb.remote.CleanupRemoteRaw(path)
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "message.eml"}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	http.ServeFile(w, r, path)
}

// apiInboxMessageAttachment streams one MIME part of a message as a download. A
// local message extracts the named attachment; a remote message fetches the part
// structurally by its MIME part path.
func (s *Server) apiInboxMessageAttachment(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	messageID := r.PathValue("messageId")
	if !mb.routed {
		m, lerr := s.Service.Store.GetMessage(r.Context(), p, messageID)
		if lerr != nil || m.InboxID != mb.inbox.ID {
			writeError(w, 404, "not found")
			return
		}
		atts, aerr := s.Service.Store.ListAttachments(r.Context(), p, m.ID)
		if aerr != nil {
			mapStoreError(w, aerr)
			return
		}
		part, _ := strconv.Atoi(r.PathValue("part"))
		for _, a := range atts {
			if a.PartIndex != part {
				continue
			}
			s.streamLocalAttachment(w, r, m, a)
			return
		}
		writeError(w, 404, "attachment not found")
		return
	}
	partPath := parsePartPath(r.PathValue("part"))
	if len(partPath) == 0 {
		writeError(w, 400, "invalid part path")
		return
	}
	filename := strings.TrimSpace(r.URL.Query().Get("filename"))
	contentType := strings.TrimSpace(r.URL.Query().Get("content_type"))
	att, ferr := mb.remote.FetchRemoteAttachment(r.Context(), p, mb.inbox.ID, messageID, partPath, filename, contentType)
	if ferr != nil {
		mapMailboxError(w, ferr)
		return
	}
	defer mb.remote.CleanupRemoteRaw(att.Path)
	name := att.Filename
	if name == "" {
		name = "attachment"
	}
	ct := att.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(att.Size, 10))
	f, oerr := openFile(att.Path)
	if oerr != nil {
		writeError(w, 500, "internal error")
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

// streamLocalAttachment serves one already-resolved local attachment.
func (s *Server) streamLocalAttachment(w http.ResponseWriter, r *http.Request, m model.Message, a model.Attachment) {
	path, perr := s.dataPath(m.RawPath)
	if perr != nil {
		writeError(w, 500, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := appExtractAttachment(path, a.PartIndex, w); err != nil {
		s.Log.Error("attachment extraction", "error", err)
	}
}

// ---- Threads ----

// apiInboxThreads lists an inbox's threads as the shared envelope.
func (s *Server) apiInboxThreads(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	if !mb.routed {
		items, lerr := s.Service.Store.ListThreads(r.Context(), p, mb.inbox.ID, limit)
		if lerr != nil {
			mapStoreError(w, lerr)
			return
		}
		writeJSON(w, 200, newEnvelope(items, "", model.CompletenessComplete, nil))
		return
	}
	threads, rerr := mb.remote.ListRemoteThreads(r.Context(), p, mb.inbox.ID, r.URL.Query().Get("folder"), limit, "")
	if rerr != nil {
		mapMailboxError(w, rerr)
		return
	}
	out := make([]remoteThreadResponse, 0, len(threads))
	for _, t := range threads {
		out = append(out, remoteThreadResponse{ID: t.Key, InboxID: t.InboxID, Subject: t.Subject, MessageCount: t.MessageCount, UnreadCount: t.UnreadCount, LastMessageAt: t.LastMessageAt})
	}
	writeJSON(w, 200, newEnvelope(out, "", model.CompletenessComplete, nil))
}

// apiInboxThread returns one thread with its messages.
func (s *Server) apiInboxThread(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	threadKey := r.PathValue("threadId")
	if !mb.routed {
		msgs, lerr := s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{InboxID: mb.inbox.ID, ThreadID: threadKey, Limit: limits.PageSizeMaxList})
		if lerr != nil {
			mapStoreError(w, lerr)
			return
		}
		if len(msgs) == 0 {
			writeError(w, 404, "thread not found")
			return
		}
		writeJSON(w, 200, map[string]any{"id": threadKey, "inbox_id": mb.inbox.ID, "subject": msgs[len(msgs)-1].Subject, "message_count": len(msgs), "messages": sanitizedMessages(msgs)})
		return
	}
	view, msgs, rerr := mb.remote.GetRemoteThread(r.Context(), p, mb.inbox.ID, threadKey)
	if rerr != nil {
		mapMailboxError(w, rerr)
		return
	}
	items := make([]remoteMessageResponse, 0, len(msgs))
	for _, v := range msgs {
		items = append(items, remoteMessageResponseOf(v))
	}
	writeJSON(w, 200, map[string]any{"id": view.Key, "inbox_id": mb.inbox.ID, "subject": view.Subject, "message_count": view.MessageCount, "unread_count": view.UnreadCount, "messages": items})
}

// ---- Search ----

// apiInboxSearch searches one inbox. A domain inbox uses the local FTS5 engine;
// a standalone inbox runs a live server search and intersects the result with its
// local metadata cache and any local label filter. The result carries the shared
// envelope; for a remote search the completeness reflects the server's own
// best-effort enumeration.
func (s *Server) apiInboxSearch(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if !mb.routed {
		items, lerr := s.Service.Store.SearchMessagesFiltered(r.Context(), p, q, store.MessageFilter{
			InboxID: mb.inbox.ID,
			From:    r.URL.Query().Get("from"),
			To:      r.URL.Query().Get("to"),
			Labels:  r.URL.Query()["label"],
			Before:  strings.TrimSpace(r.URL.Query().Get("before")),
			Limit:   limit,
		})
		if lerr != nil {
			mapStoreError(w, lerr)
			return
		}
		writeJSON(w, 200, newEnvelope(sanitizedMessages(items), "", model.CompletenessComplete, nil))
		return
	}
	cursorUID := uint32(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		if n, cerr := strconv.ParseUint(raw, 10, 32); cerr == nil {
			cursorUID = uint32(n)
		}
	}
	sq := app.RemoteSearchQuery{
		FolderPath: r.URL.Query().Get("folder"),
		From:       r.URL.Query().Get("from"),
		To:         r.URL.Query().Get("to"),
		Subject:    r.URL.Query().Get("subject"),
		Text:       q,
		Label:      firstQuery(r, "label"),
		Limit:      limit,
		Cursor:     cursorUID,
	}
	res, rerr := mb.remote.SearchRemote(r.Context(), p, mb.inbox.ID, sq)
	if rerr != nil {
		mapMailboxError(w, rerr)
		return
	}
	items := make([]remoteMessageResponse, 0, len(res.Items))
	for _, v := range res.Items {
		items = append(items, remoteMessageResponseOf(v))
	}
	cursor := ""
	if res.NextCursor != 0 {
		cursor = strconv.FormatUint(uint64(res.NextCursor), 10)
	}
	writeJSON(w, 200, newEnvelope(items, cursor, res.Completeness, nil))
}

// ---- Labels ----

// apiInboxLabels lists the distinct labels in use in an inbox. A standalone inbox
// keeps labels as local free-text metadata on cached remote messages, so the
// labels are the same concept on both kinds.
func (s *Server) apiInboxLabels(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	mb, err := s.resolveMailbox(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	if mb.routed {
		labels, lerr := mb.remote.ListRemoteLabels(r.Context(), p, mb.inbox.ID)
		if lerr != nil {
			mapMailboxError(w, lerr)
			return
		}
		writeJSON(w, 200, newEnvelope(labels, "", model.CompletenessComplete, nil))
		return
	}
	labels, lerr := s.Service.Store.ListInboxLabels(r.Context(), p, mb.inbox.ID)
	if lerr != nil {
		mapStoreError(w, lerr)
		return
	}
	writeJSON(w, 200, newEnvelope(labels, "", model.CompletenessComplete, nil))
}

// firstQuery returns the first value of a repeatable query parameter.
func firstQuery(r *http.Request, name string) string {
	if v := r.URL.Query()[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// parsePartPath parses an IMAP MIME part path ("1.2.3", or a bare "1") into a
// slice of part numbers.
func parsePartPath(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ".")
	out := make([]int, 0, len(parts))
	for _, seg := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(seg))
		if err != nil || n < 1 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// safeRawFilename derives a download filename for a raw message from its
// Message-ID, falling back to a generic name.
func safeRawFilename(messageID string) string {
	id := strings.Trim(strings.TrimSpace(messageID), "<>")
	if id == "" {
		return "message.eml"
	}
	id = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '-'
		}
		return r
	}, id)
	return id + ".eml"
}

// appExtractAttachment is a thin indirection over the mailparse extractor so the
// standalone API and the local attachment path share one call site.
func appExtractAttachment(path string, partIndex int, w io.Writer) error {
	return mailparse.ExtractAttachment(path, partIndex, w)
}

// remoteConfigResponse is the secret-free view of a standalone inbox's remote
// binding, for the API and the UI. It reports whether each secret is present but
// never returns it.
type remoteConfigResponse struct {
	Host            string             `json:"host,omitempty"`
	Port            int                `json:"port,omitempty"`
	Username        string             `json:"username,omitempty"`
	Security        string             `json:"security,omitempty"`
	Namespace       string             `json:"namespace,omitempty"`
	SMTPHost        string             `json:"smtp_host,omitempty"`
	SMTPPort        int                `json:"smtp_port,omitempty"`
	SMTPUsername    string             `json:"smtp_username,omitempty"`
	SMTPSecurity    string             `json:"smtp_security,omitempty"`
	IMAPPasswordSet bool               `json:"imap_password_set"`
	SMTPPasswordSet bool               `json:"smtp_password_set"`
	Configured      bool               `json:"configured"`
	SentCopyEnabled bool               `json:"sent_copy_enabled"`
	SentCopyFolder  string             `json:"sent_copy_folder,omitempty"`
	Capabilities    model.Capabilities `json:"capabilities"`
	MissingRoles    []string           `json:"missing_roles,omitempty"`
}

// remoteConfigView builds the secret-free remote view of a standalone inbox from
// the store, plus the folder roles that are not yet mapped (so the UI can prompt
// the operator to select or create them).
func (s *Server) remoteConfigView(ctx context.Context, p model.Principal, inboxID string) (remoteConfigResponse, error) {
	inbox, err := s.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
	if err != nil {
		return remoteConfigResponse{}, err
	}
	if inbox.Kind != model.InboxKindStandalone {
		return remoteConfigResponse{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, nil)
	}
	creds, cerr := s.Service.Store.GetRemoteCredentials(ctx, p.AccountID, inboxID)
	if cerr != nil && !errors.Is(cerr, store.ErrNotFound) {
		return remoteConfigResponse{}, cerr
	}
	caps := model.StandaloneCapabilities()
	caps.Outbound = inbox.Remote != nil && inbox.Remote.SMTP != nil
	out := remoteConfigResponse{
		Namespace:       inbox.Namespace,
		IMAPPasswordSet: strings.TrimSpace(creds.EncryptedIMAP) != "",
		SMTPPasswordSet: strings.TrimSpace(creds.EncryptedSMTP) != "",
		// Active only when a real binding (host + username) and a stored password
		// are both present: a stored secret with no host, or a host with no
		// secret, is not an active connector and must not be reported as one.
		Configured:      inbox.RemoteConfigured && inbox.Remote != nil && strings.TrimSpace(inbox.Remote.Host) != "",
		SentCopyEnabled: inbox.RemoteSentCopyEnabled,
		SentCopyFolder:  inbox.RemoteSentCopyFolder,
		Capabilities:    caps,
	}
	if inbox.Remote != nil {
		out.Host, out.Port, out.Username, out.Security = inbox.Remote.Host, inbox.Remote.Port, inbox.Remote.Username, inbox.Remote.Security
		if inbox.Remote.SMTP != nil {
			out.SMTPHost, out.SMTPPort, out.SMTPUsername, out.SMTPSecurity = inbox.Remote.SMTP.Host, inbox.Remote.SMTP.Port, inbox.Remote.SMTP.Username, inbox.Remote.SMTP.Security
		}
	}
	out.MissingRoles = s.missingRemoteRoles(ctx, p.AccountID, inboxID)
	return out, nil
}

// specialFolderRoles are the folder roles the UI treats specially. A missing role
// means the operator has not yet mapped a folder to it, so the UI prompts for an
// explicit select-or-create.
var specialFolderRoles = []string{model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleDrafts, model.FolderRoleTrash, model.FolderRoleSpam}

// missingRemoteRoles returns the special roles a standalone inbox has not mapped
// to a remote-origin folder.
func (s *Server) missingRemoteRoles(ctx context.Context, accountID, inboxID string) []string {
	var out []string
	for _, role := range specialFolderRoles {
		if _, ok := s.remoteRoleFolder(ctx, accountID, inboxID, role); !ok {
			out = append(out, role)
		}
	}
	return out
}

// apiInboxRemoteGet returns the secret-free remote configuration of a standalone
// inbox.
func (s *Server) apiInboxRemoteGet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !p.CanRead(r.PathValue("id")) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	view, err := s.remoteConfigView(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapMailboxError(w, mapRemoteStoreError(err))
		return
	}
	writeJSON(w, 200, view)
}

// verifyRemoteUpdate runs a live IMAP authentication test for a non-trivial
// remote update and returns an error when it cannot connect. It is the gate that
// stops a bad or empty connector from being persisted and then shown as active.
// A no-op update (no non-secret field and no new secret) is not tested. A blank
// secret uses the stored value, so changing only the host still proves the new
// host authenticates with the existing password.
func (s *Server) verifyRemoteUpdate(ctx context.Context, p model.Principal, inboxID string, in store.StandaloneRemoteUpdate) error {
	if !hasRemoteNonSecretFields(in) && strings.TrimSpace(in.IMAPPassword) == "" {
		return nil
	}
	if strings.TrimSpace(in.Host) == "" && !hasStoredHost(ctx, s, p.AccountID, inboxID) {
		return model.NewMailboxError(model.ErrKindInvalid, "an IMAP host is required to activate the connector", false, nil)
	}
	if _, err := s.remoteMailbox().TestStandaloneRemote(ctx, p, inboxID, in); err != nil {
		return err
	}
	return nil
}

// hasStoredHost reports whether a standalone inbox already has a stored remote
// host (so a host-less update is an update, not a fresh activation).
func hasStoredHost(ctx context.Context, s *Server, accountID, inboxID string) bool {
	inbox, err := s.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return false
	}
	return inbox.Remote != nil && strings.TrimSpace(inbox.Remote.Host) != ""
}

// apiInboxRemoteSave configures a standalone inbox's remote binding. It requires
// Owner (or an account Admin). A blank secret field retains the stored one. A
// newly established or changed IMAP binding must authenticate before it is saved.
func (s *Server) apiInboxRemoteSave(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	if !p.CanOwn(inboxID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	var in struct {
		Host         string `json:"host"`
		Port         int    `json:"port"`
		Username     string `json:"username"`
		Security     string `json:"security"`
		SMTPHost     string `json:"smtp_host"`
		SMTPPort     int    `json:"smtp_port"`
		SMTPUsername string `json:"smtp_username"`
		SMTPSecurity string `json:"smtp_security"`
		ClearSMTP    bool   `json:"clear_smtp"`
		Namespace    string `json:"namespace"`
		IMAPPassword string `json:"imap_password"`
		SMTPPassword string `json:"smtp_password"`
		// SentCopyEnabled/SentCopyFolder configure the standalone Sent copy; nil
		// means "leave unchanged".
		SentCopyEnabled *bool  `json:"sent_copy_enabled"`
		SentCopyFolder  string `json:"sent_copy_folder"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	update := store.StandaloneRemoteUpdate{
		Host: in.Host, Port: in.Port, Username: in.Username, Security: in.Security,
		SMTPHost: in.SMTPHost, SMTPPort: in.SMTPPort, SMTPUsername: in.SMTPUsername, SMTPSecurity: in.SMTPSecurity,
		ClearSMTP: in.ClearSMTP, Namespace: in.Namespace, IMAPPassword: in.IMAPPassword, SMTPPassword: in.SMTPPassword,
	}
	// A connector must authenticate before it is persisted: an unreachable host
	// or missing credential is rejected rather than saved and shown as active.
	if verr := s.verifyRemoteUpdate(r.Context(), p, inboxID, update); verr != nil {
		mapMailboxError(w, verr)
		return
	}
	rm := s.remoteMailbox()
	// Apply the sent-copy toggle before the primary connector binding. Both write
	// the same inbox row and there is no cross-setting transaction, so applying
	// the smaller, standalone-only setting first means a rejected sent-copy
	// toggle cannot leave a freshly changed primary connector in place. The
	// binding is verified above, so its own failure here is an unexpected store
	// or encryption error.
	if in.SentCopyEnabled != nil || strings.TrimSpace(in.SentCopyFolder) != "" {
		current, cerr := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inboxID)
		if cerr != nil {
			mapStoreError(w, cerr)
			return
		}
		folder := current.RemoteSentCopyFolder
		if strings.TrimSpace(in.SentCopyFolder) != "" {
			folder = in.SentCopyFolder
		}
		enabled := current.RemoteSentCopyEnabled
		if in.SentCopyEnabled != nil {
			enabled = *in.SentCopyEnabled
		}
		if err := s.Service.Store.SetRemoteSentCopy(r.Context(), p, inboxID, enabled, folder); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	inbox, err := rm.ConfigureStandaloneRemote(r.Context(), p, inboxID, update)
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	writeJSON(w, 200, inbox)
}

// apiInboxRemoteTest verifies a standalone inbox's remote binding authenticates
// and resolves a folder scope, without mutating mailbox state. A blank secret uses
// the stored value so a configuration can be tested before saving.
func (s *Server) apiInboxRemoteTest(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	// Testing a connector dials a caller-supplied host with the inbox's stored
	// credentials, so it is an Owner/Admin capability, not a read. A merely
	// Read-scoped caller must not be able to make the server open outbound
	// connections or hand a stored secret to an attacker-chosen host.
	if !p.CanOwn(inboxID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	var in struct {
		Host         string `json:"host"`
		Port         int    `json:"port"`
		Username     string `json:"username"`
		Security     string `json:"security"`
		SMTPHost     string `json:"smtp_host"`
		SMTPPort     int    `json:"smtp_port"`
		SMTPUsername string `json:"smtp_username"`
		SMTPSecurity string `json:"smtp_security"`
		Namespace    string `json:"namespace"`
		IMAPPassword string `json:"imap_password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	scope, err := s.remoteMailbox().TestStandaloneRemote(r.Context(), p, inboxID, store.StandaloneRemoteUpdate{
		Host: in.Host, Port: in.Port, Username: in.Username, Security: in.Security,
		SMTPHost: in.SMTPHost, SMTPPort: in.SMTPPort, SMTPUsername: in.SMTPUsername, SMTPSecurity: in.SMTPSecurity,
		Namespace: in.Namespace, IMAPPassword: in.IMAPPassword,
	})
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "root": scope.Root, "delimiter": string(scope.Delimiter), "personal": scope.Personal, "inbox_in_scope": scope.INBOXInScope})
}

// apiInboxRemoteRefresh forces a full metadata reconcile of a standalone inbox
// against its live server.
func (s *Server) apiInboxRemoteRefresh(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	if !p.CanAssist(inboxID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	status, err := s.remoteMailbox().ReconcileRemote(r.Context(), p.AccountID, inboxID)
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": status.Status, "indexed_at": status.IndexedAt})
}

// mapRemoteStoreError maps a raw store error returned from a remote-config helper
// onto the common classification.
func mapRemoteStoreError(err error) error {
	return normalizeMailboxStoreError(err)
}

// openFile opens a transient file for streaming.
func openFile(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// authoringResponse is the per-inbox assistant authoring configuration, secret-
// free. Mode is snapshotted onto each request at creation, so changing it never
// affects an in-flight request. A standalone inbox defaults to remote_draft
// (hand off to its connected Drafts folder); a domain inbox defaults to
// mailmoose_approval. The approver is meaningful only for mailmoose_approval, so
// approver_enabled tracks the EFFECTIVE mode (a standalone inbox switched to
// mailmoose_approval enables it; a domain inbox switched to remote_draft disables
// it). The notify override is optional and defaults to the connected address.
type authoringResponse struct {
	Mode             string `json:"mode"`
	DefaultMode      string `json:"default_mode"`
	NotifyAddress    string `json:"notify_address"`
	NotifyOverridden bool   `json:"notify_overridden"`
	ApproverEnabled  bool   `json:"approver_enabled"`
	ApproverEmail    string `json:"approver_email,omitempty"`
	// Standalone reports whether the inbox is a standalone (remote) mailbox, so a
	// client can label the tab "Approvals" and show handoff state.
	Standalone bool `json:"standalone"`
}

// apiInboxAuthoringGet returns the effective authoring settings of an inbox.
func (s *Server) apiInboxAuthoringGet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if !p.CanRead(id) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	box, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	settings, err := s.Service.Store.GetInboxAuthoringSettings(r.Context(), p, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, authoringResponse{
		Mode:             settings.Mode,
		DefaultMode:      model.DefaultAuthoringMode(box.Kind),
		NotifyAddress:    settings.NotifyAddress,
		NotifyOverridden: settings.NotifyOverridden,
		// The approver is enabled exactly when the effective mode is the
		// MailMoose approval workflow, regardless of inbox kind.
		ApproverEnabled: settings.Mode == model.AuthoringMailMooseApproval,
		ApproverEmail:   box.ApproverEmail,
		Standalone:      box.Kind == model.InboxKindStandalone,
	})
}

// apiInboxAuthoringSet updates an inbox's authoring mode and/or notify override.
// It requires Owner. The mode is validated and snapshotted onto each request at
// creation, so an in-flight request is never changed. An empty mode clears the
// override so it follows the inbox kind default.
func (s *Server) apiInboxAuthoringSet(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if !p.CanOwn(id) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	var in struct {
		Mode          *string `json:"mode"`
		NotifyAddress *string `json:"notify_address"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Mode != nil {
		if *in.Mode != "" && !model.ValidAuthoringMode(*in.Mode) {
			writeError(w, 400, "unknown authoring mode; expected mailmoose_approval or remote_draft")
			return
		}
		if err := s.Service.Store.SetInboxAuthoringMode(r.Context(), p, id, *in.Mode); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	if in.NotifyAddress != nil {
		if err := s.Service.Store.SetInboxNotifyAddress(r.Context(), p, id, *in.NotifyAddress); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	s.apiInboxAuthoringGet(w, r)
}

// patchRemoteMessage applies a read/labels patch to a standalone inbox's remote
// message: read toggles the live \Seen flag (a state change, Assistant/Owner), and
// labels are local free-text metadata. Spam is not a remote server concept, so it
// is ignored. It writes the resulting message.
func (s *Server) patchRemoteMessage(w http.ResponseWriter, r *http.Request, p model.Principal, mb mailboxBackend, id string, read *bool, labels *[]string) {
	if !p.CanAssist(mb.inbox.ID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	if read != nil {
		if _, err := mb.remote.SetRemoteRead(r.Context(), p, mb.inbox.ID, id, *read); err != nil {
			mapMailboxError(w, err)
			return
		}
	}
	if labels != nil {
		if _, err := mb.remote.SetRemoteLabels(r.Context(), p, mb.inbox.ID, id, *labels); err != nil {
			mapMailboxError(w, err)
			return
		}
	}
	updated, err := s.resolveRemoteMessage(r.Context(), p, mb, id)
	if err != nil {
		mapMailboxError(w, err)
		return
	}
	writeJSON(w, 200, sanitizedMessage(updated))
}

// resolveRemoteMessage re-reads a remote message projection as a common message.
func (s *Server) resolveRemoteMessage(ctx context.Context, p model.Principal, mb mailboxBackend, id string) (model.Message, error) {
	v, err := mb.remote.GetRemoteMessage(ctx, p, mb.inbox.ID, id)
	if err != nil {
		return model.Message{}, err
	}
	return remoteMessageToModel(v, &model.Folder{Path: v.FolderPath}), nil
}

// trashRemoteMessage moves a standalone inbox's remote message to the inbox's
// Trash-role folder. When the inbox has no mapped Trash folder the operation is
// refused rather than silently deleting. It requires Assistant/Owner.
func (s *Server) trashRemoteMessage(ctx context.Context, p model.Principal, mb mailboxBackend, id string) error {
	if !p.CanAssist(mb.inbox.ID) && !p.Admin {
		return model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	return s.moveRemoteToRole(ctx, p, mb, id, model.FolderRoleTrash)
}

// moveRemoteToRole moves a remote message to the inbox's folder carrying a role
// (Trash for a delete, Inbox for a restore). A missing role folder is a permanent
// refusal.
func (s *Server) moveRemoteToRole(ctx context.Context, p model.Principal, mb mailboxBackend, id, role string) error {
	folder, ok := s.remoteRoleFolder(ctx, p.AccountID, mb.inbox.ID, role)
	if !ok || strings.TrimSpace(folder.Path) == "" {
		return model.NewMailboxError(model.ErrKindUnsupported, "this inbox has no remote "+role+" folder mapped", false, nil)
	}
	if _, err := mb.remote.MoveRemoteMessage(ctx, p, mb.inbox.ID, id, folder.Path); err != nil {
		return err
	}
	return nil
}

// remoteMessageContent streams a remote message body; see apiInboxMessageContent.
// It is the shared implementation used by both the original and common content
// routes.
func (s *Server) remoteMessageContent(w http.ResponseWriter, r *http.Request, p model.Principal, mb mailboxBackend, messageID string) {
	path, size, ferr := mb.remote.FetchRemoteRaw(r.Context(), p, mb.inbox.ID, messageID)
	if ferr != nil {
		mapMailboxError(w, ferr)
		return
	}
	defer mb.remote.CleanupRemoteRaw(path)
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "message.eml"}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	http.ServeFile(w, r, path)
}

// roleFolderNames maps a special folder role to the conventional folder names a
// remote server uses for it. An explicit create uses the first name. Discovery
// (FolderRoleForName) maps any of these names to the role, so a created folder is
// recognised on the next reconcile.
var roleFolderNames = map[string][]string{
	model.FolderRoleInbox:  {"INBOX"},
	model.FolderRoleSent:   {"Sent", "Sent Items", "Sent Messages"},
	model.FolderRoleDrafts: {"Drafts"},
	model.FolderRoleTrash:  {"Trash", "Deleted", "Deleted Items", "Deleted Messages"},
	model.FolderRoleSpam:   {"Junk", "Spam", "Junk E-Mail"},
}

// apiInboxRemoteRole maps a standalone inbox's special folder role to a folder.
// It never silently invents a mapping: it reports the folder already carrying the
// role, or, with {"create": true}, creates the conventional folder on the live
// server (which discovery then maps to the role). Without either, it returns 404
// so a client can prompt an explicit select-or-create.
func (s *Server) apiInboxRemoteRole(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	inboxID := r.PathValue("id")
	if !p.CanAssist(inboxID) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	role := strings.TrimSpace(r.PathValue("role"))
	names, ok := roleFolderNames[role]
	if !ok {
		writeError(w, 400, "unknown folder role")
		return
	}
	inbox, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, inboxID)
	if err != nil || inbox.Kind != model.InboxKindStandalone {
		writeError(w, 404, "not found")
		return
	}
	// Detected: an existing remote-origin folder carrying the role. A local
	// seeded system folder (is_system=true) is not a remote mapping.
	if f, ok := s.remoteRoleFolder(r.Context(), p.AccountID, inboxID, role); ok {
		writeJSON(w, 200, map[string]any{"role": role, "mapped": true, "path": f.Path, "folder_id": f.ID})
		return
	}
	var in struct {
		Create   bool   `json:"create"`
		Path     string `json:"path"`
		FolderID string `json:"folder_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in)
	}
	// Mapping an existing folder: an explicit folder_id (a folder already in the
	// inbox tree) is mapped to the role and persisted with role_locked, so a later
	// reconcile never resets it. This is the "select an existing folder" path and
	// works for an arbitrarily-named folder; it needs no provider call.
	if !in.Create && strings.TrimSpace(in.FolderID) != "" {
		folder, merr := s.remoteMailbox().SetRemoteFolderRole(r.Context(), p, inboxID, strings.TrimSpace(in.FolderID), role)
		if merr != nil {
			mapMailboxError(w, merr)
			return
		}
		writeJSON(w, 200, map[string]any{"role": role, "mapped": true, "path": folder.Path, "folder_id": folder.ID})
		return
	}
	// Mapping an existing folder by path: resolve the path to a folder id first.
	if !in.Create && strings.TrimSpace(in.Path) != "" {
		folder, ferr := s.Service.Store.GetFolderByPath(r.Context(), p.AccountID, inboxID, strings.TrimSpace(in.Path))
		if ferr != nil {
			mapStoreError(w, mapRemoteStoreError(ferr))
			return
		}
		mapped, merr := s.remoteMailbox().SetRemoteFolderRole(r.Context(), p, inboxID, folder.ID, role)
		if merr != nil {
			mapMailboxError(w, merr)
			return
		}
		writeJSON(w, 200, map[string]any{"role": role, "mapped": true, "path": mapped.Path, "folder_id": mapped.ID})
		return
	}
	if !in.Create {
		writeError(w, 404, "this inbox has no folder for the "+role+" role; create one or select an existing folder")
		return
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = names[0]
	} else if !containsStringFold(names, path) {
		// An explicit path must still be a conventionally recognised name, so the
		// role is not silently invented for an arbitrary folder.
		writeError(w, 400, "the folder name does not map to the "+role+" role")
		return
	}
	folder, cerr := s.remoteMailbox().CreateRemoteFolder(r.Context(), p, inboxID, path, path)
	if cerr != nil {
		mapMailboxError(w, cerr)
		return
	}
	writeJSON(w, 201, map[string]any{"role": role, "mapped": true, "path": folder.Path, "folder_id": folder.ID, "created": true})
}

// containsStringFold reports whether a list contains a value case-insensitively.
func containsStringFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

// remoteRoleFolder returns a standalone inbox's folder carrying a role. A
// locally seeded system folder is excluded: a real remote mapping is either a
// folder mirrored from the server (origin='remote') or a folder the operator
// explicitly locked to the role (role_locked), so a missing role is never
// mistaken for one the server does not have.
func (s *Server) remoteRoleFolder(ctx context.Context, accountID, inboxID, role string) (model.Folder, bool) {
	folders, err := s.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return model.Folder{}, false
	}
	for _, f := range folders {
		if f.Role == role && strings.TrimSpace(f.Path) != "" && (f.Origin == "remote" || f.RoleLocked) {
			return f, true
		}
	}
	return model.Folder{}, false
}

// hasRemoteNonSecretFields reports whether a remote update supplies any non-secret
// field, so a secrets-only update is not gated on establishing a host.
func hasRemoteNonSecretFields(in store.StandaloneRemoteUpdate) bool {
	return strings.TrimSpace(in.Host) != "" || in.Port != 0 || strings.TrimSpace(in.Username) != "" ||
		strings.TrimSpace(in.Security) != "" || in.SMTPHost != "" || in.SMTPPort != 0 ||
		strings.TrimSpace(in.SMTPUsername) != "" || strings.TrimSpace(in.SMTPSecurity) != "" ||
		in.ClearSMTP || strings.TrimSpace(in.Namespace) != ""
}

// apiInboxHandoffs returns an inbox's RemoteDraft handoff history, newest first,
// retaining terminal records (published, ambiguous, failed/cancelled) even after
// the local draft is cleaned up. It is the durable source for the handoff history
// panel; it carries no approval token and never a frozen content hash.
func (s *Server) apiInboxHandoffs(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	if !p.CanRead(id) && !p.Admin {
		writeError(w, 403, "forbidden")
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	items, err := s.Service.Store.ListAssistantHandlingForInbox(r.Context(), p, id, limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, newEnvelope(items, "", model.CompletenessComplete, nil))
}
