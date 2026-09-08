package httpapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"name": "Gatehouse Email", "api_version": "v1", "api_base": "/v1", "agent_guide": "/agent", "openapi": "/openapi.json", "bootstrap": "/v1/bootstrap", "capabilities": []string{"inboxes", "messages", "threads", "search", "attachments", "events", "drafts", "send", "hermes-relay"}})
}
func (s *Server) agentGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	guide := "# Gatehouse Email\n\n" +
		"Authenticate with `Authorization: Bearer <key>`.\n\n" +
		"Start with `GET /v1/bootstrap` to discover accessible inboxes and mailbox roles.\n\n" +
		"Core operations:\n" +
		"- `GET /v1/messages`\n" +
		"- `GET /v1/threads`\n" +
		"- `GET /v1/search?q=...`\n" +
		"- `GET /v1/events/stream?after=evt_...`\n" +
		"- `GET/POST /v1/drafts`\n" +
		"- `POST /v1/send` (Owner)\n" +
		"- `POST /v1/messages/{id}/reply` (Owner)\n\n" +
		"Send and reply accept optional attachments as base64 JSON: [{\"filename\":\"file.pdf\",\"content_type\":\"application/pdf\",\"content\":\"...\"}].\n\n" +
		"Roles are assigned per mailbox: Read, Assistant, Owner. Admin is account-wide.\n"
	fmt.Fprint(w, guide)
}
func (s *Server) pythonExample(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "import requests\nBASE=%q\nKEY='ghm_...'\nh={'Authorization':f'Bearer {KEY}'}\nprint(requests.get(BASE+'/v1/messages',headers=h).json())\n", s.Service.Config.BaseURL)
}
func (s *Server) curlExample(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "curl -H 'Authorization: Bearer ghm_...' %s/v1/bootstrap\n", s.Service.Config.BaseURL)
}
func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"openapi": "3.0.3", "info": map[string]any{"title": "Gatehouse Email", "version": "v1"}, "servers": []map[string]string{{"url": s.Service.Config.BaseURL}}, "paths": map[string]any{"/v1/bootstrap": map[string]any{"get": map[string]any{"summary": "Discover key capabilities"}}, "/v1/inboxes": map[string]any{"get": map[string]any{"summary": "List inboxes"}}, "/v1/messages": map[string]any{"get": map[string]any{"summary": "List messages"}}, "/v1/search": map[string]any{"get": map[string]any{"summary": "Search messages"}}, "/v1/events/stream": map[string]any{"get": map[string]any{"summary": "Replay and stream events"}}, "/v1/send": map[string]any{"post": map[string]any{"summary": "Send email as an Owner", "description": "Accepts JSON attachments with filename, content_type, and base64-encoded content fields."}}}})
}

func (s *Server) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	boxes, err := s.Service.Store.ListInboxes(r.Context(), p)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"account_id": p.AccountID, "admin": p.Admin, "mailbox_roles": p.MailboxRoles, "inboxes": boxes, "events": map[string]string{"list": "/v1/events", "wait": "/v1/events/wait", "stream": "/v1/events/stream"}})
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
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		box, err := s.Service.Store.CreateInbox(r.Context(), p.AccountID, in.DomainID, in.LocalPart, in.DisplayName)
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
		var in struct {
			DisplayName          string    `json:"display_name"`
			Enabled              *bool     `json:"enabled"`
			OutboundCredentialID *string   `json:"outbound_credential_id"`
			AllowedSenders       *[]string `json:"allowed_senders"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if err := s.Service.Store.UpdateInbox(r.Context(), p, id, in.DisplayName, in.Enabled, in.OutboundCredentialID); err != nil {
			mapStoreError(w, err)
			return
		}
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
		}
		v, _ := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id)
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
			_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
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
				_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
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
	return map[string]any{"id": m.ID, "from": m.From.Address, "to": firstString(m.To), "subject": m.Subject, "date": when, "seen": m.Read, "snippet": snippet, "hasOtp": false, "source": "external", "text": m.Text, "html": m.HTML, "messageId": m.RFCMessageID, "threadId": m.ThreadID, "hasAttachments": m.HasAttachments}
}
func firstString(v []string) string {
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	f := store.MessageFilter{InboxID: r.URL.Query().Get("inbox"), ThreadID: r.URL.Query().Get("thread"), From: r.URL.Query().Get("from"), Unread: boolQuery(r, "unread"), HasAttachment: boolQuery(r, "has_attachment"), Limit: intParam(r, "limit", 100)}
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
		var in struct{ Read, Archived *bool }
		if !decodeJSON(w, r, &in) {
			return
		}
		if err := s.Service.Store.UpdateMessageState(r.Context(), p, id, in.Read, in.Archived); err != nil {
			mapStoreError(w, err)
			return
		}
		m, _ := s.Service.Store.GetMessage(r.Context(), p, id)
		writeJSON(w, 200, m)
	case http.MethodDelete:
		path, _, ev, err := s.Service.Store.DeleteMessage(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if path != "" {
			_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
		}
		s.Service.Hub.Publish(ev)
		w.WriteHeader(204)
	}
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
	if err = s.Service.Store.UpdateMessageState(r.Context(), p, m.ID, &seen, nil); err != nil {
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
	path := filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(m.RawPath))
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	w.Header().Set("Content-Disposition", disp)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := mailparse.ExtractAttachment(path, a.PartIndex, w); err != nil {
		s.Log.Error("attachment extraction", "error", err)
	}
}

func (s *Server) apiThreads(w http.ResponseWriter, r *http.Request) {
	items, err := s.Service.Store.ListThreads(r.Context(), principal(r), r.URL.Query().Get("inbox"), intParam(r, "limit", 100))
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
	items, err := s.Service.Store.SearchMessages(r.Context(), principal(r), r.URL.Query().Get("q"), r.URL.Query().Get("inbox"), intParam(r, "limit", 100))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, items)
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
		InboxID     string               `json:"inbox_id"`
		From        string               `json:"from"`
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
	res, err := s.Service.Send(r.Context(), p, app.SendInput{InboxID: in.InboxID, To: []string(in.To), CC: []string(in.CC), BCC: []string(in.BCC), Subject: in.Subject, Text: in.Text, HTML: in.HTML, Attachments: in.Attachments}, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"queued": true, "messageId": res.Message.RFCMessageID, "provider_message_id": res.ProviderMessageID, "message": res.Message})
}
func (s *Server) apiReply(w http.ResponseWriter, r *http.Request) {
	m, err := s.Service.Store.GetMessage(r.Context(), principal(r), r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	var in struct {
		Text        string               `json:"text"`
		HTML        string               `json:"html,omitempty"`
		Attachments []app.SendAttachment `json:"attachments,omitempty"`
	}
	if !decodeJSONLimit(w, r, &in, s.Service.Config.MaxMessageBytes*2) {
		return
	}
	res, err := s.Service.Send(r.Context(), principal(r), app.SendInput{InboxID: m.InboxID, ReplyToMessageID: m.ID, Text: in.Text, HTML: in.HTML, Attachments: in.Attachments}, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 201, res)
}

func (s *Server) apiDrafts(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.ListDrafts(r.Context(), p, r.URL.Query().Get("inbox"))
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodPost:
		var d model.Draft
		if !decodeJSON(w, r, &d) {
			return
		}
		v, err := s.Service.Store.CreateDraft(r.Context(), p, d)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, v)
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
		var d model.Draft
		if !decodeJSON(w, r, &d) {
			return
		}
		d.ID = id
		v, err := s.Service.Store.UpdateDraft(r.Context(), p, d)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 200, v)
	case http.MethodDelete:
		if err := s.Service.Store.DeleteDraft(r.Context(), p, id); err != nil {
			mapStoreError(w, err)
			return
		}
		w.WriteHeader(204)
	}
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	after := store.ParseCursor(r.URL.Query().Get("after"))
	v, err := s.Service.Store.ListEvents(r.Context(), principal(r), after, r.URL.Query().Get("inbox"), intParam(r, "limit", 100))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) waitEvents(r *http.Request, timeout time.Duration) ([]model.Event, error) {
	p := principal(r)
	after := store.ParseCursor(r.URL.Query().Get("after"))
	inbox := r.URL.Query().Get("inbox")
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
	sec := intParam(r, "timeout", 60)
	if sec < 1 {
		sec = 1
	}
	if sec > 60 {
		sec = 60
	}
	v, err := s.waitEvents(r, time.Duration(sec)*time.Second)
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
	after := store.ParseCursor(r.URL.Query().Get("after"))
	inbox := r.URL.Query().Get("inbox")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	_, ch, cancel := s.Service.Hub.Subscribe(32)
	defer cancel()
	bw := bufio.NewWriter(w)
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
		items, err := s.Service.Store.ListEvents(r.Context(), p, after, inbox, 500)
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
		case <-r.Context().Done():
			return
		}
	}
}
func (s *Server) apiMessagesWait(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	q := r.URL.Query()
	after := store.ParseCursor(q.Get("after"))
	inboxID := q.Get("inbox")
	timeoutSec := intParam(r, "timeout", 60)
	fromContains := ""
	subjectContains := ""
	compat := false
	if r.Method == http.MethodPost {
		var in struct {
			After           string `json:"after"`
			Inbox           string `json:"inbox"`
			Timeout         int    `json:"timeout"`
			Address         string `json:"address"`
			FromContains    string `json:"fromContains"`
			SubjectContains string `json:"subjectContains"`
			TimeoutSec      int    `json:"timeoutSec"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
		if in.After != "" {
			after = store.ParseCursor(in.After)
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
		if in.TimeoutSec > 0 {
			timeoutSec = in.TimeoutSec
			compat = true
		}
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
		var in struct{ Name string }
		if !decodeJSON(w, r, &in) {
			return
		}
		v, err := s.Service.Store.CreateDomain(r.Context(), p.AccountID, in.Name)
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
			CatchAllInboxID *string `json:"catch_all_inbox_id"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.CatchAllInboxID != nil {
			if err := s.Service.Store.SetDomainCatchAll(r.Context(), p.AccountID, id, *in.CatchAllInboxID); err != nil {
				mapStoreError(w, err)
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"updated": true})
	case http.MethodDelete:
		if err := s.Service.Store.DeleteDomain(r.Context(), p.AccountID, id); err != nil {
			mapStoreError(w, err)
			return
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
	w.WriteHeader(204)
}

func (s *Server) apiOutbound(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		v, err := s.Service.Store.ListOutboundCredentials(r.Context(), p.AccountID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(v))
		for _, c := range v {
			out = append(out, map[string]any{"id": c.ID, "name": c.Name, "provider": c.Provider, "created_at": c.CreatedAt, "updated_at": c.UpdatedAt})
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			ID, Name, Provider string
			Config             map[string]any `json:"config"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		v, err := s.Service.SaveOutboundCredential(r.Context(), p.AccountID, in.ID, in.Name, in.Provider, in.Config)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"id": v.ID, "name": v.Name, "provider": v.Provider})
	}
}
func (s *Server) apiOutboundDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	if err := s.Service.Store.DeleteOutboundCredential(r.Context(), p.AccountID, r.PathValue("id")); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(204)
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
		ID, InboxID, Name, GatewayID string
		LastAckEventID               int64
		CreatedAt                    time.Time
		LastConnectedAt              *time.Time
	}
	out := []item{}
	for _, h := range v {
		out = append(out, item{h.ID, h.InboxID, h.Name, h.GatewayID, h.LastAckEventID, h.CreatedAt, h.LastConnectedAt})
	}
	writeJSON(w, 200, out)
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
	w.WriteHeader(204)
}

func (s *Server) mailgunIngest(w http.ResponseWriter, r *http.Request) {
	// Compat: the legacy Mailgun route predates the generic inbound path.
	s.ingestProvider(w, r, "mailgun")
}

func (s *Server) ingestInbound(w http.ResponseWriter, r *http.Request) {
	s.ingestProvider(w, r, r.PathValue("provider"))
}

func (s *Server) ingestProvider(w http.ResponseWriter, r *http.Request, provider string) {
	m, dup, err := s.Service.IngestInbound(r.Context(), provider, r)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "recipient rejected", http.StatusNotAcceptable)
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
		s.Log.Warn("inbound ingest failed", "provider", provider, "error", err)
		http.Error(w, "ingest failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, 200, map[string]any{"accepted": true, "duplicate": dup, "message_id": m.ID})
}
