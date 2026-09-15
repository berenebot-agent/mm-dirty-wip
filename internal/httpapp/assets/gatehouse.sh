#!/usr/bin/env bash
#
# gatehouse.sh -- Gatehouse Mail agent client (Bash; curl + jq only).
#
# This is the canonical example client served at `GET /examples/curl` and is
# the Bash sibling of the Python example at `GET /examples/python`. It wraps the
# whole agent-facing REST surface of Gatehouse Mail and is meant to be read as
# much as run: every command maps one-to-one onto an HTTP route, and a comment
# above each handler names the route it calls.
#
# Quick start
# -----------
#
# # 1. Point the client at an instance and an API key.
# export GATEHOUSE_BASE_URL="https://your-instance"
# export GATEHOUSE_API_KEY="ghm_..."
#
#     # 2. Discover who you are and which inboxes you can reach.
#     ./gatehouse.sh bootstrap
#     ./gatehouse.sh inboxes --table
#
#     # 3. Read mail.
#     ./gatehouse.sh list --inbox inb_123 --unread --limit 20 --table
#     ./gatehouse.sh get msg_123
#     ./gatehouse.sh search invoice --inbox inb_123
#     ./gatehouse.sh threads --inbox inb_123
#     ./gatehouse.sh thread thr_123
#     ./gatehouse.sh labels
#
#     # 4. Send and reply.
#     ./gatehouse.sh send --to alice@example.com --subject "Hi" --text "Hello"
#     ./gatehouse.sh send --to a@x,b@y --subject "Report" --text "See attached" \
#         --attach report.pdf --wait
#     ./gatehouse.sh reply msg_123 --text "Thanks!"
#
#     # 5. Triage.
#     ./gatehouse.sh mark-read msg_123
#     ./gatehouse.sh label msg_123 --labels "Invoices,Unpaid"
#     ./gatehouse.sh delete msg_123
#
#     # 6. Drafts and human-in-the-loop approval.
#     ./gatehouse.sh drafts --inbox inb_123
#     ./gatehouse.sh drafts --inbox inb_123 --to alice@example.com \
#         --subject "Quote" --text "..." --action request-send
#     ./gatehouse.sh send-requests --active
#     ./gatehouse.sh approve drf_123 --feedback "Looks good"
#
#     # 7. Realtime and long-poll.
#     ./gatehouse.sh events --after evt_100
#     ./gatehouse.sh watch --after evt_100      # streams until Ctrl-C
#     ./gatehouse.sh wait --inbox inb_123 --timeout 120
#
# Scenario: bootstrap -> list -> send -> reply -> events
# -----------------------------------------------------
#
# BASE_URL=https://your-instance
# KEY=ghm_...
#
#     # Who am I and which inboxes can I reach?
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" bootstrap --table
#     INBOX=$(./gatehouse.sh --base "$BASE_URL" --key "$KEY" inboxes \
#         | jq -r '.[0].id')
#
#     # Read the newest unread message in that inbox.
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" list \
#         --inbox "$INBOX" --unread --limit 5 --table
#     MSG=$(./gatehouse.sh --base "$BASE_URL" --key "$KEY" list \
#         --inbox "$INBOX" --limit 1 | jq -r '.[0].id')
#
#     # Send, then reply to the message we just read.
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" send \
#         --to alice@example.org --subject "Hello" --text "Hi there"
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" reply "$MSG" \
#         --text "Thanks for the note"
#
#     # Follow durable events from the cursor returned by bootstrap/list.
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" events --inbox "$INBOX"
#     ./gatehouse.sh --base "$BASE_URL" --key "$KEY" watch --inbox "$INBOX"
#
# Credentials
# -----------
# Resolution order (first non-empty wins):
#
#   1. --base / --key command-line flags
#   2. GATEHOUSE_BASE_URL / GATEHOUSE_API_KEY environment variables
#   3. ~/.gatehouse/client.json (create it mode 0600):
#
#          {
# "base_url": "https://your-instance",
#            "api_key": "ghm_...",
#            "inbox": "inb_123"
#          }
#
# The client never reads ~/.hermes/.env or any other credential store.
#
# Output and exit codes
# ---------------------
# JSON goes to stdout by default so it can be piped into jq; diagnostics go to
# stderr. --table prints a simple columnar view (built with `jq -r`) instead.
# Exit codes: 0 success, 1 error (any non-2xx response prints the API's `error`
# field to stderr), 2 usage error.
#
# Realtime caveat
# ---------------
# `watch` opens the SSE endpoint with `curl -N` and STREAMS one JSON line per
# event until it is interrupted (Ctrl-C); it never returns on its own. `wait`
# calls the JSON long-poll endpoint and BLOCKS until a new message arrives or
# --timeout elapses (server-side cap: 600 seconds), then returns. Neither is
# suitable for a single non-blocking poll: use `events` (one page of history) or
# `list` for that. Both streaming commands hold a network connection open, so
# run them on a dedicated worker rather than in the middle of a request/response
# cycle.
#
# Run `gatehouse.sh <command> --help` for per-command usage.

set -euo pipefail

VERSION="1.0"
DEFAULT_BASE_URL="http://localhost:8081"
CONFIG_PATH="${GATEHOUSE_CONFIG:-${HOME:-}/.gatehouse/client.json}"

# Global state (set by the option parser).
BASE="${GATEHOUSE_BASE_URL:-}"
KEY="${GATEHOUSE_API_KEY:-}"
INBOX="${GATEHOUSE_INBOX:-}"
TABLE=0
COMMAND=""

# Command option state.
POSITIONAL=()
ATTACH_FILES=()
TO_PARTS=()
CC_PARTS=()
BCC_PARTS=()
LABEL_PARTS=()
LABELS_PARTS=()

AFTER=""
BEFORE=""
LIMIT=""
TIMEOUT="60"
Q=""
FROM_Q=""
FROM_ADDR=""
REPLY_TO=""
UNREAD=0
HAS_ATT=0
ACTIVE=0
CREATE=0
EXTERNAL=0
WAIT=0
NO_IDEM=0
IDEM_KEY=""
LIST_MODE=0
DELETE_DRAFT=0
MODE=""

SUBJECT=""
TEXT=""
HTML=""
SENDER=""
ACTION=""
FEEDBACK=""
SUBJECT_SET=0
TEXT_SET=0
HTML_SET=0
FEEDBACK_SET=0
LABELS_SET=0
CLEAR_LABELS=0

# Response plumbing.
RESP_TMP=""
RESP_STATUS=""
WORKDIR=""
QUERY=""

# ---------------------------------------------------------------------------
# Tables (jq -r filters). Each filter writes a header row then one TSV row per
# result, or a `key<TAB>value` view when the response is a single object.
# ---------------------------------------------------------------------------

TABLE_DEFAULT='if type=="array" then (.[] | tostring) else (to_entries[] | "\(.key)\t\(.value|tostring)") end'

TABLE_MESSAGES='def rows: if type=="array" then . elif ((.messages? // null)|type)=="array" then .messages else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","FROM","SUBJECT","READ","ATT","DATE","LABELS"]|@tsv),
(rows[] | [.id,
           (if ((.from//null)|type)=="object" then (.from.address//"") else ((.from//"")|tostring) end),
           (.subject//""),
           ((.read//false)|tostring),
           (if .has_attachments then "yes" else "" end),
           (.received_at//.created_at//.sent_at//""),
           ((.labels//[])|join(", "))] | map(tostring) | @tsv) end'

TABLE_INBOXES='def rows: if type=="array" then . elif ((.inboxes? // null)|type)=="array" then .inboxes else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","ADDRESS","NAME","ENABLED"]|@tsv),
(rows[] | [.id, (.address//""), (.display_name//""), ((.enabled//false)|tostring)] | map(tostring) | @tsv) end'

TABLE_THREADS='def rows: if type=="array" then . elif ((.threads? // null)|type)=="array" then .threads else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","INBOX","SUBJECT","MESSAGES","LAST"]|@tsv),
(rows[] | [.id, (.inbox_id//""), (.subject//""), ((.message_count//0)|tostring), (.last_message_at//"")] | map(tostring) | @tsv) end'

TABLE_LABELS='def rows: if type=="array" then . elif ((.labels? // null)|type)=="array" then .labels else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["LABEL"]|@tsv),
(rows[] | [(if type=="object" then (.label//"") else . end)] | @tsv) end'

TABLE_OUTBOX='(["ID","SUBJECT","TO","STATUS","ATTEMPTS","LAST_ERROR","NEXT_RETRY","DATE"]|@tsv),
(.[] | [.id, (.subject//""), ((.to//[])|join(", ")), (.status//""), ((.attempts//0)|tostring), (.last_error//""), (.next_retry//""), (.created_at//.received_at//"")] | map(tostring) | @tsv)'

TABLE_DRAFTS='def rows: if type=="array" then . elif ((.drafts? // null)|type)=="array" then .drafts else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","INBOX","TO","SUBJECT","STATUS","UPDATED"]|@tsv),
(rows[] | [.id, (.inbox_id//""), ((.to//[])|join(", ")), (.subject//""), (.status//""), (.updated_at//.created_at//"")] | map(tostring) | @tsv) end'

TABLE_ATTACH='def rows: if type=="array" then . elif ((.attachments? // null)|type)=="array" then .attachments else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","FILENAME","TYPE","SIZE"]|@tsv),
(rows[] | [.id, (.filename//""), (.content_type//""), ((.size//0)|tostring)] | map(tostring) | @tsv) end'

TABLE_EVENTS='def rows: if type=="array" then . elif ((.events? // null)|type)=="array" then .events else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["CURSOR","TYPE","ENTITY","INBOX","CREATED"]|@tsv),
(rows[] | [.cursor, (.type//""), (.entity_id//""), (.inbox_id//""), (.created_at//"")] | map(tostring) | @tsv) end'

TABLE_REQUESTS='def rows: if type=="array" then . elif ((.send_requests? // null)|type)=="array" then .send_requests else null end;
if rows==null then (to_entries[] | "\(.key)\t\(.value|tostring)") else
(["ID","DRAFT","INBOX","STATUS","DELIVERY","APPROVER","REQUESTED"]|@tsv),
(rows[] | [.id, (.draft_id//""), (.inbox_id//""), (.status//""), (.delivery_status//""), (.approver_email//""), (.requested_at//"")] | map(tostring) | @tsv) end'

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------

usage() {
	cat <<'EOF'
gatehouse.sh -- Gatehouse Mail agent client (Bash; curl + jq only).

usage:
  gatehouse.sh [--base URL] [--key KEY] [--inbox ID] [--table] <command> [args]

global options:
--base URL API base URL (default: $GATEHOUSE_BASE_URL, then
                 ~/.gatehouse/client.json, then http://localhost:8081
                 with a warning)
  --key KEY      API key (default: $GATEHOUSE_API_KEY, then client.json)
  --inbox ID     default inbox id for commands that use one
  --table        print a simple columnar view instead of JSON
  --version      print the client version
  -h, --help     print this help

commands:
  bootstrap        GET  /v1/bootstrap               discover account, roles, inboxes
  inboxes          GET  /v1/inboxes                 list accessible inboxes
  list             GET  /v1/messages                list messages (filters below)
  get              GET  /v1/messages/{id}           one message
  search           GET  /v1/search?q=               full-text search
  threads          GET  /v1/threads                 list threads
  thread           GET  /v1/threads/{id}            thread detail (+ --messages)
  labels           GET  /v1/labels                  distinct labels in use
  send             POST /v1/send                    send (Owner)
  reply            POST /v1/messages/{id}/reply    reply (Owner)
  mark-read        PATCH  /v1/messages/{id}         set read=true
  mark-unread      PATCH  /v1/messages/{id}         set read=false
  label            PATCH  /v1/messages/{id}         replace labels
  delete           DELETE /v1/messages/{id}         delete a message
  outbox           GET  /v1/outbox                  pending/failed sends
  retry            POST /v1/outbox/{id}/retry       re-queue a failed send
  cancel           DELETE /v1/outbox/{id}           cancel a pending send
  drafts           GET/POST /v1/drafts              list or create a draft
  draft            GET/PATCH/DELETE /v1/drafts/{id} read, edit or delete a draft
  draft-attach     POST /v1/drafts/{id}/attachments multipart upload
  request-send     POST /v1/drafts/{id}/request-send submit draft for approval
  approve          POST /v1/drafts/{id}/approve     approve a frozen draft (Owner)
  reject           POST /v1/drafts/{id}/reject      reject with feedback (Owner)
  cancel-request   POST /v1/drafts/{id}/cancel-send-request
  send-requests    GET  /v1/send-requests           approval requests
  events           GET  /v1/events?after=           one page of event history
  watch            GET  /v1/events/stream           STREAMS one JSON line/event
  wait             GET  /v1/messages/wait           BLOCKS for a new message

common flags:
  list/search:   --inbox ID --label LABEL --from ADDR --to ADDR
                 --unread --has-attachment --before ID --limit N
  send:          --to ADDR --cc ADDR --bcc ADDR --subject TEXT --text TEXT
                 --html HTML --attach FILE --sender ADDR --wait
                 --idempotency-key KEY --no-idem
  reply:         --text TEXT --html HTML --attach FILE --sender ADDR --wait
                 --idempotency-key KEY --no-idem
  label:         --labels a,b --clear-labels
  drafts/draft:  --create --to --cc --bcc --subject --text --html --from
                 --reply-to --attach --action draft|request-send|send
  draft-attach:  --attach FILE (repeatable) --list
  request-send:  --external
  approve/reject:--feedback TEXT
  events/watch:  --after CURSOR --inbox ID
  wait:          --after CURSOR --inbox ID --timeout SECONDS

realtime:
  watch never returns: it streams until interrupted (Ctrl-C).
  wait blocks until a message arrives or --timeout (max 600s), then returns.

credentials:
  --base/--key, then GATEHOUSE_BASE_URL/GATEHOUSE_API_KEY, then
  ~/.gatehouse/client.json (chmod 600). Never ~/.hermes/.env.

exit codes: 0 ok, 1 error, 2 usage. JSON to stdout, diagnostics to stderr.
EOF
}

# ---------------------------------------------------------------------------
# Errors and tooling
# ---------------------------------------------------------------------------

err() {
	printf 'gatehouse: %s\n' "$*" >&2
}

die() {
	err "$*"
	exit 1
}

die_usage() {
	err "$*"
	printf '\n' >&2
	usage >&2
	exit 2
}

warn() {
	printf 'gatehouse: warning: %s\n' "$*" >&2
}

require_tools() {
	local ok=1
	if ! command -v curl >/dev/null 2>&1; then
		err "curl is required but was not found on PATH"
		ok=0
	fi
	if ! command -v jq >/dev/null 2>&1; then
		err "jq is required but was not found on PATH"
		ok=0
	fi
	if [ "$ok" -ne 1 ]; then
		err "install curl and jq (--help needs neither)"
		exit 1
	fi
}

require_base64() {
	if ! command -v base64 >/dev/null 2>&1; then
		die "base64 is required to attach files but was not found on PATH"
	fi
}

# ---------------------------------------------------------------------------
# Credentials
# ---------------------------------------------------------------------------

load_config() {
	[ -f "$CONFIG_PATH" ] || return 0
	local perms mode
	perms="$(stat -c '%a' "$CONFIG_PATH" 2>/dev/null || stat -f '%Lp' "$CONFIG_PATH" 2>/dev/null || true)"
	if [ -n "$perms" ]; then
		mode="${perms: -3}"
		if [ "$mode" != "600" ]; then
			warn "$CONFIG_PATH is mode $perms; expected 0600"
		fi
	fi
	if [ -z "$BASE" ]; then
		BASE="$(jq -r '.base_url // .base // empty' "$CONFIG_PATH" 2>/dev/null || true)"
	fi
	if [ -z "$KEY" ]; then
		KEY="$(jq -r '.api_key // .key // empty' "$CONFIG_PATH" 2>/dev/null || true)"
	fi
	if [ -z "$INBOX" ]; then
		INBOX="$(jq -r '.inbox // empty' "$CONFIG_PATH" 2>/dev/null || true)"
	fi
}

# ---------------------------------------------------------------------------
# URL / query helpers
# ---------------------------------------------------------------------------

urlenc() {
	jq -rn --arg v "$1" '$v|@uri'
}

addq() {
	local enc
	enc="$(urlenc "$2")"
	if [ -z "$QUERY" ]; then
		QUERY="$1=$enc"
	else
		QUERY="$QUERY&$1=$enc"
	fi
}

add_multi() {
	local name="$1"
	shift
	local v
	for v in "$@"; do
		addq "$name" "$v"
	done
}

# to_json_of ADDR... -> JSON array, splitting each value on commas.
to_json_of() {
	if [ "$#" -eq 0 ]; then
		printf '[]'
		return
	fi
	jq -n -c --args '$ARGS.positional | map(split(",")) | add | map(sub("^\\s+|\\s+$";"")) | map(select(length>0))' "$@"
}

idem_key() {
	printf 'gh-%s-%s-%s' "$(date +%s 2>/dev/null || printf '0')" "$$" "$RANDOM$RANDOM"
}

idem_for_request() {
	if [ "$NO_IDEM" -eq 1 ]; then
		return 0
	fi
	if [ -n "$IDEM_KEY" ]; then
		printf '%s' "$IDEM_KEY"
	else
		idem_key
	fi
}

mime_for() {
	local ext
	ext="$(printf '%s' "${1##*.}" | tr '[:upper:]' '[:lower:]')"
	case "$ext" in
	pdf) printf 'application/pdf' ;;
	png) printf 'image/png' ;;
	jpg | jpeg) printf 'image/jpeg' ;;
	gif) printf 'image/gif' ;;
	webp) printf 'image/webp' ;;
	svg) printf 'image/svg+xml' ;;
	txt | log | md) printf 'text/plain' ;;
	csv) printf 'text/csv' ;;
	html | htm) printf 'text/html' ;;
	json) printf 'application/json' ;;
	xml) printf 'application/xml' ;;
	zip) printf 'application/zip' ;;
	gz | tgz) printf 'application/gzip' ;;
	doc) printf 'application/msword' ;;
	docx) printf 'application/vnd.openxmlformats-officedocument.wordprocessingml.document' ;;
	xls) printf 'application/vnd.ms-excel' ;;
	xlsx) printf 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet' ;;
	ppt) printf 'application/vnd.ms-powerpoint' ;;
	pptx) printf 'application/vnd.openxmlformats-officedocument.presentationml.presentation' ;;
	eml) printf 'message/rfc822' ;;
	ics) printf 'text/calendar' ;;
	*) printf 'application/octet-stream' ;;
	esac
}

# ---------------------------------------------------------------------------
# Attachments
# ---------------------------------------------------------------------------

build_attachments() {
	ATT_JSON_FILE="$WORKDIR/attachments.json"
	if [ "${#ATTACH_FILES[@]}" -eq 0 ]; then
		printf '[]' >"$ATT_JSON_FILE"
		return 0
	fi
	require_base64
	local ndjson="$WORKDIR/att.ndjson"
	: >"$ndjson"
	local f b64 fn ct
	for f in "${ATTACH_FILES[@]}"; do
		if [ ! -f "$f" ]; then
			die "cannot read $f"
		fi
		b64="$WORKDIR/att.b64"
		if base64 -w0 "$f" >"$b64" 2>/dev/null; then
			:
		else
			base64 "$f" | tr -d '\n' >"$b64"
		fi
		fn="$(basename -- "$f")"
		ct="$(mime_for "$f")"
		jq -n --rawfile c "$b64" --arg fn "$fn" --arg ct "$ct" \
			'{filename:$fn, content_type:$ct, content:$c}' >>"$ndjson"
	done
	jq -s '.' "$ndjson" >"$ATT_JSON_FILE"
}

# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

# api_fetch METHOD PATH [JSON_BODY] [IDEM] [MAXTIME]
# Sets RESP_TMP and RESP_STATUS. Fails clearly on transport errors.
api_fetch() {
	local method="$1" path="$2" body="${3:-}" idem="${4:-}" maxtime="${5:-60}"
	require_tools
	local -a cargs=(-sS --max-time "$maxtime" -X "$method" -A "gatehouse.sh/$VERSION"
		-H "Authorization: Bearer $KEY" -H "Accept: application/json")
	if [ -n "$body" ]; then
		cargs+=(-H "Content-Type: application/json" --data-binary "$body")
	fi
	if [ -n "$idem" ]; then
		cargs+=(-H "Idempotency-Key: $idem")
	fi
	RESP_TMP="$(mktemp "$WORKDIR/resp.XXXXXX")"
	if ! RESP_STATUS="$(curl "${cargs[@]}" -o "$RESP_TMP" -w '%{http_code}' "$BASE$path")"; then
		die "request failed: $method $path"
	fi
}

handle_response() {
	local status="$1" tmp="$2" filter="${3:-}"
	if [ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
		if [ "$status" -eq 204 ]; then
			if [ "$TABLE" -eq 1 ]; then
				printf 'ok: true\n'
			else
				printf '{"ok": true}\n'
			fi
			return 0
		fi
		if [ ! -s "$tmp" ]; then
			return 0
		fi
		if [ "$TABLE" -eq 1 ]; then
			if [ -n "$filter" ]; then
				jq -r "$filter" "$tmp"
			else
				jq -r "$TABLE_DEFAULT" "$tmp"
			fi
		else
			if ! jq . "$tmp" 2>/dev/null; then
				cat "$tmp"
			fi
		fi
		return 0
	fi
	local msg
	msg="$(jq -r '.error // empty' "$tmp" 2>/dev/null || true)"
	if [ -z "$msg" ]; then
		msg="$(cat "$tmp" 2>/dev/null || true)"
	fi
	if [ -z "$msg" ]; then
		msg="HTTP $status"
	fi
	err "$msg"
	exit 1
}

# api METHOD PATH [JSON_BODY] [IDEM] [TABLE_FILTER] [MAXTIME]
api() {
	local method="$1" path="$2" body="${3:-}" idem="${4:-}" filter="${5:-}" maxtime="${6:-60}"
	api_fetch "$method" "$path" "$body" "$idem" "$maxtime"
	handle_response "$RESP_STATUS" "$RESP_TMP" "$filter"
}

upload_attachments() {
	local id="$1"
	shift
	require_tools
	local -a fargs=()
	local f ct
	for f in "$@"; do
		if [ ! -f "$f" ]; then
			die "cannot read $f"
		fi
		ct="$(mime_for "$f")"
		fargs+=(-F "attachments=@${f};type=${ct}")
	done
	local tmp status
	local -a cargs=(-sS --max-time 120 -X POST -A "gatehouse.sh/$VERSION"
		-H "Authorization: Bearer $KEY")
	cargs+=("${fargs[@]}")
	tmp="$(mktemp "$WORKDIR/resp.XXXXXX")"
	if ! status="$(curl "${cargs[@]}" -o "$tmp" -w '%{http_code}' "$BASE/v1/drafts/$id/attachments")"; then
		die "upload failed"
	fi
	handle_response "$status" "$tmp" "$TABLE_ATTACH"
}

# ---------------------------------------------------------------------------
# Body builders
# ---------------------------------------------------------------------------

build_send_body() {
	local to_json cc_json bcc_json
	to_json="$(to_json_of ${TO_PARTS[@]+"${TO_PARTS[@]}"})"
	cc_json="$(to_json_of ${CC_PARTS[@]+"${CC_PARTS[@]}"})"
	bcc_json="$(to_json_of ${BCC_PARTS[@]+"${BCC_PARTS[@]}"})"
	jq -n -c \
		--arg inbox "$INBOX" \
		--arg subject "$SUBJECT" --arg text "$TEXT" --arg html "$HTML" --arg sender "$SENDER" \
		--argjson to "$to_json" --argjson cc "$cc_json" --argjson bcc "$bcc_json" \
		--argjson has_subject "$SUBJECT_SET" --argjson has_text "$TEXT_SET" --argjson has_html "$HTML_SET" \
		--slurpfile att "$ATT_JSON_FILE" \
		'{to:$to}
     + (if ($cc|length)>0 then {cc:$cc} else {} end)
     + (if ($bcc|length)>0 then {bcc:$bcc} else {} end)
     + (if $has_subject==1 then {subject:$subject} else {} end)
     + (if $has_text==1 then {text:$text} else {} end)
     + (if $has_html==1 then {html:$html} else {} end)
     + (if $sender!="" then {sender:$sender} else {} end)
     + (if $inbox!="" then {inbox_id:$inbox} else {} end)
     + (if ($att[0]|length)>0 then {attachments:$att[0]} else {} end)'
}

build_reply_body() {
	jq -n -c \
		--arg text "$TEXT" --arg html "$HTML" --arg sender "$SENDER" \
		--argjson has_text "$TEXT_SET" --argjson has_html "$HTML_SET" \
		--slurpfile att "$ATT_JSON_FILE" \
		'{}
     + (if $has_text==1 then {text:$text} else {} end)
     + (if $has_html==1 then {html:$html} else {} end)
     + (if $sender!="" then {sender:$sender} else {} end)
     + (if ($att[0]|length)>0 then {attachments:$att[0]} else {} end)'
}

# build_draft_body INCLUDE_INBOX(0|1)
build_draft_body() {
	local include_inbox="${1:-0}"
	local to_json cc_json bcc_json
	to_json="$(to_json_of ${TO_PARTS[@]+"${TO_PARTS[@]}"})"
	cc_json="$(to_json_of ${CC_PARTS[@]+"${CC_PARTS[@]}"})"
	bcc_json="$(to_json_of ${BCC_PARTS[@]+"${BCC_PARTS[@]}"})"
	jq -n -c \
		--arg inbox "$INBOX" --arg from_addr "$FROM_ADDR" --arg reply_to "$REPLY_TO" \
		--arg subject "$SUBJECT" --arg text "$TEXT" --arg html "$HTML" --arg action "$ACTION" \
		--argjson to "$to_json" --argjson cc "$cc_json" --argjson bcc "$bcc_json" \
		--argjson has_subject "$SUBJECT_SET" --argjson has_text "$TEXT_SET" --argjson has_html "$HTML_SET" \
		--argjson include_inbox "$include_inbox" \
		--slurpfile att "$ATT_JSON_FILE" \
		'{}
     + (if ($include_inbox==1 and $inbox!="") then {inbox_id:$inbox} else {} end)
     + (if ($to|length)>0 then {to:$to} else {} end)
     + (if ($cc|length)>0 then {cc:$cc} else {} end)
     + (if ($bcc|length)>0 then {bcc:$bcc} else {} end)
     + (if $has_subject==1 then {subject:$subject} else {} end)
     + (if $has_text==1 then {text:$text} else {} end)
     + (if $has_html==1 then {html:$html} else {} end)
     + (if $from_addr!="" then {from_address:$from_addr} else {} end)
     + (if $reply_to!="" then {reply_to_message_id:$reply_to} else {} end)
     + (if $action!="" then {action:$action} else {} end)
     + (if ($att[0]|length)>0 then {attachments:$att[0]} else {} end)'
}

# ---------------------------------------------------------------------------
# Positional helpers
# ---------------------------------------------------------------------------

need_pos() {
	if [ "${#POSITIONAL[@]}" -lt 1 ]; then
		die_usage "${1:-$COMMAND}: missing required argument"
	fi
	POSITION="${POSITIONAL[0]}"
}

reject_extra() {
	if [ "${#POSITIONAL[@]}" -gt 1 ]; then
		die_usage "$COMMAND: unexpected argument: ${POSITIONAL[1]}"
	fi
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

cmd_bootstrap() {
	reject_extra
	api GET "/v1/bootstrap" "" "" "$TABLE_INBOXES"
}

cmd_inboxes() {
	reject_extra
	api GET "/v1/inboxes" "" "" "$TABLE_INBOXES"
}

cmd_list() {
	reject_extra
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	add_multi label ${LABEL_PARTS[@]+"${LABEL_PARTS[@]}"}
	if [ -n "$FROM_Q" ]; then addq from "$FROM_Q"; fi
	if [ "${#TO_PARTS[@]}" -gt 0 ]; then addq to "${TO_PARTS[0]}"; fi
	if [ "$UNREAD" -eq 1 ]; then addq unread true; fi
	if [ "$HAS_ATT" -eq 1 ]; then addq has_attachment true; fi
	if [ -n "$BEFORE" ]; then addq before "$BEFORE"; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/messages"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_MESSAGES"
}

cmd_get() {
	need_pos "$COMMAND"
	reject_extra
	api GET "/v1/messages/$POSITION" "" "" "$TABLE_MESSAGES"
}

cmd_search() {
	local query=""
	if [ "${#POSITIONAL[@]}" -gt 0 ]; then
		query="${POSITIONAL[0]}"
	fi
	if [ -z "$query" ]; then
		query="$Q"
	fi
	if [ -z "$query" ]; then
		die_usage "search: missing QUERY (positional or --q)"
	fi
	reject_extra
	QUERY=""
	addq q "$query"
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	add_multi label ${LABEL_PARTS[@]+"${LABEL_PARTS[@]}"}
	if [ -n "$FROM_Q" ]; then addq from "$FROM_Q"; fi
	if [ "${#TO_PARTS[@]}" -gt 0 ]; then addq to "${TO_PARTS[0]}"; fi
	if [ "$HAS_ATT" -eq 1 ]; then addq has_attachment true; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	api GET "/v1/search?$QUERY" "" "" "$TABLE_MESSAGES"
}

cmd_threads() {
	reject_extra
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/threads"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_THREADS"
}

cmd_thread() {
	need_pos "$COMMAND"
	reject_extra
	local path="/v1/threads/$POSITION"
	if [ "$MODE" = "messages" ]; then
		api GET "$path/messages" "" "" "$TABLE_MESSAGES"
	else
		api GET "$path" "" "" "$TABLE_THREADS"
	fi
}

cmd_labels() {
	reject_extra
	api GET "/v1/labels" "" "" "$TABLE_LABELS"
}

cmd_send() {
	reject_extra
	if [ "${#TO_PARTS[@]}" -eq 0 ]; then
		die_usage "send: --to is required"
	fi
	build_attachments
	local body idem
	body="$(build_send_body)"
	idem="$(idem_for_request)"
	local path="/v1/send"
	if [ "$WAIT" -eq 1 ]; then path="$path?wait=true"; fi
	api POST "$path" "$body" "$idem" "$TABLE_DEFAULT"
}

cmd_reply() {
	need_pos "$COMMAND"
	reject_extra
	build_attachments
	local body idem path
	body="$(build_reply_body)"
	idem="$(idem_for_request)"
	path="/v1/messages/$POSITION/reply"
	if [ "$WAIT" -eq 1 ]; then path="$path?wait=true"; fi
	api POST "$path" "$body" "$idem" "$TABLE_DEFAULT"
}

cmd_mark_read() {
	need_pos "$COMMAND"
	reject_extra
	api PATCH "/v1/messages/$POSITION" '{"read":true}' "" "$TABLE_MESSAGES"
}

cmd_mark_unread() {
	need_pos "$COMMAND"
	reject_extra
	api PATCH "/v1/messages/$POSITION" '{"read":false}' "" "$TABLE_MESSAGES"
}

cmd_label() {
	need_pos "$COMMAND"
	reject_extra
	local labels_json
	if [ "$CLEAR_LABELS" -eq 1 ]; then
		labels_json='[]'
	elif [ "$LABELS_SET" -eq 1 ]; then
		labels_json="$(jq -n -c --args '$ARGS.positional | map(split(",")) | add | map(sub("^\\s+|\\s+$";"")) | map(select(length>0))' ${LABELS_PARTS[@]+"${LABELS_PARTS[@]}"})"
	else
		die_usage "label: needs --labels a,b or --clear-labels"
	fi
	local body
	body="$(jq -n -c --argjson labels "$labels_json" '{labels:$labels}')"
	api PATCH "/v1/messages/$POSITION" "$body" "" "$TABLE_MESSAGES"
}

cmd_delete() {
	need_pos "$COMMAND"
	reject_extra
	api DELETE "/v1/messages/$POSITION" "" "" "$TABLE_DEFAULT"
}

cmd_outbox() {
	reject_extra
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/outbox"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_OUTBOX"
}

cmd_retry() {
	need_pos "$COMMAND"
	reject_extra
	api POST "/v1/outbox/$POSITION/retry" "" "" "$TABLE_DEFAULT"
}

cmd_cancel() {
	need_pos "$COMMAND"
	reject_extra
	api DELETE "/v1/outbox/$POSITION" "" "" "$TABLE_DEFAULT"
}

cmd_drafts() {
	reject_extra
	local write=0
	if [ "$CREATE" -eq 1 ] || [ -n "$ACTION" ] || [ "${#TO_PARTS[@]}" -gt 0 ] ||
		[ "${#CC_PARTS[@]}" -gt 0 ] || [ "${#BCC_PARTS[@]}" -gt 0 ] ||
		[ "$SUBJECT_SET" -eq 1 ] || [ "$TEXT_SET" -eq 1 ] || [ "$HTML_SET" -eq 1 ] ||
		[ -n "$FROM_ADDR" ] || [ -n "$REPLY_TO" ] || [ "${#ATTACH_FILES[@]}" -gt 0 ]; then
		write=1
	fi
	if [ "$write" -eq 1 ]; then
		build_attachments
		local body idem
		body="$(build_draft_body 1)"
		idem="$(idem_for_request)"
		api POST "/v1/drafts" "$body" "$idem" "$TABLE_DRAFTS"
		return
	fi
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ -n "$BEFORE" ]; then addq before "$BEFORE"; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/drafts"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_DRAFTS"
}

cmd_draft() {
	need_pos "$COMMAND"
	reject_extra
	if [ "$DELETE_DRAFT" -eq 1 ]; then
		api DELETE "/v1/drafts/$POSITION" "" "" "$TABLE_DEFAULT"
		return
	fi
	build_attachments
	local body
	body="$(build_draft_body 0)"
	if [ "$body" = "{}" ]; then
		api GET "/v1/drafts/$POSITION" "" "" "$TABLE_DRAFTS"
		return
	fi
	local idem
	idem="$(idem_for_request)"
	api PATCH "/v1/drafts/$POSITION" "$body" "$idem" "$TABLE_DRAFTS"
}

cmd_draft_attach() {
	need_pos "$COMMAND"
	reject_extra
	if [ "$LIST_MODE" -eq 1 ]; then
		api GET "/v1/drafts/$POSITION/attachments" "" "" "$TABLE_ATTACH"
		return
	fi
	if [ "${#ATTACH_FILES[@]}" -eq 0 ]; then
		die_usage "draft-attach needs one or more --attach FILE (or --list)"
	fi
	upload_attachments "$POSITION" ${ATTACH_FILES[@]+"${ATTACH_FILES[@]}"}
}

cmd_request_send() {
	need_pos "$COMMAND"
	reject_extra
	local body=""
	if [ "$EXTERNAL" -eq 1 ]; then body='{"external":true}'; fi
	local idem
	idem="$(idem_for_request)"
	api POST "/v1/drafts/$POSITION/request-send" "$body" "$idem" "$TABLE_DRAFTS"
}

cmd_approve() {
	need_pos "$COMMAND"
	reject_extra
	local body=""
	if [ "$FEEDBACK_SET" -eq 1 ]; then
		body="$(jq -n -c --arg f "$FEEDBACK" '{feedback:$f}')"
	fi
	local idem
	idem="$(idem_for_request)"
	api POST "/v1/drafts/$POSITION/approve" "$body" "$idem" "$TABLE_DEFAULT"
}

cmd_reject() {
	need_pos "$COMMAND"
	reject_extra
	local body=""
	if [ "$FEEDBACK_SET" -eq 1 ]; then
		body="$(jq -n -c --arg f "$FEEDBACK" '{feedback:$f}')"
	fi
	api POST "/v1/drafts/$POSITION/reject" "$body" "" "$TABLE_DRAFTS"
}

cmd_cancel_request() {
	need_pos "$COMMAND"
	reject_extra
	api POST "/v1/drafts/$POSITION/cancel-send-request" "" "" "$TABLE_DRAFTS"
}

cmd_send_requests() {
	reject_extra
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ "$ACTIVE" -eq 1 ]; then addq active true; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/send-requests"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_REQUESTS"
}

cmd_events() {
	reject_extra
	QUERY=""
	if [ -n "$AFTER" ]; then addq after "$AFTER"; fi
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ -n "$LIMIT" ]; then addq limit "$LIMIT"; fi
	local path="/v1/events"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	api GET "$path" "" "" "$TABLE_EVENTS"
}

# watch streams SSE until interrupted (Ctrl-C) on a healthy stream. It also
# records curl's exit status and the HTTP status line so a non-2xx response or a
# transport failure is reported to stderr and exits non-zero, rather than being
# mistaken for a clean end of stream.
cmd_watch() {
	reject_extra
	require_tools
	QUERY=""
	if [ -n "$AFTER" ]; then addq after "$AFTER"; fi
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	local path="/v1/events/stream"
	if [ -n "$QUERY" ]; then path="$path?$QUERY"; fi
	err "watching $BASE$path (streams until Ctrl-C; this command does not return)"
	local hdr="$WORKDIR/watch.headers"
	local status_file="$WORKDIR/watch.status"
	: >"$hdr"
	local data=""
	local raw=""
	local line field value
	while IFS= read -r line || [ -n "$line" ]; do
		line="${line%$'\r'}"
		if [ -z "$line" ]; then
			if [ -n "$data" ]; then
				if ! printf '%s\n' "$data" | jq -c . 2>/dev/null; then
					printf '%s\n' "$data"
				fi
				data=""
			fi
			continue
		fi
		case "$line" in
		:*) continue ;;
		esac
		field="${line%%:*}"
		value="${line#*:}"
		value="${value# }"
		if [ "$field" = "data" ]; then
			if [ -z "$data" ]; then
				data="$value"
			else
				data="$data
$value"
			fi
		else
			case "$line" in
			\{*) raw="$line" ;;
			esac
		fi
	done < <(set +e; curl -N -sS -D "$hdr" -A "gatehouse.sh/$VERSION" \
		-H "Authorization: Bearer $KEY" -H "Accept: text/event-stream" "$BASE$path"
		printf '%s' "$?" >"$status_file")
	if [ -n "$data" ]; then
		if ! printf '%s\n' "$data" | jq -c . 2>/dev/null; then
			printf '%s\n' "$data"
		fi
	fi
	local curl_status=0
	if [ -f "$status_file" ]; then
		curl_status="$(cat "$status_file")"
	fi
	local status=""
	if [ -f "$hdr" ]; then
		while IFS= read -r line; do
			case "$line" in
			HTTP/*)
				read -r _ver status _reason <<<"$line"
				;;
			esac
		done <"$hdr"
	fi
	case "$status" in
	'' | *[!0-9]*) status="" ;;
	esac
	if [ "$curl_status" -eq 0 ] && [ -n "$status" ] &&
		[ "$status" -ge 200 ] && [ "$status" -lt 300 ]; then
		return 0
	fi
	local msg
	msg="$(printf '%s' "$raw" | jq -r '.error // empty' 2>/dev/null || true)"
	if [ -z "$msg" ]; then
		msg="${raw//$'\n'/ }"
	fi
	if [ -z "$msg" ]; then
		msg="stream failed"
	fi
	if [ -z "$status" ]; then
		err "watch failed: $msg (curl exit $curl_status)"
	else
		err "watch failed: HTTP $status: $msg"
	fi
	exit 1
}

# wait blocks (long-poll) on /v1/messages/wait until a message or timeout.
cmd_wait() {
	reject_extra
	local t="${TIMEOUT:-60}"
	QUERY=""
	if [ -n "$INBOX" ]; then addq inbox "$INBOX"; fi
	if [ -n "$AFTER" ]; then addq after "$AFTER"; fi
	addq timeout "$t"
	api_fetch GET "/v1/messages/wait?$QUERY" "" "" "$((t + 15))"
	if [ "$RESP_STATUS" -eq 204 ]; then
		err "no new message before timeout (${t}s)"
		exit 0
	fi
	handle_response "$RESP_STATUS" "$RESP_TMP" "$TABLE_MESSAGES"
}

# ---------------------------------------------------------------------------
# Option parser
# ---------------------------------------------------------------------------

parse_flags() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--base)
			BASE="$2"
			shift 2
			;;
		--base=*)
			BASE="${1#*=}"
			shift
			;;
		--key)
			KEY="$2"
			shift 2
			;;
		--key=*)
			KEY="${1#*=}"
			shift
			;;
		--inbox)
			INBOX="$2"
			shift 2
			;;
		--inbox=*)
			INBOX="${1#*=}"
			shift
			;;
		--table)
			TABLE=1
			shift
			;;
		--after)
			AFTER="$2"
			shift 2
			;;
		--after=*)
			AFTER="${1#*=}"
			shift
			;;
		--before)
			BEFORE="$2"
			shift 2
			;;
		--before=*)
			BEFORE="${1#*=}"
			shift
			;;
		--limit)
			LIMIT="$2"
			shift 2
			;;
		--limit=*)
			LIMIT="${1#*=}"
			shift
			;;
		--timeout)
			TIMEOUT="$2"
			shift 2
			;;
		--timeout=*)
			TIMEOUT="${1#*=}"
			shift
			;;
		--q | --query)
			Q="$2"
			shift 2
			;;
		--q=* | --query=*)
			Q="${1#*=}"
			shift
			;;
		--from)
			FROM_Q="$2"
			FROM_ADDR="$2"
			shift 2
			;;
		--from=*)
			FROM_Q="${1#*=}"
			FROM_ADDR="${1#*=}"
			shift
			;;
		--to)
			TO_PARTS+=("$2")
			shift 2
			;;
		--to=*)
			TO_PARTS+=("${1#*=}")
			shift
			;;
		--cc)
			CC_PARTS+=("$2")
			shift 2
			;;
		--cc=*)
			CC_PARTS+=("${1#*=}")
			shift
			;;
		--bcc)
			BCC_PARTS+=("$2")
			shift 2
			;;
		--bcc=*)
			BCC_PARTS+=("${1#*=}")
			shift
			;;
		--label)
			LABEL_PARTS+=("$2")
			shift 2
			;;
		--label=*)
			LABEL_PARTS+=("${1#*=}")
			shift
			;;
		--labels)
			LABELS_PARTS+=("$2")
			LABELS_SET=1
			shift 2
			;;
		--labels=*)
			LABELS_PARTS+=("${1#*=}")
			LABELS_SET=1
			shift
			;;
		--clear-labels)
			CLEAR_LABELS=1
			shift
			;;
		--subject)
			SUBJECT="$2"
			SUBJECT_SET=1
			shift 2
			;;
		--subject=*)
			SUBJECT="${1#*=}"
			SUBJECT_SET=1
			shift
			;;
		--text)
			TEXT="$2"
			TEXT_SET=1
			shift 2
			;;
		--text=*)
			TEXT="${1#*=}"
			TEXT_SET=1
			shift
			;;
		--html)
			HTML="$2"
			HTML_SET=1
			shift 2
			;;
		--html=*)
			HTML="${1#*=}"
			HTML_SET=1
			shift
			;;
		--sender)
			SENDER="$2"
			shift 2
			;;
		--sender=*)
			SENDER="${1#*=}"
			shift
			;;
		--reply-to)
			REPLY_TO="$2"
			shift 2
			;;
		--reply-to=*)
			REPLY_TO="${1#*=}"
			shift
			;;
		--attach | --attachment)
			ATTACH_FILES+=("$2")
			shift 2
			;;
		--attach=* | --attachment=*)
			ATTACH_FILES+=("${1#*=}")
			shift
			;;
		--action)
			case "$2" in
			draft | request-send | send) ACTION="$2" ;;
			*) die_usage "unknown action: $2 (expected draft, request-send or send)" ;;
			esac
			shift 2
			;;
		--action=*)
			case "${1#*=}" in
			draft | request-send | send) ACTION="${1#*=}" ;;
			*) die_usage "unknown action: ${1#*=}" ;;
			esac
			shift
			;;
		--feedback)
			FEEDBACK="$2"
			FEEDBACK_SET=1
			shift 2
			;;
		--feedback=*)
			FEEDBACK="${1#*=}"
			FEEDBACK_SET=1
			shift
			;;
		--idempotency-key)
			IDEM_KEY="$2"
			shift 2
			;;
		--idempotency-key=*)
			IDEM_KEY="${1#*=}"
			shift
			;;
		--no-idem | --no-idempotency)
			NO_IDEM=1
			shift
			;;
		--wait)
			WAIT=1
			shift
			;;
		--unread)
			UNREAD=1
			shift
			;;
		--has-attachment)
			HAS_ATT=1
			shift
			;;
		--active)
			ACTIVE=1
			shift
			;;
		--create)
			CREATE=1
			shift
			;;
		--external)
			EXTERNAL=1
			shift
			;;
		--messages)
			MODE="messages"
			shift
			;;
		--delete)
			DELETE_DRAFT=1
			shift
			;;
		--list)
			LIST_MODE=1
			shift
			;;
		--version)
			printf 'gatehouse.sh %s\n' "$VERSION"
			exit 0
			;;
		--help | -h)
			usage
			exit 0
			;;
		--)
			shift
			break
			;;
		-*)
			die_usage "unknown option: $1"
			;;
		*)
			POSITIONAL+=("$1")
			shift
			;;
		esac
	done
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

main() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--base)
			BASE="$2"
			shift 2
			;;
		--base=*)
			BASE="${1#*=}"
			shift
			;;
		--key)
			KEY="$2"
			shift 2
			;;
		--key=*)
			KEY="${1#*=}"
			shift
			;;
		--inbox)
			INBOX="$2"
			shift 2
			;;
		--inbox=*)
			INBOX="${1#*=}"
			shift
			;;
		--table)
			TABLE=1
			shift
			;;
		--version)
			printf 'gatehouse.sh %s\n' "$VERSION"
			exit 0
			;;
		--help | -h)
			usage
			exit 0
			;;
		--)
			shift
			break
			;;
		-*)
			die_usage "unknown global option: $1"
			;;
		*)
			break
			;;
		esac
	done

	if [ "$#" -eq 0 ]; then
		die_usage "missing command"
	fi

	COMMAND="$1"
	shift

	POSITIONAL=()
	parse_flags "$@"

	if [ "$COMMAND" = "help" ]; then
		usage
		exit 0
	fi

	require_tools
	load_config
	if [ -z "$BASE" ]; then
		BASE="$DEFAULT_BASE_URL"
		printf 'gatehouse.sh: GATEHOUSE_BASE_URL not set; defaulting to %s\n' "$BASE" >&2
	fi
	BASE="${BASE%/}"
	if [ -z "$KEY" ]; then
		die "no API key: pass --key, set GATEHOUSE_API_KEY, or write $CONFIG_PATH"
	fi

	WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/gatehouse.XXXXXX")"
	trap 'rm -rf "$WORKDIR"' EXIT

	case "$COMMAND" in
	bootstrap) cmd_bootstrap ;;
	inboxes) cmd_inboxes ;;
	list) cmd_list ;;
	get) cmd_get ;;
	search) cmd_search ;;
	threads) cmd_threads ;;
	thread) cmd_thread ;;
	labels) cmd_labels ;;
	send) cmd_send ;;
	reply) cmd_reply ;;
	mark-read) cmd_mark_read ;;
	mark-unread) cmd_mark_unread ;;
	label) cmd_label ;;
	delete) cmd_delete ;;
	outbox) cmd_outbox ;;
	retry) cmd_retry ;;
	cancel) cmd_cancel ;;
	drafts) cmd_drafts ;;
	draft) cmd_draft ;;
	draft-attach) cmd_draft_attach ;;
	request-send) cmd_request_send ;;
	approve) cmd_approve ;;
	reject) cmd_reject ;;
	cancel-request) cmd_cancel_request ;;
	send-requests) cmd_send_requests ;;
	events) cmd_events ;;
	watch) cmd_watch ;;
	wait) cmd_wait ;;
	*) die_usage "unknown command: $COMMAND" ;;
	esac
}

main "$@"
