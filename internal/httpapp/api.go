package httpapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/apispec"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/htmlsanitize"
	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/limits"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"name":        "MailMoose",
		"api_version": "v1",
		"api_base":    "/v1",
		"auth": map[string]string{
			"scheme":     "bearer",
			"header":     "Authorization",
			"key_prefix": "mmm_",
		},
		"agent_guide": "/agent",
		"openapi":     "/openapi.json",
		"reference":   "/agent",
		"changelog":   "/changelog",
		"examples": map[string]string{
			"python": "/examples/python",
			"bash":   "/examples/bash",
			"curl":   "/examples/curl",
		},
		"bootstrap":    "/v1/bootstrap",
		"capabilities": []string{"inboxes", "identities", "messages", "threads", "search", "labels", "attachments", "events", "drafts", "draft-approval", "outbox", "send", "hermes-relay", "external-aliases"},
		"limits":       s.discoveryLimits(),
	})
}

// discoveryLimits advertises the pagination, size and rate bounds a client
// needs to page safely and to set retry expectations. Pagination values come
// from the same constants the store clamps with, so the advertised numbers
// cannot drift from behaviour; the rest are runtime config.
func (s *Server) discoveryLimits() map[string]any {
	return map[string]any{
		"page_size_default":    limits.PageSizeDefault,
		"page_size_max_list":   limits.PageSizeMaxList,
		"page_size_max_events": limits.PageSizeMaxEvents,
		"message_bytes_max":    s.Service.Config.MaxMessageBytes,
		"attachment_bytes_max": s.Service.Config.MaxMessageBytes,
		"send_per_minute":      s.Service.Config.SendLimitPerMinute,
		"login_per_minute":     s.Service.Config.LoginLimitPerMinute,
		"storage_quota_bytes":  s.Service.Config.DefaultQuotaBytes,
	}
}
func (s *Server) agentGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	fmt.Fprint(w, apispec.RenderAgentGuide(apispec.Routes()))
}

// pythonExample serves the embedded Python client. It is not content-hashed,
// so it must not use the immutable asset cache policy.
func (s *Server) pythonExample(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "text/x-python; charset=utf-8", "no-cache", clientWithBase(pythonClient, s.requestBaseURL(r)))
}

// bashExample serves the embedded Bash client.
func (s *Server) bashExample(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "text/x-shellscript; charset=utf-8", "no-cache", clientWithBase(bashClient, s.requestBaseURL(r)))
}

// clientWithBase rewrites the embedded client's default base URL to the origin
// the caller reached, so a client downloaded from an instance talks to that
// instance by default instead of localhost. The source keeps its localhost
// default for running the file directly on the host.
func clientWithBase(src []byte, base string) []byte {
	if base == "" {
		return src
	}
	out := bytes.ReplaceAll(src, []byte(`DEFAULT_BASE_URL = "http://localhost:8081"`), []byte(`DEFAULT_BASE_URL = "`+base+`"`))
	return bytes.ReplaceAll(out, []byte(`DEFAULT_BASE_URL="http://localhost:8081"`), []byte(`DEFAULT_BASE_URL="`+base+`"`))
}

// curlExample serves the embedded curl scenario cookbook.
func (s *Server) curlExample(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "text/plain; charset=utf-8", "no-cache", curlCookbook)
}
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, apispec.RenderOpenAPI(s.requestBaseURL(r), apispec.Routes()))
}

func (s *Server) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	boxes, err := s.Service.Store.ListInboxes(r.Context(), p)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"account_id": p.AccountID, "admin": p.Admin, "mailbox_roles": p.MailboxRoles, "permissions": effectiveMailboxPermissions(p, boxes), "inboxes": boxes, "events": map[string]string{"list": "/v1/events", "wait": "/v1/events/wait", "stream": "/v1/events/stream"}, "limits": s.discoveryLimits()})
}

type mailboxPermissions struct {
	Role       string `json:"role"`
	CanRead    bool   `json:"can_read"`
	CanDraft   bool   `json:"can_draft"`
	CanSend    bool   `json:"can_send"`
	CanApprove bool   `json:"can_approve"`
}

func effectiveMailboxPermissions(p model.Principal, boxes []model.Inbox) map[string]mailboxPermissions {
	out := make(map[string]mailboxPermissions, len(boxes))
	for _, box := range boxes {
		role := p.Role(box.ID)
		if p.Admin {
			role = "admin"
		}
		out[box.ID] = mailboxPermissions{
			Role:       role,
			CanRead:    p.CanRead(box.ID),
			CanDraft:   p.CanAssist(box.ID),
			CanSend:    p.CanOwn(box.ID),
			CanApprove: p.CanOwn(box.ID),
		}
	}
	return out
}

func (s *Server) apiInboxes(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	switch r.Method {
	case http.MethodGet:
		boxes, err := s.Service.Store.ListInboxes(r.Context(), p)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, boxes)
	case http.MethodPost:
		if !adminOnly(w, p) {
			return
		}
		var in struct {
			DomainID    string `json:"domain_id"`
			LocalPart   string `json:"local_part"`
			Localpart   string `json:"localpart"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		local := in.LocalPart
		if local == "" {
			local = in.Localpart
		}
		box, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, in.DomainID, local, in.DisplayName)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, box)
	}
}
func (s *Server) apiInbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.GetInbox(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPatch:
		if !p.CanOwn(id) && !p.Admin {
			writeError(w, 403, "forbidden")
			return
		}
		var in struct {
			DisplayName          *string            `json:"display_name"`
			Enabled              *bool              `json:"enabled"`
			AllowedSenders       *[]string          `json:"allowed_senders"`
			SenderRestricted     *bool              `json:"sender_restricted"`
			RequireAuthenticated *bool              `json:"require_authenticated"`
			ApproverEmail        *string            `json:"approver_email"`
			Aliases              *[]string          `json:"aliases"`
			AliasNames           *map[string]string `json:"alias_names"`
			DefaultSender        *string            `json:"default_sender"`
			TrashRetentionDays   json.RawMessage    `json:"trash_retention_days"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.ApproverEmail != nil {
			normalized, err := normalizeApproverEmail(*in.ApproverEmail)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			if err := s.Service.Store.SetInboxApprover(r.Context(), p.AccountID, id, normalized); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		display := ""
		if in.DisplayName != nil {
			display = *in.DisplayName
		}
		if err := s.Service.Store.UpdateInbox(r.Context(), p, id, display, in.Enabled); err != nil {
			mapStoreError(w, err)
			return
		}
		// allowed_senders implies restriction when sender_restricted is absent,
		// preserving the pre-toggle API contract for older clients.
		restricted := in.SenderRestricted
		if in.AllowedSenders != nil {
			senders, err := normalizeAllowedSenders(*in.AllowedSenders)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			if err = s.Service.Store.SetInboxAllowedSenders(r.Context(), p.AccountID, id, senders); err != nil {
				mapStoreError(w, err)
				return
			}
			if restricted == nil {
				inferred := len(senders) > 0
				restricted = &inferred
			}
		}
		if restricted != nil {
			if err := s.Service.Store.SetInboxSenderRestricted(r.Context(), p.AccountID, id, *restricted); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.RequireAuthenticated != nil {
			if err := s.Service.Store.SetInboxRequireAuthenticated(r.Context(), p.AccountID, id, *in.RequireAuthenticated); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.Aliases != nil || in.AliasNames != nil {
			var forms []aliasForm
			if in.Aliases != nil {
				var err error
				forms, err = normalizeAliasForms(*in.Aliases)
				if err != nil {
					writeError(w, 400, err.Error())
					return
				}
			} else {
				// Names-only update: keep the existing alias set and apply the
				// supplied display names.
				box, gerr := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id)
				if gerr != nil {
					mapStoreError(w, gerr)
					return
				}
				for _, addr := range box.Aliases {
					at := strings.LastIndex(addr, "@")
					if at <= 0 {
						continue
					}
					forms = append(forms, aliasForm{
						LocalPart:   strings.ToLower(addr[:at]),
						DomainName:  strings.ToLower(addr[at+1:]),
						DisplayName: box.AliasNames[addr],
					})
				}
			}
			if in.AliasNames != nil {
				names := map[string]string{}
				for addr, name := range *in.AliasNames {
					normalized, nerr := store.NormalizeAliasDisplayName(name)
					if nerr != nil {
						writeError(w, 400, nerr.Error())
						return
					}
					names[strings.ToLower(strings.TrimSpace(addr))] = normalized
				}
				for i := range forms {
					key := strings.ToLower(forms[i].LocalPart + "@" + forms[i].DomainName)
					if name, ok := names[key]; ok {
						forms[i].DisplayName = name
					}
				}
			}
			if err := s.applyInboxAliases(r.Context(), p.AccountID, id, forms); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.DefaultSender != nil {
			// Apply after aliases so a sender may reference a just-set alias.
			if err := s.Service.Store.SetInboxDefaultSender(r.Context(), p.AccountID, id, *in.DefaultSender); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		// trash_retention_days: absent leaves the override unchanged; an
		// explicit null clears it so the inbox inherits the account default; a
		// number sets the inbox override (0 keeps trash until purged by hand).
		if len(in.TrashRetentionDays) > 0 {
			var days *int
			if string(in.TrashRetentionDays) != "null" {
				var n int
				if err := json.Unmarshal(in.TrashRetentionDays, &n); err != nil {
					writeError(w, 400, "trash_retention_days must be an integer or null")
					return
				}
				days = &n
			}
			if err := s.Service.Store.SetInboxTrashRetention(r.Context(), p.AccountID, id, days); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		v, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodDelete:
		if !adminOnly(w, p) {
			return
		}
		paths, err := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		for _, path := range paths {
			s.removeDataFile(path)
		}
		w.WriteHeader(204)
	}
}

func (s *Server) apiIdentities(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		boxes, err := s.Service.Store.ListInboxes(r.Context(), p)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(boxes))
		for _, b := range boxes {
			items = append(items, map[string]any{"address": b.Address, "name": b.DisplayName, "createdAt": b.CreatedAt, "pushContentTier": 1})
		}
		writeJSON(w, 200, map[string]any{"identities": items})
	case http.MethodPost:
		var in struct {
			Name      string `json:"name"`
			LocalPart string `json:"localpart"`
			DomainID  string `json:"domain_id,omitempty"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		domainID := in.DomainID
		if domainID == "" {
			ds, err := s.Service.Store.ListDomains(r.Context(), p.AccountID)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			if len(ds) == 0 {
				writeError(w, 400, "add a domain first")
				return
			}
			if len(ds) > 1 {
				writeError(w, 400, "domain_id is required when the account has multiple domains")
				return
			}
			domainID = ds[0].ID
		}
		local := strings.TrimSpace(in.LocalPart)
		if local == "" {
			local = "agent-" + strings.ToLower(strings.TrimPrefix(idgen.New("id"), "id_"))[:8]
		}
		box, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, domainID, local, in.Name)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		_, tok, err := s.Service.Store.CreateAPIKey(r.Context(), p.AccountID, "identity:"+box.Address, false, map[string]string{box.ID: "owner"})
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"address": box.Address, "name": box.DisplayName, "pushContentTier": 1, "token": tok})
	}
}
func (s *Server) apiIdentityDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	addr, err := url.PathUnescape(r.PathValue("address"))
	if err != nil {
		writeError(w, 400, "invalid address")
		return
	}
	boxes, err := s.Service.Store.ListInboxes(r.Context(), p)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	for _, b := range boxes {
		if strings.EqualFold(b.Address, addr) {
			paths, err := s.Service.Store.PurgeInbox(r.Context(), p.AccountID, b.ID)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			for _, path := range paths {
				s.removeDataFile(path)
			}
			writeJSON(w, 200, map[string]bool{"deleted": true})
			return
		}
	}
	writeError(w, 404, "not found")
}
func inboxByAddress(ctx context.Context, st *store.Store, p model.Principal, address string) (model.Inbox, error) {
	boxes, err := st.ListInboxes(ctx, p)
	if err != nil {
		return model.Inbox{}, err
	}
	for _, b := range boxes {
		if strings.EqualFold(b.Address, strings.TrimSpace(address)) {
			return b, nil
		}
	}
	return model.Inbox{}, store.ErrNotFound
}
func openAgentMessage(m model.Message) map[string]any {
	when := m.CreatedAt
	if m.ReceivedAt != nil {
		when = *m.ReceivedAt
	}
	snippet := strings.TrimSpace(m.Text)
	if len(snippet) > 240 {
		snippet = snippet[:240]
	}
	return map[string]any{"id": m.ID, "from": m.From.Address, "to": firstString(m.To), "subject": m.Subject, "date": when, "seen": m.Read, "snippet": snippet, "hasOtp": false, "source": "external", "text": m.Text, "html": htmlsanitize.Sanitize(m.HTML), "messageId": m.RFCMessageID, "threadId": m.ThreadID, "hasAttachments": m.HasAttachments}
}
func firstString(v []string) string {
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	unread, ok := boolQuery(w, r, "unread")
	if !ok {
		return
	}
	hasAttachment, ok := boolQuery(w, r, "has_attachment")
	if !ok {
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	spam, ok := boolQuery(w, r, "spam")
	if !ok {
		return
	}
	includeSpam, ok := boolQuery(w, r, "include_spam")
	if !ok {
		return
	}
	trashed, ok := boolQuery(w, r, "trashed")
	if !ok {
		return
	}
	f := store.MessageFilter{InboxID: r.URL.Query().Get("inbox"), ThreadID: r.URL.Query().Get("thread"), From: r.URL.Query().Get("from"), To: r.URL.Query().Get("to"), Unread: unread, HasAttachment: hasAttachment, Labels: r.URL.Query()["label"], Limit: limit}
	if trashed != nil && *trashed {
		f.Trashed = true
		f.IncludeSpam = true
	}
	if spam != nil && *spam {
		f.SpamOnly = true
	} else if includeSpam != nil && *includeSpam {
		f.IncludeSpam = true
	}
	compatAddress := strings.TrimSpace(r.URL.Query().Get("address"))
	if compatAddress != "" {
		b, err := inboxByAddress(r.Context(), s.Service.Store, p, compatAddress)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		f.InboxID = b.ID
	}
	items, err := s.Service.Store.ListMessages(r.Context(), p, f)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if compatAddress != "" {
		out := make([]map[string]any, 0, len(items))
		for _, m := range items {
			out = append(out, openAgentMessage(m))
		}
		writeJSON(w, 200, map[string]any{"messages": out})
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) apiMessage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		m, err := s.Service.Store.GetMessage(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if addr := strings.TrimSpace(r.URL.Query().Get("address")); addr != "" {
			b, e := inboxByAddress(r.Context(), s.Service.Store, p, addr)
			if e != nil || b.ID != m.InboxID {
				writeError(w, 404, "not found")
				return
			}
			writeJSON(w, 200, openAgentMessage(m))
			return
		}
		writeJSON(w, 200, m)
	case http.MethodPatch:
		var in struct {
			Read   *bool     `json:"read"`
			Labels *[]string `json:"labels"`
			Spam   *bool     `json:"spam"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.Read != nil {
			if err := s.Service.Store.UpdateMessageState(r.Context(), p, id, in.Read); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.Spam != nil {
			_, ev, err := s.Service.Store.SetMessageSpam(r.Context(), p, id, *in.Spam)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			if ev != nil {
				s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
				s.Service.Hub.Publish(*ev)
			}
		}
		if in.Labels != nil {
			ev, err := s.Service.Store.ReplaceMessageLabels(r.Context(), p, id, *in.Labels)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
			s.Service.Hub.Publish(ev)
		}
		m, err := s.Service.Store.GetMessage(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, m)
	case http.MethodDelete:
		// Delete moves the message to Trash (recoverable); use the purge route
		// to erase it permanently.
		_, ev, err := s.Service.Store.TrashMessage(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if ev != nil {
			s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
			s.Service.Hub.Publish(*ev)
		}
		w.WriteHeader(204)
	}
}

// apiMessageRestore returns a trashed message to the mailbox.
func (s *Server) apiMessageRestore(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, ev, err := s.Service.Store.RestoreMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	writeJSON(w, 200, m)
}

// apiMessagePurge permanently erases a trashed message and unlinks its file.
func (s *Server) apiMessagePurge(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	path, _, ev, err := s.Service.Store.PurgeMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if path != "" {
		s.removeDataFile(path)
	}
	s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
	s.Service.Hub.Publish(ev)
	w.WriteHeader(204)
}

// apiInboxTrashEmpty permanently purges every trashed message in an inbox.
func (s *Server) apiInboxTrashEmpty(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	paths, events, err := s.Service.Store.EmptyTrash(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	for _, path := range paths {
		s.removeDataFile(path)
	}
	for _, ev := range events {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(ev)
	}
	writeJSON(w, 200, map[string]int{"purged": len(events)})
}
func (s *Server) apiSeen(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	seen := true
	var in struct {
		Address string `json:"address"`
		Seen    *bool  `json:"seen"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	}
	if in.Seen != nil {
		seen = *in.Seen
	}
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if in.Address != "" {
		b, e := inboxByAddress(r.Context(), s.Service.Store, p, in.Address)
		if e != nil || b.ID != m.InboxID {
			writeError(w, 404, "not found")
			return
		}
	}
	if err = s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &seen); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": m.ID, "seen": seen})
}
func (s *Server) apiMessageAttachments(w http.ResponseWriter, r *http.Request) {
	items, err := s.Service.Store.ListAttachments(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) apiAttachment(w http.ResponseWriter, r *http.Request) {
	a, m, err := s.Service.Store.GetAttachment(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	path, perr := s.dataPath(m.RawPath)
	if perr != nil {
		writeError(w, 500, "internal error")
		return
	}
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	w.Header().Set("Content-Disposition", disp)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := mailparse.ExtractAttachment(path, a.PartIndex, w); err != nil {
		s.Log.Error("attachment extraction", "error", err)
	}
}

func (s *Server) apiThreads(w http.ResponseWriter, r *http.Request) {
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	items, err := s.Service.Store.ListThreads(r.Context(), principal(r), r.URL.Query().Get("inbox"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) apiThread(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	msgs, err := s.Service.Store.ListMessages(r.Context(), p, store.MessageFilter{ThreadID: r.PathValue("id"), Limit: 200})
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if len(msgs) == 0 {
		writeError(w, 404, "thread not found")
		return
	}
	writeJSON(w, 200, map[string]any{"id": r.PathValue("id"), "inbox_id": msgs[0].InboxID, "subject": msgs[len(msgs)-1].Subject, "message_count": len(msgs), "messages": msgs})
}
func (s *Server) apiThreadMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.Service.Store.ListMessages(r.Context(), principal(r), store.MessageFilter{ThreadID: r.PathValue("id"), Limit: 200})
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, msgs)
}
func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	hasAttachment, ok := boolQuery(w, r, "has_attachment")
	if !ok {
		return
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	items, err := s.Service.Store.SearchMessagesFiltered(r.Context(), principal(r), r.URL.Query().Get("q"), store.MessageFilter{
		InboxID:       r.URL.Query().Get("inbox"),
		From:          r.URL.Query().Get("from"),
		To:            r.URL.Query().Get("to"),
		Before:        r.URL.Query().Get("before"),
		Labels:        r.URL.Query()["label"],
		HasAttachment: hasAttachment,
		Limit:         limit,
	})
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) apiLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := s.Service.Store.ListLabels(r.Context(), principal(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, labels)
}

type stringList []string

func (x *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		if strings.TrimSpace(one) != "" {
			*x = []string{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*x = many
	return nil
}
func (s *Server) apiSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	key := p.AccountID
	if p.APIKeyID != "" {
		key = p.APIKeyID
	}
	if !s.sendLimiter.Allow(key) {
		writeError(w, 429, "send rate limit exceeded")
		return
	}
	var in struct {
		InboxID string `json:"inbox_id"`
		// From selects the inbox when inbox_id is omitted; Sender chooses the
		// sending identity (primary or an alias) once the inbox is known.
		From        string               `json:"from"`
		Sender      string               `json:"sender"`
		To          stringList           `json:"to"`
		CC          stringList           `json:"cc,omitempty"`
		BCC         stringList           `json:"bcc,omitempty"`
		Subject     string               `json:"subject"`
		Text        string               `json:"text"`
		HTML        string               `json:"html,omitempty"`
		Attachments []app.SendAttachment `json:"attachments,omitempty"`
	}
	if !decodeJSONLimit(w, r, &in, s.Service.Config.MaxMessageBytes*2) {
		return
	}
	if in.InboxID == "" && in.From != "" {
		b, err := inboxByAddress(r.Context(), s.Service.Store, p, in.From)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		in.InboxID = b.ID
	}
	if in.InboxID == "" && !p.Admin {
		for id, role := range p.MailboxRoles {
			if role == "owner" {
				if in.InboxID != "" {
					writeError(w, 400, "inbox_id or from is required for keys owning multiple inboxes")
					return
				}
				in.InboxID = id
			}
		}
	}
	if in.InboxID == "" {
		writeError(w, 400, "inbox_id or from is required")
		return
	}
	res, err := s.Service.Send(r.Context(), p, app.SendInput{InboxID: in.InboxID, FromAddress: in.Sender, To: []string(in.To), CC: []string(in.CC), BCC: []string(in.BCC), Subject: in.Subject, Text: in.Text, HTML: in.HTML, Attachments: in.Attachments}, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	// Async by default: return the pending message immediately. With ?wait=true
	// the request blocks until the worker delivers or fails (or times out).
	if r.URL.Query().Get("wait") == "true" {
		m, werr := s.Service.WaitForDelivery(r.Context(), p.AccountID, res.Message.ID, 30*time.Second)
		if werr != nil {
			writeError(w, 504, werr.Error())
			return
		}
		res.Message = m
		res.ProviderMessageID = m.ProviderMessageID
	}
	writeJSON(w, 200, map[string]any{"queued": res.Message.Status == "pending", "messageId": res.Message.RFCMessageID, "provider_message_id": res.ProviderMessageID, "message": res.Message})
}
func (s *Server) apiReply(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.Service.Store.GetMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	var in struct {
		Sender      string               `json:"sender"`
		Text        string               `json:"text"`
		HTML        string               `json:"html,omitempty"`
		Attachments []app.SendAttachment `json:"attachments,omitempty"`
	}
	if !decodeJSONLimit(w, r, &in, s.Service.Config.MaxMessageBytes*2) {
		return
	}
	res, err := s.Service.Send(r.Context(), p, app.SendInput{InboxID: m.InboxID, FromAddress: in.Sender, ReplyToMessageID: m.ID, Text: in.Text, HTML: in.HTML, Attachments: in.Attachments}, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	// Async by default: return the pending message immediately. With ?wait=true
	// the request blocks until the worker delivers or fails (or times out).
	if r.URL.Query().Get("wait") == "true" {
		dm, werr := s.Service.WaitForDelivery(r.Context(), p.AccountID, res.Message.ID, 30*time.Second)
		if werr != nil {
			writeError(w, 504, werr.Error())
			return
		}
		res.Message = dm
		res.ProviderMessageID = dm.ProviderMessageID
	}
	writeJSON(w, 201, res)
}

func (s *Server) apiDrafts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	switch r.Method {
	case http.MethodGet:
		limit, ok := limitQuery(w, r)
		if !ok {
			return
		}
		v, err := s.Service.Store.ListDraftsPaged(r.Context(), p, r.URL.Query().Get("inbox"), r.URL.Query().Get("before"), limit)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		in, ok := s.decodeDraftWrite(w, r)
		if !ok {
			return
		}
		action, err := normalizeDraftAction(in.Action)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		d := draftFromInput(in)
		v, err := s.Service.Store.CreateDraft(r.Context(), p, d)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if len(in.Attachments) > 0 {
			if _, err = s.persistDraftAttachments(r.Context(), p, v.ID, in.Attachments); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		s.writeDraftResult(w, r, p, v.ID, action, in.External, http.StatusCreated)
	}
}
func (s *Server) apiDraft(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.GetDraft(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPatch:
		in, ok := s.decodeDraftWrite(w, r)
		if !ok {
			return
		}
		action, err := normalizeDraftAction(in.Action)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		old, err := s.Service.Store.GetDraft(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		d := patchDraft(in, old)
		v, err := s.Service.Store.UpdateDraft(r.Context(), p, d)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if len(in.Attachments) > 0 {
			if _, err = s.persistDraftAttachments(r.Context(), p, v.ID, in.Attachments); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		s.writeDraftResult(w, r, p, v.ID, action, in.External, http.StatusOK)
	case http.MethodDelete:
		paths, err := s.Service.Store.DeleteDraftCascade(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		for _, path := range paths {
			s.removeDataFile(path)
		}
		w.WriteHeader(204)
	}
}

// writeDraftResult performs the action selected by a draft write and writes the
// response. "draft" returns the saved draft, "request-send" submits it for
// approval (Assistant) and "send" sends it now (Owner).
func (s *Server) writeDraftResult(w http.ResponseWriter, r *http.Request, p model.Principal, draftID, action string, external bool, okStatus int) {
	switch action {
	case "send":
		d, err := s.Service.Store.GetDraft(r.Context(), p, draftID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		res, err := s.Service.SendDraft(r.Context(), p, draftID, app.SendInput{InboxID: d.InboxID, FromAddress: d.FromAddress, FromName: d.FromName, ReplyToMessageID: d.ReplyToMessageID, To: d.To, CC: d.CC, BCC: d.BCC, Subject: d.Subject, Text: d.Text, HTML: d.HTML}, idemKey(r))
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"queued": res.Message.Status == "pending", "messageId": res.Message.RFCMessageID, "provider_message_id": res.ProviderMessageID, "message": res.Message})
	case "request-send":
		d, err := s.Service.RequestSend(r.Context(), p, draftID, external)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, okStatus, d)
	default:
		d, err := s.Service.Store.GetDraft(r.Context(), p, draftID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, okStatus, d)
	}
}

// apiDraftSend sends a draft: it copies the draft's fields and attachments into
// a new pending outbound message, then deletes the draft. Requires owner. An
// optional body may override the stored fields and append attachments before
// sending, so an owner can edit and send in one request.
func (s *Server) apiDraftSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	in, ok := s.decodeDraftWrite(w, r)
	if !ok {
		return
	}
	d, err := s.Service.Store.GetDraft(r.Context(), p, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !p.CanOwn(d.InboxID) {
		mapStoreError(w, store.ErrForbidden)
		return
	}
	if in.To != nil || in.CC != nil || in.BCC != nil || in.Subject != nil || in.Text != nil || in.HTML != nil || in.FromAddress != "" || in.Sender != "" {
		if _, err = s.Service.Store.UpdateDraft(r.Context(), p, patchDraft(in, d)); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	if len(in.Attachments) > 0 {
		if _, err = s.persistDraftAttachments(r.Context(), p, id, in.Attachments); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	s.writeDraftResult(w, r, p, id, "send", false, http.StatusOK)
}

// maxFeedbackBytes bounds agent/human feedback stored against a send request.
const maxFeedbackBytes = 4096

// apiDraftRequestSend records an assistant's request that a draft be authorized
// and sent. The draft is frozen until the request is decided or cancelled.
func (s *Server) apiDraftRequestSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	key := p.AccountID
	if p.APIKeyID != "" {
		key = p.APIKeyID
	}
	if !s.sendLimiter.Allow(key) {
		writeError(w, 429, "send rate limit exceeded")
		return
	}
	in, ok := s.decodeDraftWrite(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	d, err := s.Service.Store.GetDraft(r.Context(), p, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	// Optional body overrides and attachments are applied before freezing, so an
	// assistant can edit and submit in one request.
	if in.To != nil || in.CC != nil || in.BCC != nil || in.Subject != nil || in.Text != nil || in.HTML != nil || in.FromAddress != "" || in.Sender != "" {
		if _, err = s.Service.Store.UpdateDraft(r.Context(), p, patchDraft(in, d)); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	if len(in.Attachments) > 0 {
		if _, err = s.persistDraftAttachments(r.Context(), p, id, in.Attachments); err != nil {
			mapStoreError(w, err)
			return
		}
	}
	d, err = s.Service.RequestSend(r.Context(), p, id, in.External)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, d)
}

// apiDraftCancelSendRequest withdraws an outstanding request and unfreezes the
// draft.
func (s *Server) apiDraftCancelSendRequest(w http.ResponseWriter, r *http.Request) {
	d, err := s.Service.CancelSendRequest(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, d)
}

// apiDraftApprove authorizes a pending request and enqueues the frozen draft
// through the existing send flow. Requires owner.
func (s *Server) apiDraftApprove(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		Feedback string `json:"feedback"`
	}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &in) {
			return
		}
	}
	if len(in.Feedback) > maxFeedbackBytes {
		writeError(w, 400, "feedback is too long")
		return
	}
	res, err := s.Service.ApproveDraft(r.Context(), p, r.PathValue("id"), in.Feedback, model.DecisionMethodAPI, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"queued": true, "messageId": res.Message.RFCMessageID, "message": res.Message})
}

// apiDraftReject rejects a pending request with optional feedback. Requires
// owner.
func (s *Server) apiDraftReject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Feedback string `json:"feedback"`
	}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &in) {
			return
		}
	}
	if len(in.Feedback) > maxFeedbackBytes {
		writeError(w, 400, "feedback is too long")
		return
	}
	d, err := s.Service.RejectDraft(r.Context(), principal(r), r.PathValue("id"), in.Feedback, model.DecisionMethodAPI)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, d)
}

// apiDraftSendRequest returns the most recent send request for a draft. It
// works after the draft has been consumed by an approved send.
func (s *Server) apiDraftSendRequest(w http.ResponseWriter, r *http.Request) {
	sr, err := s.Service.Store.GetSendRequestByDraft(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, sr)
}

// apiSendRequests lists send requests, optionally scoped to an inbox or to
// outstanding requests only.
func (s *Server) apiSendRequests(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	activeOnly := r.URL.Query().Get("active") == "true"
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	v, err := s.Service.Store.ListSendRequests(r.Context(), p, r.URL.Query().Get("inbox"), activeOnly, limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

// apiDraftAttachments uploads (POST) or lists (GET) a draft's attachments.
func (s *Server) apiDraftAttachments(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.ListDraftAttachments(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		// Bound the request body before multipart parsing: ParseMultipartForm
		// spills oversized parts to temp files, so without a cap an
		// authenticated caller could exhaust disk before the per-attachment
		// size check runs. The ceiling allows the configured message size plus
		// multipart overhead.
		r.Body = http.MaxBytesReader(w, r.Body, s.Service.Config.MaxMessageBytes*2+1<<20)
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			writeError(w, 400, "invalid multipart form")
			return
		}
		atts, err := s.formAttachments(r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if len(atts) == 0 {
			writeError(w, 400, "no attachments provided")
			return
		}
		out, err := s.persistDraftAttachments(r.Context(), p, id, atts)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, out)
	}
}

// apiDraftAttachmentContent downloads a single draft attachment's bytes,
// mirroring the message attachment download headers.
func (s *Server) apiDraftAttachmentContent(w http.ResponseWriter, r *http.Request) {
	a, err := s.Service.Store.GetDraftAttachment(r.Context(), principal(r), r.PathValue("id"), r.PathValue("attId"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	path, perr := s.dataPath(a.RawPath)
	if perr != nil {
		writeError(w, 404, "not found")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, 404, "not found")
		return
	}
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	w.Header().Set("Content-Disposition", disp)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// apiDraftAttachment deletes a single draft attachment.
func (s *Server) apiDraftAttachment(w http.ResponseWriter, r *http.Request) {
	raw, err := s.Service.Store.DeleteDraftAttachment(r.Context(), principal(r), r.PathValue("id"), r.PathValue("attId"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if raw != "" {
		s.removeDataFile(raw)
	}
	w.WriteHeader(204)
}

func (s *Server) apiOutbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	v, err := s.Service.Store.ListOutbox(r.Context(), p, r.URL.Query().Get("inbox"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) apiOutboxRetry(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.Store.RequeueFailed(r.Context(), p, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) apiOutboxDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_, ev, err := s.Service.Store.DeleteOutboxMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if ev != nil {
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
		s.Service.Hub.Publish(*ev)
	}
	w.WriteHeader(204)
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	after := store.ParseCursor(r.URL.Query().Get("after"))
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	v, err := s.Service.Store.ListEvents(r.Context(), principal(r), after, r.URL.Query().Get("inbox"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) waitEvents(r *http.Request, after int64, inbox string, timeout time.Duration) ([]model.Event, error) {
	p := principal(r)
	items, err := s.Service.Store.ListEvents(r.Context(), p, after, inbox, 100)
	if err != nil || len(items) > 0 {
		return items, err
	}
	_, ch, cancel := s.Service.Hub.Subscribe(16)
	defer cancel()
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case <-ch:
			items, err = s.Service.Store.ListEvents(r.Context(), p, after, inbox, 100)
			if err != nil || len(items) > 0 {
				return items, err
			}
		case <-t.C:
			return []model.Event{}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
}
func (s *Server) apiEventsWait(w http.ResponseWriter, r *http.Request) {
	sec, ok := intQuery(w, r, "timeout", 60)
	if !ok {
		return
	}
	if sec < 1 {
		sec = 1
	}
	if sec > 60 {
		sec = 60
	}
	p := principal(r)
	waitKey := credentialKey(p)
	if !s.waitLimiter.acquire(waitKey) {
		writeError(w, 429, "too many concurrent waits")
		return
	}
	defer s.waitLimiter.release(waitKey)
	// With no cursor, wait for events after the current head so the call blocks
	// for new events instead of replaying history. An explicit cursor keeps the
	// documented drain semantics (return anything already after it).
	rawAfter := strings.TrimSpace(r.URL.Query().Get("after"))
	var after int64
	if rawAfter == "" {
		head, err := s.Service.Store.LatestEventID(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		after = head
	} else {
		parsed, ok := store.ParseCursorStrict(rawAfter)
		if !ok {
			writeError(w, 400, "invalid after: must be an evt_ cursor")
			return
		}
		after = parsed
	}
	v, err := s.waitEvents(r, after, r.URL.Query().Get("inbox"), time.Duration(sec)*time.Second)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) apiEventsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unavailable")
		return
	}
	p := principal(r)
	// Revoking, rotating, or rescoping the credential cancels this stream.
	streamKey := credentialKey(p)
	if !s.streamLimiter.acquire(streamKey) {
		writeError(w, 429, "too many concurrent streams")
		return
	}
	defer s.streamLimiter.release(streamKey)
	scopeCtx, unregister := s.Service.Hub.RegisterScope(p.Scopes()...)
	defer unregister()
	ctx, cancelCtx := context.WithCancel(r.Context())
	defer cancelCtx()
	stop := context.AfterFunc(scopeCtx, cancelCtx)
	defer stop()
	after := store.ParseCursor(r.URL.Query().Get("after"))
	inbox := r.URL.Query().Get("inbox")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, ch, cancelSub := s.Service.Hub.Subscribe(32)
	defer cancelSub()
	bw := bufio.NewWriter(w)
	// Flush an initial comment so clients know the stream is established.
	fmt.Fprint(bw, ": connected\n\n")
	_ = bw.Flush()
	fl.Flush()
	send := func(e model.Event) error {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(bw, "id: %s\nevent: %s\ndata: %s\n\n", e.Cursor, e.Type, b); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		fl.Flush()
		after = e.ID
		return nil
	}
	for {
		items, err := s.Service.Store.ListEvents(ctx, p, after, inbox, 500)
		if err != nil {
			return
		}
		for _, e := range items {
			if send(e) != nil {
				return
			}
		}
		select {
		case <-ch:
			continue
		case <-time.After(20 * time.Second):
			fmt.Fprint(bw, ": keepalive\n\n")
			_ = bw.Flush()
			fl.Flush()
		case <-ctx.Done():
			return
		}
	}
}
func (s *Server) apiMessagesWait(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	waitKey := credentialKey(p)
	if !s.waitLimiter.acquire(waitKey) {
		writeError(w, 429, "too many concurrent waits")
		return
	}
	defer s.waitLimiter.release(waitKey)
	q := r.URL.Query()
	rawAfter := strings.TrimSpace(q.Get("after"))
	inboxID := q.Get("inbox")
	timeoutSec, ok := intQuery(w, r, "timeout", 60)
	if !ok {
		return
	}
	fromContains := ""
	subjectContains := ""
	compat := false
	includeSpam := false
	if b, ok := boolQuery(w, r, "include_spam"); !ok {
		return
	} else if b != nil {
		includeSpam = *b
	}
	if r.Method == http.MethodPost {
		var in struct {
			After           string `json:"after"`
			Inbox           string `json:"inbox"`
			Timeout         int    `json:"timeout"`
			Address         string `json:"address"`
			FromContains    string `json:"fromContains"`
			SubjectContains string `json:"subjectContains"`
			TimeoutSec      int    `json:"timeoutSec"`
			IncludeSpam     *bool  `json:"include_spam"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
		if in.After != "" {
			rawAfter = strings.TrimSpace(in.After)
		}
		if in.Inbox != "" {
			inboxID = in.Inbox
		}
		if in.Timeout > 0 {
			timeoutSec = in.Timeout
		}
		if in.Address != "" {
			b, err := inboxByAddress(r.Context(), s.Service.Store, p, in.Address)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			inboxID = b.ID
			compat = true
		}
		fromContains = strings.ToLower(in.FromContains)
		subjectContains = strings.ToLower(in.SubjectContains)
		if in.IncludeSpam != nil {
			includeSpam = *in.IncludeSpam
		}
		if in.TimeoutSec > 0 {
			timeoutSec = in.TimeoutSec
			compat = true
		}
	}
	// Resolve the cursor. An explicit cursor drains from there. With none, the
	// native path waits for genuinely new mail (seed from the current head)
	// rather than replaying history, while the openagent.email compatibility
	// path scans what is already stored so it can return an
	// already-received matching message. A malformed cursor is rejected rather
	// than silently treated as zero.
	var after int64
	switch {
	case rawAfter != "":
		parsed, ok := store.ParseCursorStrict(rawAfter)
		if !ok {
			writeError(w, 400, "invalid after: must be an evt_ cursor")
			return
		}
		after = parsed
	case compat:
		after = 0
	default:
		head, err := s.Service.Store.LatestEventID(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		after = head
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}
	deadline := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer deadline.Stop()
	_, ch, cancel := s.Service.Hub.Subscribe(16)
	defer cancel()
	for {
		eventsList, err := s.Service.Store.ListEvents(r.Context(), p, after, inboxID, 100)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		for _, e := range eventsList {
			if e.ID > after {
				after = e.ID
			}
			if e.Type != "message.received" {
				continue
			}
			m, err := s.Service.Store.GetMessage(r.Context(), p, e.EntityID)
			if err != nil {
				continue
			}
			// Default message waits skip Spam (progressing the cursor) so an
			// automated consumer is not fed quarantine. An explicit
			// include_spam=true opts in.
			if m.Spam && !includeSpam {
				continue
			}
			if fromContains != "" && !strings.Contains(strings.ToLower(m.From.Address), fromContains) {
				continue
			}
			if subjectContains != "" && !strings.Contains(strings.ToLower(m.Subject), subjectContains) {
				continue
			}
			if compat {
				writeJSON(w, 200, openAgentMessage(m))
			} else {
				writeJSON(w, 200, m)
			}
			return
		}
		select {
		case <-ch:
			continue
		case <-deadline.C:
			if compat {
				writeError(w, 408, "timeout")
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) apiDomains(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.ListDomains(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		var in struct {
			Name             string `json:"name"`
			InheritReceiving *bool  `json:"inherit_receiving"`
			InheritSending   *bool  `json:"inherit_sending"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		opts := store.DomainCreateOptions{}
		if in.InheritReceiving != nil {
			opts.DisableReceiving = !*in.InheritReceiving
		}
		if in.InheritSending != nil {
			opts.DisableSending = !*in.InheritSending
		}
		v, err := s.Service.Store.CreateDomainWithOptions(r.Context(), p.AccountID, in.Name, opts)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, v)
	}
}
func (s *Server) apiDomain(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			CatchAllInboxID  *string `json:"catch_all_inbox_id"`
			InheritReceiving *bool   `json:"inherit_receiving"`
			InheritSending   *bool   `json:"inherit_sending"`
			// ParentDomainID links the domain to an ancestor added after it (a
			// non-empty domain id) or unlinks it (an empty string). Omission
			// leaves the current parent unchanged.
			ParentDomainID *string `json:"parent_domain_id"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.ParentDomainID != nil {
			if err := s.Service.Store.SetDomainParent(r.Context(), p.AccountID, id, *in.ParentDomainID); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.CatchAllInboxID != nil {
			if err := s.Service.Store.SetDomainCatchAll(r.Context(), p.AccountID, id, *in.CatchAllInboxID); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		if in.InheritReceiving != nil || in.InheritSending != nil {
			d, err := s.Service.Store.GetDomain(r.Context(), p.AccountID, id)
			if err != nil {
				mapStoreError(w, err)
				return
			}
			recv, send := d.InheritReceiving, d.InheritSending
			if in.InheritReceiving != nil {
				recv = *in.InheritReceiving
			}
			if in.InheritSending != nil {
				send = *in.InheritSending
			}
			if err := s.Service.Store.SetDomainInheritance(r.Context(), p.AccountID, id, recv, send); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"updated": true})
	case http.MethodDelete:
		paths, err := s.Service.Store.PurgeDomain(r.Context(), p.AccountID, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		for _, path := range paths {
			s.removeDataFile(path)
		}
		w.WriteHeader(204)
	}
}
func (s *Server) apiKeys(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.ListAPIKeys(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		var in struct {
			Name      string            `json:"name"`
			Admin     bool              `json:"admin"`
			Mailboxes map[string]string `json:"mailboxes"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		k, plain, err := s.Service.Store.CreateAPIKey(r.Context(), p.AccountID, in.Name, in.Admin, in.Mailboxes)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"key": k, "token": plain})
	}
}
func (s *Server) apiKey(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	if err := s.Service.Store.RevokeAPIKey(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	s.Service.Hub.CancelScope("key:" + r.PathValue("id"))
	s.Service.Store.DeleteKeySessionsForClient(r.Context(), r.PathValue("id"))
	w.WriteHeader(204)
}

func (s *Server) apiClients(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	keys, err := s.Service.Store.ListAPIKeys(r.Context(), p.AccountID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	hermes, err := s.Service.Store.ListHermesConnections(r.Context(), p.AccountID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	webhooks, err := s.Service.Store.ListWebhookClients(r.Context(), p.AccountID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": keys, "hermes": hermes, "webhooks": webhooks})
}

type webhookCreateRequest struct {
	InboxID      string `json:"inbox_id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Mode         string `json:"mode"`
	Auth         string `json:"auth"`
	BearerSecret string `json:"bearer_secret"`
}

func (s *Server) apiWebhookClients(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	if r.Method == http.MethodGet {
		v, err := s.Service.Store.ListWebhookClients(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		for i := range v {
			v[i].SecretEncrypted = ""
		}
		writeJSON(w, 200, v)
		return
	}
	var in webhookCreateRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := validateWebhookConfig(in.URL, in.Mode, in.Auth); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := validateWebhookBearerSecret(in.Auth, in.BearerSecret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	secret, err := webhookSecret(in.BearerSecret)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "webhook client creation failed"})
		return
	}
	encrypted, err := s.Service.EncryptSecret([]byte(secret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "webhook client creation failed"})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "Webhook"
	}
	c, err := s.Service.Store.CreateWebhookClient(r.Context(), p.AccountID, in.InboxID, name, strings.TrimSpace(in.URL), in.Mode, in.Auth, encrypted)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	c.SecretEncrypted = ""
	writeJSON(w, http.StatusCreated, map[string]any{"client": c, "secret": secret})
}

func (s *Server) apiWebhookClient(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		if err := s.Service.Store.DeleteWebhookClient(r.Context(), p.AccountID, id); err != nil {
			mapStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var in webhookCreateRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := validateWebhookConfig(in.URL, in.Mode, in.Auth); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := validateWebhookBearerSecret(in.Auth, in.BearerSecret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	encrypted := ""
	if in.BearerSecret != "" {
		var err error
		encrypted, err = s.Service.EncryptSecret([]byte(in.BearerSecret))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "webhook client update failed"})
			return
		}
	}
	if err := s.Service.Store.UpdateWebhookClientWithSecret(r.Context(), p.AccountID, id, strings.TrimSpace(in.Name), strings.TrimSpace(in.URL), in.Mode, in.Auth, encrypted); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) apiWebhookRotate(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	secret, err := auth.RandomToken(32)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "secret rotation failed"})
		return
	}
	encrypted, err := s.Service.EncryptSecret([]byte(secret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "secret rotation failed"})
		return
	}
	if err = s.Service.Store.RotateWebhookSecret(r.Context(), p.AccountID, r.PathValue("id"), encrypted); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"secret": secret})
}

func (s *Server) apiWebhookEnable(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.Service.Store.SetWebhookEnabled(r.Context(), p.AccountID, r.PathValue("id"), in.Enabled); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"enabled": in.Enabled})
}

func webhookSecret(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return auth.RandomToken(32)
}

func validateWebhookBearerSecret(authMode, secret string) error {
	if secret == "" {
		return nil
	}
	if authMode != "bearer" {
		return fmt.Errorf("bearer_secret requires bearer authentication")
	}
	// RFC 6750 b64token: header-safe token characters followed by optional padding.
	padding := false
	for i := 0; i < len(secret); i++ {
		c := secret[i]
		if c == '=' && i > 0 {
			padding = true
			continue
		}
		if padding || !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~+/", rune(c))) {
			return fmt.Errorf("paste only the bearer token, without the Bearer prefix, whitespace or control characters")
		}
	}
	return nil
}

func validateWebhookConfig(raw, mode, authMode string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("webhook URL must be an HTTPS URL without userinfo or fragment")
	}
	if mode != "notify" && mode != "forward" {
		return fmt.Errorf("mode must be notify or forward")
	}
	if authMode != "signature" && authMode != "bearer" {
		return fmt.Errorf("auth must be signature or bearer")
	}
	return netutil.ValidateBaseURL(raw)
}

func (s *Server) apiHermesEnroll(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		InboxID string `json:"inbox_id"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	gatewayID, secret, deliveryKey, err := s.Service.CreateHermesRelay(r.Context(), p, in.InboxID, in.Name)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"gateway_id": gatewayID, "secret": secret, "delivery_key": deliveryKey, "connector_url": s.Service.Config.BaseURL, "env": hermesEnvBlock(s.Service.Config.BaseURL, gatewayID, secret, deliveryKey)})
}
func (s *Server) apiHermesList(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	v, err := s.Service.Store.ListHermesConnections(r.Context(), p.AccountID)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	type item struct {
		ID, InboxID, Name, GatewayID, OutboundRole string
		LastAckEventID                             int64
		CreatedAt                                  time.Time
		LastConnectedAt                            *time.Time
	}
	out := []item{}
	for _, h := range v {
		out = append(out, item{h.ID, h.InboxID, h.Name, h.GatewayID, h.OutboundRole, h.LastAckEventID, h.CreatedAt, h.LastConnectedAt})
	}
	writeJSON(w, 200, out)
}

// apiHermesConnection updates a relay connection's outbound role. An Assistant
// role makes the relay draft-and-request-approval instead of sending.
func (s *Server) apiHermesConnection(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.Service.Store.SetHermesOutboundRole(r.Context(), p.AccountID, r.PathValue("id"), in.Role); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "role": strings.ToLower(strings.TrimSpace(in.Role))})
}

func (s *Server) apiHermesDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	if err := s.Service.Store.DeleteHermesConnection(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	w.WriteHeader(204)
}

// apiOpenClawEnroll creates an OpenClaw relay connector directly and returns
// its one-time credentials, mirroring the Hermes enroll endpoint. OpenClaw
// shares the relay transport; only the stored connector kind differs.
func (s *Server) apiOpenClawEnroll(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		InboxID string `json:"inbox_id"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	gatewayID, secret, deliveryKey, err := s.Service.CreateRelay(r.Context(), p, in.InboxID, in.Name, store.KindOpenClaw)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"gateway_id": gatewayID, "secret": secret, "delivery_key": deliveryKey, "connector_url": s.Service.Config.BaseURL, "kind": "openclaw"})
}

// apiOpenClawList lists the account's OpenClaw relay connectors, never secrets.
func (s *Server) apiOpenClawList(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	v, err := s.Service.Store.ListRelayConnections(r.Context(), p.AccountID, store.KindOpenClaw)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	type item struct {
		ID, InboxID, Name, GatewayID, OutboundRole string
		LastAckEventID                             int64
		CreatedAt                                  time.Time
		LastConnectedAt                            *time.Time
	}
	out := []item{}
	for _, h := range v {
		out = append(out, item{h.ID, h.InboxID, h.Name, h.GatewayID, h.OutboundRole, h.LastAckEventID, h.CreatedAt, h.LastConnectedAt})
	}
	writeJSON(w, 200, out)
}

// apiOpenClawConnection updates an OpenClaw connector's outbound role.
func (s *Server) apiOpenClawConnection(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.Service.Store.SetHermesOutboundRole(r.Context(), p.AccountID, r.PathValue("id"), in.Role); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "role": strings.ToLower(strings.TrimSpace(in.Role))})
}

// apiOpenClawDelete removes an OpenClaw connector and immediately closes its
// live relay socket.
func (s *Server) apiOpenClawDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	if err := s.Service.Store.DeleteHermesConnection(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	w.WriteHeader(204)
}

// apiOpenClawSetupCode mints a one-time setup code for the OpenClaw plugin's
// setup wizard, plus the exact claim URL and a ready-to-paste CLI command.
// The code is returned once; only its hash is stored.
func (s *Server) apiOpenClawSetupCode(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	var in struct {
		InboxID string `json:"inbox_id"`
		Name    string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	code, err := s.Service.CreateRelayEnrollCode(r.Context(), p, in.InboxID, in.Name, store.KindOpenClaw, 15*time.Minute)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	base := strings.TrimRight(s.Service.Config.BaseURL, "/")
	writeJSON(w, 201, map[string]any{
		"code":       code,
		"expires_in": 900,
		"setup_url":  base + "/#" + code,
		"command":    "openclaw channels add --channel mailmoose --code " + base + "/#" + code,
	})
}

func (s *Server) mailgunIngest(w http.ResponseWriter, r *http.Request) {
	s.ingestProvider(w, r, "mailgun")
}

func (s *Server) ingestInbound(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	// The legacy /internal/ingest/mailgun alias was removed. Mailgun must use
	// the raw-MIME endpoint, whose suffix is protocol-significant.
	if provider == "mailgun" {
		http.NotFound(w, r)
		return
	}
	// The MX provider does not accept webhook dispatch; its mail arrives through
	// the signed /internal/mx endpoints.
	if provider == "mx" || provider == "dialmx" {
		http.NotFound(w, r)
		return
	}
	s.ingestProvider(w, r, provider)
}

func (s *Server) ingestProvider(w http.ResponseWriter, r *http.Request, provider string) {
	// Bound the total request body before any transport reads it. The
	// transport itself applies a tighter per-message cap; this is a hard
	// ceiling that also covers multipart overhead and form fields.
	r.Body = http.MaxBytesReader(w, r.Body, s.Service.Config.MaxMessageBytes*2+1<<20)
	select {
	case s.inboundSem <- struct{}{}:
		defer func() { <-s.inboundSem }()
	case <-r.Context().Done():
		http.Error(w, "request cancelled", 499)
		return
	}
	ctx := r.Context()
	if provider == "resend" {
		// Resend posts metadata, then we fetch the MIME before persisting it.
		// A webhook caller can disconnect while that fetch is still running.
		// Keep the entire ingest (including the commit) alive independently,
		// bounded by the 30s metadata + 2m download limits plus commit time.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
	}
	m, dup, err := s.Service.IngestInbound(ctx, provider, r)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "recipient rejected", http.StatusNotAcceptable)
			return
		}
		if errors.Is(err, store.ErrQuota) {
			http.Error(w, "storage quota exceeded", http.StatusNotAcceptable)
			return
		}
		if errors.Is(err, transport.ErrUnknownProvider) {
			http.Error(w, "unknown inbound provider", http.StatusNotFound)
			return
		}
		if errors.Is(err, transport.ErrInboundUnauthorized) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if errors.Is(err, transport.ErrInboundIgnored) {
			// Acknowledge provider events that need no ingest so they are not
			// retried; nothing was persisted.
			writeJSON(w, 200, map[string]any{"accepted": true, "ignored": true})
			return
		}
		// Permanently invalid inbound messages (malformed MIME, oversize,
		// unsupported content type) are terminal: the provider should not
		// retry them.
		if isTerminalInboundError(err) {
			http.Error(w, "invalid message", http.StatusNotAcceptable)
			return
		}
		s.Log.Warn("inbound ingest failed", "provider", provider, "error", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, 200, map[string]any{"accepted": true, "duplicate": dup, "message_id": m.ID})
}

// isTerminalInboundError reports whether an inbound error is permanent and the
// provider should not retry the delivery.
func isTerminalInboundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, prefix := range []string{
		"message too large",
		"body-mime missing",
		"invalid multipart boundary",
		"unsupported content type",
		"parse MIME:",
		"multipart without boundary",
		"too many multipart parts",
		"too many form fields",
		"duplicate field",
		"multiple body-mime parts",
		"form field too large",
		"delivery id too long",
		"empty message",
		"too many mime parts",
		"mime nesting too deep",
	} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}
