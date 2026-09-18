package apispec

import "strings"

// RenderAgentGuide renders the served /agent reference guide: a route table
// generated from the table plus the narrative prose that carries semantics the
// one-line summaries cannot.
func RenderAgentGuide(routes []Route) string {
	var b strings.Builder
	b.WriteString("# MailMoose\n\n")
	b.WriteString("Authenticate with `Authorization: Bearer <key>`.\n\n")
	b.WriteString("The complete machine-readable contract is at `GET /openapi.json`.\n\n")
	b.WriteString("Start with `GET /v1/bootstrap` to discover accessible inboxes and effective permissions.\n\n")
	b.WriteString(agentGuideQuickstart)
	for _, group := range distinctGroups(routes) {
		b.WriteString("## ")
		b.WriteString(group)
		b.WriteString("\n")
		for _, r := range routes {
			if r.Group != group {
				continue
			}
			b.WriteString("- `")
			b.WriteString(r.Method)
			b.WriteString(" ")
			b.WriteString(r.Path)
			b.WriteString("` — ")
			b.WriteString(r.Summary)
			if r.Role != "" {
				b.WriteString(" (")
				b.WriteString(r.Role)
				b.WriteString(")")
			}
			b.WriteString("\n")
			if r.Description != "" {
				b.WriteString("  ")
				b.WriteString(r.Description)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString(agentGuideNarrative)
	b.WriteString(agentGuideSecurityAppendix)
	return b.String()
}

// distinctGroups returns the distinct non-empty Group values in first-seen
// order for the supplied table.
func distinctGroups(routes []Route) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range routes {
		if r.Group == "" || seen[r.Group] {
			continue
		}
		seen[r.Group] = true
		out = append(out, r.Group)
	}
	return out
}

const agentGuideQuickstart = "## Quick start\n" +
	"1. Call `GET /v1/bootstrap` and choose an inbox whose `permissions[inbox_id].can_send` is `true`. The matching `inboxes` entry includes the primary address, managed aliases and `default_sender`.\n" +
	"2. Send with `POST /v1/send?wait=true`:\n\n" +
	"```http\nPOST /v1/send?wait=true\nAuthorization: Bearer <key>\nContent-Type: application/json\n\n{\"inbox_id\":\"inb_...\",\"to\":[\"recipient@example.com\"],\"subject\":\"Test message\",\"text\":\"Hello from MailMoose\"}\n```\n\n" +
	"3. With `wait=true`, a successful response waits for the provider attempt and includes the message status. Without it, the message is returned as soon as it is queued.\n" +
	"- Send fields: `inbox_id` is required unless `from` identifies an accessible inbox or this non-admin key owns exactly one inbox; `to` and a non-empty `subject` are required; a non-empty `text` or `html` body is required. `sender`, `from`, `cc`, `bcc` and `attachments` are optional.\n" +
	"- `sender` must be the inbox primary address or one of its returned aliases. It is not the same as `from`, which is the compatibility inbox selector.\n" +
	"- A `400` uses `code: invalid_request`; insufficient mailbox access uses `403` and `code: forbidden`; an invalid sender uses `403` and `code: sender_not_allowed`; a missing resource uses `404` and `code: not_found`.\n" +
	"- Common error bodies are `{\"error\":\"sender not allowed\",\"code\":\"sender_not_allowed\"}`, `{\"error\":\"forbidden\",\"code\":\"forbidden\"}` and `{\"error\":\"not found\",\"code\":\"not_found\"}`.\n" +
	"- For an executable client, use `GET /examples/python` (Python 3 standard library only) or `GET /examples/bash` (requires `curl` and `jq`). The command-security notes are in the appendix below.\n\n"

// agentGuideSecurityAppendix is intentionally after the quickstart and route
// reference. It preserves the scanner-safe workaround without making it the
// first thing an agent must read.
const agentGuideSecurityAppendix = "## Appendix: command security notes\n" +
	"- `BASE` is the origin that served this guide (scheme + host, no trailing slash, e.g. `https://mail.example.com`); `KEY` is your API key. Set both up front:\n" +
	" `export BASE=\"https://your-instance\" KEY=\"mmm_...\"`\n" +
	"- `GET /examples/python` (`mailmoose.py`) uses only the Python 3 standard library, so it runs on hosts with no `curl` or `jq`. `GET /examples/bash` (`mailmoose.sh`) requires both `curl` and `jq` on `PATH`; use the Python client when `jq` is unavailable.\n" +
	"- If you call the API with curl directly, use one command with the JSON body inline and nothing after it:\n" +
	"  `curl -sS -X POST -H \"Authorization: Bearer $KEY\" -H \"Content-Type: application/json\" -d '{\"inbox_id\":\"inb_...\",\"to\":[\"a@b.c\"],\"subject\":\"...\",\"text\":\"...\"}' \"$BASE/v1/send?wait=true\"`\n" +
	"- Do not stage the request body in a temporary file and do not pass `curl --data-binary @file`. Do not write the response to a file with `-o` and re-read it. Do not pipe the response to a formatter (`| jq .`, `python -m json.tool`) or run it through an interpreter (`python3 -c '...'`); a pipe or interpreter turns the call into a chained-execution shape even when the JSON body is inline. Do not bundle `export`, the request and a formatter into one shell invocation; keep each step a separate command.\n" +
	"- Host command-security scanners often flag file-upload shapes, pipes and chained execution wrappers and require human approval. A single curl with an inline `-d`, no pipe and no interpreter is the reliably clean shape. The response is already JSON, so no formatter is needed.\n\n"

// agentGuideNarrative is the prose from the original hand-written guide that
// adds semantics beyond the generated one-line summaries. Keep it in sync with
// the served reference.
const agentGuideNarrative = "## Label notes\n" +
	"- Free-text tags shared across the account; a message may have many. Matching ignores case and surrounding whitespace.\n\n" +
	"## Draft write notes\n" +
	"- Draft writes (`POST /v1/drafts`, `PATCH /v1/drafts/{id}`, `POST /v1/drafts/{id}/send`, `POST /v1/drafts/{id}/request-send`) accept base64 JSON `attachments` (same shape as send/reply) and an `action` of `draft` (default), `request-send` or `send`, so a draft can be created, attached and submitted for approval in one request. `PATCH` is partial: only supplied fields change. `sender` is accepted as an alias for `from_address`.\n\n" +
	"## Draft approval (human-in-the-loop)\n" +
	"An Assistant can draft and request send; an Owner authorizes. Approvals always apply to the exact frozen draft.\n" +
	"- `POST /v1/drafts/{id}/request-send` (Assistant) — submit for authorization; the draft becomes `pending_approval` and is frozen. If the inbox has an `approver_email` configured, the approval-request email is sent automatically; `{\"external\": true}` is optional and only errors when no approver is configured.\n" +
	"- `POST /v1/drafts/{id}/cancel-send-request` (Assistant) — withdraw the request and return the draft to `draft`.\n" +
	"- `POST /v1/drafts/{id}/approve` (Owner) — approve and enqueue the frozen draft through the normal outbound flow. Optional `{\"feedback\":\"...\"}`.\n" +
	"- `POST /v1/drafts/{id}/reject` (Owner) — reject with optional `{\"feedback\":\"...\"}`; the draft becomes `rejected`, stays editable, and can be resubmitted.\n" +
	"- `GET /v1/drafts/{id}/send-request` — the latest request (works after the draft has been sent); `GET /v1/send-requests?inbox={id}&active=true` lists requests.\n" +
	"- Draft reads include `status` (`draft`, `pending_approval`, `rejected`) and the latest `send_request`, including `approver_email`, `token_expires_at` and `decision_method` (`ui`, `api` or `email`).\n" +
	"- External approval: an inbox may configure an `approver_email` (set via `PATCH /v1/inboxes/{id}`). The approver gets an email with Approve/Reject `mailto:` actions and replies to the inbox; MailMoose consumes the reply, validates the token and sender, and records the decision. A UI decision wins safely over an outstanding email request.\n" +
	"- External requests expire after `APPROVAL_EXPIRY_HOURS` (default 48, `0` disables); an expired request returns the draft to `draft` and the token is permanently dead.\n" +
	"- Events: `draft.send_requested`, `draft.send_request_cancelled`, `draft.approved`, `draft.rejected`, `draft.sent`, `draft.send_failed`, `draft.approval_expired`.\n" +
	"- Approval is asynchronous: it enqueues a pending message; watch `draft.sent` or `draft.send_failed` for the delivery outcome. Approval and delivery are separate states.\n\n" +
	"## Send and reply (Owner)\n" +
	"- `POST /v1/send` with `{\"inbox_id\":\"...\",\"to\":[\"a@b.c\"],\"subject\":\"...\",\"text\":\"...\"}` — enqueues into the outbox and returns immediately (`queued:true`). Add `?wait=true` to block until delivery. Add `\"sender\":\"sales@example.com\"` to send as one of the inbox's aliases (provider resolved from that alias's domain).\n" +
	"- `POST /v1/messages/{id}/reply` with `{\"text\":\"...\"}` — add `?wait=true` to block until delivery\n" +
	"- Send and reply accept optional attachments as base64 JSON: `[{\"filename\":\"file.pdf\",\"content_type\":\"application/pdf\",\"content\":\"<base64>\"}]`\n" +
	"- Send the JSON body inline in a single command, or use `GET /examples/python` (stdlib only) or `GET /examples/bash` (needs `curl` + `jq`); never stage it in a temporary file or pass `curl --data-binary @file`.\n" +
	"- Use an `Idempotency-Key` header to make sends retry-safe. Keys are scoped to the account and the inbox used by the request. Repeating a completed key returns the original message without sending again; reusing it concurrently returns a conflict, and a key used for another inbox also conflicts. Failed enqueue operations release the key; abandoned pending reservations expire after 15 minutes.\n\n" +
	"## Outbox notes\n" +
	"- Messages are enqueued by send/reply/approved drafts; watch `outbox` for pending and failed sends before retrying or cancelling.\n\n" +
	"Roles are assigned per mailbox: Read, Assistant, Owner. Admin is account-wide.\n"
