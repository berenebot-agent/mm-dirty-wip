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
	"gatehouse-mail/internal/htmlsanitize"
	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/transport"
)

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"name": "Gatehouse Mail", "api_version": "v1", "api_base": "/v1", "agent_guide": "/agent", "openapi": "/openapi.json", "bootstrap": "/v1/bootstrap", "capabilities": []string{"inboxes", "messages", "threads", "search", "labels", "attachments", "events", "drafts", "draft-approval", "outbox", "send", "hermes-relay"}})
}
func (s *Server) agentGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	guide := "# Gatehouse Mail\n\n" +
		"Authenticate with `Authorization: Bearer <key>`.\n\n" +
		"Start with `GET /v1/bootstrap` to discover accessible inboxes and mailbox roles.\n\n" +
		"## Inboxes\n" +
		"- `GET /v1/inboxes` — list inboxes you can access\n" +
		"- `GET /v1/inboxes/{id}` — inbox detail\n" +
		"- `POST /v1/inboxes` (Admin) — create an inbox\n" +
		"- `PATCH /v1/inboxes/{id}` (Owner) — set display name, allowed senders, approver, aliases and `default_sender`\n\n" +
		"## Messages\n" +
		"- `GET /v1/messages?inbox={id}&label=...&from=...&to=...&unread=true&has_attachment=true&before={id}` — list messages (Spam excluded; `spam=true` lists only Spam, `include_spam=true` includes it)\n" +
		"- `GET /v1/messages/{id}` — message detail\n" +
		"- `PATCH /v1/messages/{id}` — set `read`/`archived`/`labels`/`spam`\n" +
		"- `DELETE /v1/messages/{id}` (Assistant/Owner)\n" +
		"- `GET /v1/messages/{id}/attachments` — attachment metadata\n" +
		"- `GET /v1/attachments/{id}` — download attachment bytes\n\n" +
		"## Threads\n" +
		"- `GET /v1/threads?inbox={id}`\n" +
		"- `GET /v1/threads/{id}` and `GET /v1/threads/{id}/messages`\n\n" +
		"## Search\n" +
		"- `GET /v1/search?q=...&inbox={id}&label=...&from=...&to=...&has_attachment=true` — FTS5 search\n\n" +
		"## Labels\n" +
		"- Free-text tags shared across the account; a message may have many. Matching ignores case and surrounding whitespace.\n" +
		"- `GET /v1/labels` — distinct labels currently in use\n" +
		"- `PATCH /v1/messages/{id}` with `{\"labels\":[\"Invoices\",\"Unpaid\"]}` — replace the label set (`[]` clears; omit the field to leave it unchanged)\n" +
		"- `GET /v1/messages?label=Invoices&label=Unpaid` — messages carrying all listed labels\n\n" +
		"## Events (realtime)\n" +
		"- `GET /v1/events?after=evt_...` — incremental history\n" +
		"- `GET /v1/events/wait?after=evt_...&timeout=60` — long poll\n" +
		"- `GET /v1/events/stream?after=evt_...` — SSE\n\n" +
		"## Drafts\n" +
		"- `GET/POST /v1/drafts`, `GET/PATCH/DELETE /v1/drafts/{id}` (Assistant/Owner)\n" +
		"- `GET/POST /v1/drafts/{id}/attachments`, `DELETE /v1/drafts/{id}/attachments/{attId}` (Assistant/Owner; upload is multipart with field `attachments`)\n" +
		"- `POST /v1/drafts/{id}/send` (Owner) — send a draft immediately (copies fields + attachments, deletes the draft)\n\n" +
		"## Draft approval (human-in-the-loop)\n" +
		"An Assistant can draft and request send; an Owner authorizes. Approvals always apply to the exact frozen draft.\n" +
		"- `POST /v1/drafts/{id}/request-send` (Assistant) — submit for authorization; the draft becomes `pending_approval` and is frozen. If the inbox has an `approver_email` configured, the approval-request email is sent automatically; `{\"external\": true}` is optional and only errors when no approver is configured.\n" +
		"- `POST /v1/drafts/{id}/cancel-send-request` (Assistant) — withdraw the request and return the draft to `draft`.\n" +
		"- `POST /v1/drafts/{id}/approve` (Owner) — approve and enqueue the frozen draft through the normal outbound flow. Optional `{\"feedback\":\"...\"}`.\n" +
		"- `POST /v1/drafts/{id}/reject` (Owner) — reject with optional `{\"feedback\":\"...\"}`; the draft becomes `rejected`, stays editable, and can be resubmitted.\n" +
		"- `GET /v1/drafts/{id}/send-request` — the latest request (works after the draft has been sent); `GET /v1/send-requests?inbox={id}&active=true` lists requests.\n" +
		"- Draft reads include `status` (`draft`, `pending_approval`, `rejected`) and the latest `send_request`, including `approver_email`, `token_expires_at` and `decision_method` (`ui`, `api` or `email`).\n" +
		"- External approval: an inbox may configure an `approver_email` (set via `PATCH /v1/inboxes/{id}`). The approver gets an email with Approve/Reject `mailto:` actions and replies to the inbox; Gatehouse consumes the reply, validates the token and sender, and records the decision. A UI decision wins safely over an outstanding email request.\n" +
		"- External requests expire after `APPROVAL_EXPIRY_HOURS` (default 48, `0` disables); an expired request returns the draft to `draft` and the token is permanently dead.\n" +
		"- Events: `draft.send_requested`, `draft.send_request_cancelled`, `draft.approved`, `draft.rejected`, `draft.sent`, `draft.send_failed`, `draft.approval_expired`.\n" +
		"- Approval is asynchronous: it enqueues a pending message; watch `draft.sent` or `draft.send_failed` for the delivery outcome. Approval and delivery are separate states.\n\n" +
		"## Send and reply (Owner)\n" +
		"- `POST /v1/send` with `{\"inbox_id\":\"...\",\"to\":[\"a@b.c\"],\"subject\":\"...\",\"text\":\"...\"}` — enqueues into the outbox and returns immediately (`queued:true`). Add `?wait=true` to block until delivery. Add `\"sender\":\"sales@example.com\"` to send as one of the inbox's aliases (provider resolved from that alias's domain).\n" +
		"- `POST /v1/messages/{id}/reply` with `{\"text\":\"...\"}`\n" +
		"- Send and reply accept optional attachments as base64 JSON: `[{\"filename\":\"file.pdf\",\"content_type\":\"application/pdf\",\"content\":\"<base64>\"}]`\n" +
		"- Use an `Idempotency-Key` header to make sends retry-safe.\n\n" +
		"## Outbox (Owner)\n" +
		"- `GET /v1/outbox?inbox={id}` — list pending and failed outbound messages\n" +
		"- `POST /v1/outbox/{id}/retry` — re-queue a failed message\n" +
		"- `DELETE /v1/outbox/{id}` — cancel a pending send or discard a failed one\n\n" +
		"## Admin (Admin role)\n" +
		"- `GET/POST /v1/admin/domains`, `PATCH/DELETE /v1/admin/domains/{id}`\n" +
		"- `GET/POST /v1/admin/keys`, `DELETE /v1/admin/keys/{id}`\n" +
		"- `GET/PUT/DELETE /v1/admin/domains/{id}/sending` — sending provider config\n" +
		"- `GET/PUT/DELETE /v1/admin/domains/{id}/receiving` — receiving provider config\n" +
		"- `GET /v1/admin/domains/{id}/sending/deliveries` — delivery activity for a domain\n" +
		"- `GET /v1/admin/hermes`, `DELETE /v1/admin/hermes/{id}`\n\n" +
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
	writeJSON(w, 200, map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "Gatehouse Mail", "version": "v1"},
		"servers": []map[string]string{{"url": s.Service.Config.BaseURL}},
		"paths": map[string]any{
			"/v1/bootstrap": map[string]any{"get": map[string]any{"summary": "Discover key capabilities and accessible inboxes", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/inboxes": map[string]any{
				"get":  map[string]any{"summary": "List inboxes", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Create an inbox (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/inboxes/{id}": map[string]any{
				"get":    map[string]any{"summary": "Get an inbox", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"patch":  map[string]any{"summary": "Update an inbox (display_name, enabled, allowed_senders, sender_restricted, approver_email, aliases, default_sender)", "description": "aliases replaces the inbox's alias set; each entry is a full local@domain address on any domain the account owns. Aliases route inbound mail to this inbox and may be chosen as the From address when sending. default_sender preselects the compose/reply From address (the inbox primary or one of its aliases); empty clears it to the primary.", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Delete an inbox (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/identities": map[string]any{
				"get":  map[string]any{"summary": "List identities (openagent.email compat)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Create an identity (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/messages": map[string]any{"get": map[string]any{"summary": "List messages with filters (inbox, thread, label, from, to, unread, has_attachment, before)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/messages/wait": map[string]any{
				"get":  map[string]any{"summary": "Long-poll for a new message", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Long-poll for a new message (compat)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/messages/{id}": map[string]any{
				"get":    map[string]any{"summary": "Get a message", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"patch":  map[string]any{"summary": "Update read/archived/labels state", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Delete a message (Assistant/Owner)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/messages/{id}/seen":        map[string]any{"post": map[string]any{"summary": "Mark a message seen (compat)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/messages/{id}/attachments": map[string]any{"get": map[string]any{"summary": "List message attachments", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/messages/{id}/reply":       map[string]any{"post": map[string]any{"summary": "Reply to a message (Owner); optional sender chooses the From identity", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/attachments/{id}":          map[string]any{"get": map[string]any{"summary": "Download an attachment", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/threads":                   map[string]any{"get": map[string]any{"summary": "List threads", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/threads/{id}":              map[string]any{"get": map[string]any{"summary": "Get a thread", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/threads/{id}/messages":     map[string]any{"get": map[string]any{"summary": "List messages in a thread", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/search":                    map[string]any{"get": map[string]any{"summary": "Search messages (FTS5)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/labels":                    map[string]any{"get": map[string]any{"summary": "List distinct labels in use", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/events":                    map[string]any{"get": map[string]any{"summary": "Incremental event history", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/events/wait":               map[string]any{"get": map[string]any{"summary": "Long-poll for events", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/events/stream":             map[string]any{"get": map[string]any{"summary": "SSE event stream", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/send":                      map[string]any{"post": map[string]any{"summary": "Send email as an Owner", "description": "Enqueues into the outbox and returns immediately. Add ?wait=true to block until delivery. Accepts JSON attachments with filename, content_type, and base64-encoded content fields. Optional sender chooses a From identity (the inbox primary or one of its aliases); the sending provider is resolved from that address's domain.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts": map[string]any{
				"get":  map[string]any{"summary": "List drafts", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Create a draft", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/drafts/{id}": map[string]any{
				"get":    map[string]any{"summary": "Get a draft", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"patch":  map[string]any{"summary": "Update a draft", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Delete a draft", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/drafts/{id}/send":                map[string]any{"post": map[string]any{"summary": "Send a draft (Owner)", "description": "Copies the draft's fields and attachments into a new outbound message, then deletes the draft.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/request-send":        map[string]any{"post": map[string]any{"summary": "Request authorization to send a draft (Assistant)", "description": "Freezes the draft as pending_approval until the request is approved, rejected, cancelled or expired. A configured inbox approver makes the request external automatically (the approval email is sent to them); {\"external\": true} is optional and only errors when no approver is configured.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/cancel-send-request": map[string]any{"post": map[string]any{"summary": "Cancel a pending send request (Assistant)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/approve":             map[string]any{"post": map[string]any{"summary": "Approve and send a pending draft (Owner)", "description": "Approves the exact frozen draft and enqueues it through the outbound flow. Accepts an optional feedback field.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/reject":              map[string]any{"post": map[string]any{"summary": "Reject a pending send request (Owner)", "description": "Marks the draft rejected and stores optional feedback for the agent.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/send-request":        map[string]any{"get": map[string]any{"summary": "Get the latest send request for a draft", "description": "Returns the workflow record even after the draft has been consumed by an approved send.", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/drafts/{id}/attachments": map[string]any{
				"get":  map[string]any{"summary": "List draft attachments", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Upload draft attachments (multipart field 'attachments')", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/drafts/{id}/attachments/{attId}": map[string]any{"delete": map[string]any{"summary": "Delete a draft attachment", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/send-requests":                   map[string]any{"get": map[string]any{"summary": "List draft send requests (filter by inbox and active)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/outbox":                          map[string]any{"get": map[string]any{"summary": "List pending and failed outbound messages", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/outbox/{id}/retry":               map[string]any{"post": map[string]any{"summary": "Re-queue a failed outbound message", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/outbox/{id}":                     map[string]any{"delete": map[string]any{"summary": "Cancel a pending send or discard a failed one", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/admin/domains": map[string]any{
				"get":  map[string]any{"summary": "List domains (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Create a domain (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/admin/domains/{id}": map[string]any{
				"patch":  map[string]any{"summary": "Update a domain (Admin)", "description": "Accepts catch_all_inbox_id only.", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Delete a domain (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/admin/domains/{id}/sending": map[string]any{
				"get":    map[string]any{"summary": "Get a domain's sending provider config (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"put":    map[string]any{"summary": "Set a domain's sending provider config (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Clear a domain's sending provider config (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/admin/domains/{id}/receiving": map[string]any{
				"get":    map[string]any{"summary": "Get a domain's receiving provider config (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"put":    map[string]any{"summary": "Set a domain's receiving provider config (Admin)", "description": "Accepts provider, config, and regenerate_secret. Generated secrets are returned once in the response.", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"delete": map[string]any{"summary": "Clear a domain's receiving provider config (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/admin/domains/{id}/sending/deliveries": map[string]any{"get": map[string]any{"summary": "List delivery attempts for a domain (Admin)", "description": "Returns the per-attempt delivery log for a domain, newest first, with message_id linking to the message. Supports limit and before (keyset on attempt id).", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/admin/keys": map[string]any{
				"get":  map[string]any{"summary": "List API keys (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
				"post": map[string]any{"summary": "Create an API key (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}},
			},
			"/v1/admin/keys/{id}":   map[string]any{"delete": map[string]any{"summary": "Revoke an API key (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/admin/hermes":      map[string]any{"get": map[string]any{"summary": "List Hermes connections (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
			"/v1/admin/hermes/{id}": map[string]any{"delete": map[string]any{"summary": "Delete a Hermes connection (Admin)", "security": []map[string]any{{"bearerAuth": []string{}}}}},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
		},
	})
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
		if !p.CanOwn(id) && !p.Admin {
			writeError(w, 403, "forbidden")
			return
		}
		var in struct {
			DisplayName      *string   `json:"display_name"`
			Enabled          *bool     `json:"enabled"`
			AllowedSenders   *[]string `json:"allowed_senders"`
			SenderRestricted *bool     `json:"sender_restricted"`
			ApproverEmail    *string   `json:"approver_email"`
			Aliases          *[]string `json:"aliases"`
			DefaultSender    *string   `json:"default_sender"`
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
		if in.Aliases != nil {
			forms, err := normalizeAliasForms(*in.Aliases)
			if err != nil {
				writeError(w, 400, err.Error())
				return
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
	f := store.MessageFilter{InboxID: r.URL.Query().Get("inbox"), ThreadID: r.URL.Query().Get("thread"), From: r.URL.Query().Get("from"), To: r.URL.Query().Get("to"), Unread: boolQuery(r, "unread"), HasAttachment: boolQuery(r, "has_attachment"), Labels: r.URL.Query()["label"], Limit: intParam(r, "limit", 100)}
	if b := boolQuery(r, "spam"); b != nil && *b {
		f.SpamOnly = true
	} else if b := boolQuery(r, "include_spam"); b != nil && *b {
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
			Read     *bool
			Archived *bool
			Labels   *[]string
			Spam     *bool
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.Read != nil || in.Archived != nil {
			if err := s.Service.Store.UpdateMessageState(r.Context(), p, id, in.Read, in.Archived); err != nil {
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
		path, _, ev, err := s.Service.Store.DeleteMessage(r.Context(), p, id)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		if path != "" {
			_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
		}
		s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
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
	items, err := s.Service.Store.SearchMessagesFiltered(r.Context(), principal(r), r.URL.Query().Get("q"), store.MessageFilter{
		InboxID:       r.URL.Query().Get("inbox"),
		From:          r.URL.Query().Get("from"),
		To:            r.URL.Query().Get("to"),
		Before:        r.URL.Query().Get("before"),
		Labels:        r.URL.Query()["label"],
		HasAttachment: boolQuery(r, "has_attachment"),
		Limit:         intParam(r, "limit", 100),
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
	writeJSON(w, 200, map[string]any{"queued": true, "messageId": res.Message.RFCMessageID, "provider_message_id": res.ProviderMessageID, "message": res.Message})
}
func (s *Server) apiReply(w http.ResponseWriter, r *http.Request) {
	m, err := s.Service.Store.GetMessage(r.Context(), principal(r), r.PathValue("id"))
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
	res, err := s.Service.Send(r.Context(), principal(r), app.SendInput{InboxID: m.InboxID, FromAddress: in.Sender, ReplyToMessageID: m.ID, Text: in.Text, HTML: in.HTML, Attachments: in.Attachments}, idemKey(r))
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
		paths, err := s.Service.Store.DeleteDraftCascade(r.Context(), p, id)
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

// apiDraftSend sends a draft: it copies the draft's fields and attachments into
// a new pending outbound message, then deletes the draft. Requires owner.
func (s *Server) apiDraftSend(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	id := r.PathValue("id")
	d, err := s.Service.Store.GetDraft(r.Context(), p, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if !p.CanOwn(d.InboxID) {
		mapStoreError(w, store.ErrForbidden)
		return
	}
	res, err := s.Service.SendDraft(r.Context(), p, id, app.SendInput{InboxID: d.InboxID, FromAddress: d.FromAddress, ReplyToMessageID: d.ReplyToMessageID, To: d.To, CC: d.CC, BCC: d.BCC, Subject: d.Subject, Text: d.Text, HTML: d.HTML}, idemKey(r))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"queued": true, "messageId": res.Message.RFCMessageID, "message": res.Message})
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
	var in struct {
		External bool `json:"external"`
	}
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &in) {
			return
		}
	}
	d, err := s.Service.RequestSend(r.Context(), p, r.PathValue("id"), in.External)
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
	v, err := s.Service.Store.ListSendRequests(r.Context(), p, r.URL.Query().Get("inbox"), activeOnly, intParam(r, "limit", 100))
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
		out := make([]model.DraftAttachment, 0, len(atts))
		for _, a := range atts {
			rawPath := s.draftAttachmentPath()
			if err = os.MkdirAll(filepath.Dir(rawPath), 0o700); err != nil {
				writeError(w, 500, err.Error())
				return
			}
			if err = os.WriteFile(rawPath, a.Content, 0o600); err != nil {
				writeError(w, 500, err.Error())
				return
			}
			rel, _ := filepath.Rel(s.Service.Config.DataDir, rawPath)
			rec, aerr := s.Service.Store.AddDraftAttachment(r.Context(), p, id, model.DraftAttachment{Filename: a.Filename, ContentType: a.ContentType, Size: int64(len(a.Content)), RawPath: filepath.ToSlash(rel)})
			if aerr != nil {
				_ = os.Remove(rawPath)
				mapStoreError(w, aerr)
				return
			}
			out = append(out, rec)
		}
		writeJSON(w, 201, out)
	}
}

// apiDraftAttachment deletes a single draft attachment.
func (s *Server) apiDraftAttachment(w http.ResponseWriter, r *http.Request) {
	raw, err := s.Service.Store.DeleteDraftAttachment(r.Context(), principal(r), r.PathValue("id"), r.PathValue("attId"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if raw != "" {
		_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(raw)))
	}
	w.WriteHeader(204)
}

func (s *Server) apiOutbox(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	v, err := s.Service.Store.ListOutbox(r.Context(), p, r.URL.Query().Get("inbox"), intParam(r, "limit", 100))
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
	path, _, ev, err := s.Service.Store.DeleteOutboxMessage(r.Context(), p, r.PathValue("id"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if path != "" {
		_ = os.Remove(filepath.Join(s.Service.Config.DataDir, filepath.FromSlash(path)))
	}
	s.Log.Info("event published", "type", ev.Type, "cursor", ev.Cursor, "entity_id", ev.EntityID, "inbox_id", ev.InboxID)
	s.Service.Hub.Publish(ev)
	w.WriteHeader(204)
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
	// Revoking, rotating, or rescoping the credential cancels this stream.
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
	q := r.URL.Query()
	after := store.ParseCursor(q.Get("after"))
	inboxID := q.Get("inbox")
	timeoutSec := intParam(r, "timeout", 60)
	fromContains := ""
	subjectContains := ""
	compat := false
	includeSpam := false
	if b := boolQuery(r, "include_spam"); b != nil {
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
		if in.IncludeSpam != nil {
			includeSpam = *in.IncludeSpam
		}
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
			Name string `json:"name"`
		}
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
		paths, err := s.Service.Store.PurgeDomain(r.Context(), p.AccountID, id)
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
	s.Service.Hub.CancelScope("hrm:" + r.PathValue("id"))
	w.WriteHeader(204)
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
	if provider == "mx" {
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
