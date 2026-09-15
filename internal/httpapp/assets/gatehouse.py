#!/usr/bin/env python3
"""Gatehouse Mail agent client (Python 3 standard library only).

This is the canonical example client served at ``GET /examples/python``. It
wraps the whole agent-facing REST surface of Gatehouse Mail and is meant to be
read as much as run: every command maps one-to-one onto an HTTP route, and the
``--help`` text for each command lists every option.

Quick start
-----------
::

    # 1. Point the client at an instance and an API key.
    export GATEHOUSE_BASE_URL="http://localhost:8081"
    export GATEHOUSE_API_KEY="ghm_..."

    # 2. Discover who you are and which inboxes you can reach.
    python3 gatehouse.py bootstrap
    python3 gatehouse.py inboxes --table

    # 3. Read mail.
    python3 gatehouse.py list --inbox inb_123 --unread --limit 20 --table
    python3 gatehouse.py get msg_123
    python3 gatehouse.py search "invoice" --inbox inb_123
    python3 gatehouse.py threads --inbox inb_123
    python3 gatehouse.py thread thr_123
    python3 gatehouse.py labels

    # 4. Send and reply.
    python3 gatehouse.py send --to alice@example.com --subject "Hi" --text "Hello"
    python3 gatehouse.py send --to a@x,b@y --subject "Report" --text "See attached" \
        --attach report.pdf --wait
    python3 gatehouse.py reply msg_123 --text "Thanks!"

    # 5. Triage.
    python3 gatehouse.py mark-read msg_123
    python3 gatehouse.py label msg_123 --labels "Invoices,Unpaid"
    python3 gatehouse.py delete msg_123

    # 6. Drafts and human-in-the-loop approval.
    python3 gatehouse.py drafts --inbox inb_123
    python3 gatehouse.py drafts --inbox inb_123 --to alice@example.com \
        --subject "Quote" --text "..." --action request-send
    python3 gatehouse.py send-requests --active
    python3 gatehouse.py approve drf_123 --feedback "Looks good"

    # 7. Realtime and long-poll.
    python3 gatehouse.py events --after evt_100
    python3 gatehouse.py watch --after evt_100      # streams until Ctrl-C
    python3 gatehouse.py wait --inbox inb_123 --timeout 120

Credentials
-----------
Resolution order (first non-empty wins):

1. ``--base`` / ``--key`` command-line flags
2. ``GATEHOUSE_BASE_URL`` / ``GATEHOUSE_API_KEY`` environment variables
3. ``~/.gatehouse/client.json`` (create it mode 0600)::

       {
         "base_url": "http://localhost:8081",
         "api_key": "ghm_...",
         "inbox": "inb_123"
       }

The client never reads ``~/.hermes/.env`` or any other credential store.

Output and exit codes
---------------------
JSON goes to stdout by default so it can be piped into ``jq``; diagnostics go
to stderr. ``--table`` prints a simple columnar view of list results instead.
Exit codes: ``0`` success, ``1`` error (any non-2xx response prints the API's
``error`` field to stderr), ``2`` usage error.

Realtime caveat
---------------
``watch`` opens the SSE endpoint and **streams one JSON line per event until it
is interrupted** (Ctrl-C); it never returns on its own. ``wait`` calls the JSON
long-poll endpoint and **blocks** until a new message arrives or ``--timeout``
elapses (server-side cap: 600 seconds), then returns. Neither is suitable for a
single non-blocking poll: use ``events`` (one page of history) or ``list`` for
that. Both streaming commands hold a network connection open, so run them on a
dedicated worker rather than in the middle of a request/response cycle.
"""

import argparse
import base64
import json
import mimetypes
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid

DEFAULT_BASE_URL = "http://localhost:8081"
CONFIG_PATH = os.path.join(os.path.expanduser("~"), ".gatehouse", "client.json")

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_USAGE = 2

EPILOG = """\
commands:
  bootstrap        GET  /v1/bootstrap              discover account, roles, inboxes
  inboxes          GET  /v1/inboxes                list accessible inboxes
  list             GET  /v1/messages               list messages (filters below)
  get              GET  /v1/messages/{id}          one message
  search           GET  /v1/search?q=               full-text search
  threads          GET  /v1/threads                list threads
  thread           GET  /v1/threads/{id}           thread detail (+ --messages)
  labels           GET  /v1/labels                 distinct labels in use
  send             POST /v1/send                   send (Owner)
  reply            POST /v1/messages/{id}/reply   reply (Owner)
  mark-read        PATCH  /v1/messages/{id}        set read=true
  mark-unread      PATCH  /v1/messages/{id}        set read=false
  label            PATCH  /v1/messages/{id}        replace labels
  delete           DELETE /v1/messages/{id}        delete a message
  outbox           GET  /v1/outbox                 pending/failed sends
  retry            POST /v1/outbox/{id}/retry      re-queue a failed send
  cancel           DELETE /v1/outbox/{id}          cancel a pending send
  drafts           GET/POST /v1/drafts             list or create a draft
  draft            GET/PATCH/DELETE /v1/drafts/{id}
  draft-attach     POST /v1/drafts/{id}/attachments multipart upload
  request-send     POST /v1/drafts/{id}/request-send submit draft for approval
  approve          POST /v1/drafts/{id}/approve    approve a frozen draft (Owner)
  reject           POST /v1/drafts/{id}/reject     reject with feedback (Owner)
  cancel-request   POST /v1/drafts/{id}/cancel-send-request
  send-requests    GET  /v1/send-requests          approval requests
  events           GET  /v1/events?after=          one page of event history
  watch            GET  /v1/events/stream          STREAMS one JSON line/event
  wait             GET  /v1/messages/wait          BLOCKS for a new message

realtime:
  watch never returns: it streams until interrupted (Ctrl-C).
  wait blocks until a message arrives or --timeout (max 600s), then returns.

credentials:
  --base/--key, then GATEHOUSE_BASE_URL/GATEHOUSE_API_KEY, then
  ~/.gatehouse/client.json (chmod 600). Never ~/.hermes/.env.

exit codes: 0 ok, 1 error, 2 usage. JSON to stdout, diagnostics to stderr.
"""


class ApiError(Exception):
    """A non-2xx API response or a transport failure."""

    def __init__(self, status, message):
        super().__init__(message)
        self.status = status
        self.message = message


class ClientError(Exception):
    """A local client-side error (missing file, bad invocation)."""


class UsageError(Exception):
    """A command was invoked with invalid or missing arguments."""


class Response(object):
    """A parsed HTTP response."""

    __slots__ = ("status", "headers", "data")

    def __init__(self, status, headers, data):
        self.status = status
        self.headers = headers
        self.data = data


def err(message):
    sys.stderr.write("gatehouse: %s\n" % message)


def resolve_inbox(value):
    if value is None:
        return ""
    return str(value).strip()


# ---------------------------------------------------------------------------
# Credentials
# ---------------------------------------------------------------------------


def load_config_file():
    try:
        os.stat(CONFIG_PATH)
    except FileNotFoundError:
        return {}
    except OSError as exc:
        err("cannot read %s: %s" % (CONFIG_PATH, exc))
        return {}
    try:
        mode = os.stat(CONFIG_PATH).st_mode
    except OSError:
        mode = 0
    if mode & 0o077:
        err("warning: %s is readable by other users; chmod 600 it" % CONFIG_PATH)
    try:
        with open(CONFIG_PATH, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError) as exc:
        err("cannot parse %s: %s" % (CONFIG_PATH, exc))
        return {}
    if not isinstance(data, dict):
        return {}
    return data


def resolve_client(args):
    config = load_config_file()
    base = (
        args.base
        or os.environ.get("GATEHOUSE_BASE_URL")
        or config.get("base_url")
        or config.get("base")
        or DEFAULT_BASE_URL
    )
    key = (
        args.key
        or os.environ.get("GATEHOUSE_API_KEY")
        or config.get("api_key")
        or config.get("key")
        or ""
    )
    inbox = (
        resolve_inbox(getattr(args, "inbox", None))
        or resolve_inbox(os.environ.get("GATEHOUSE_INBOX"))
        or resolve_inbox(config.get("inbox"))
    )
    return Client(base, key, inbox)


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------


def extract_error(exc):
    raw = b""
    try:
        raw = exc.read()
    except Exception:
        raw = b""
    if raw:
        try:
            payload = json.loads(raw.decode("utf-8", "replace"))
            if isinstance(payload, dict) and payload.get("error"):
                return str(payload["error"])
        except ValueError:
            pass
        text = raw.decode("utf-8", "replace").strip()
        if text:
            return text
    return "HTTP %s %s" % (exc.code, exc.reason)


class Client(object):
    def __init__(self, base, key, inbox):
        self.base = (base or DEFAULT_BASE_URL).rstrip("/")
        self.key = key or ""
        self.inbox = inbox or ""

    def _url(self, path, params):
        url = self.base + path
        if params:
            clean = {}
            for name, value in params.items():
                if value is None:
                    continue
                if isinstance(value, (list, tuple)) and not value:
                    continue
                clean[name] = value
            if clean:
                url += "?" + urllib.parse.urlencode(clean, doseq=True)
        return url

    def _request_object(self, method, path, params, headers, data, content_type):
        url = self._url(path, params)
        merged = {"Authorization": "Bearer " + self.key, "Accept": "application/json"}
        if content_type:
            merged["Content-Type"] = content_type
        if headers:
            merged.update(headers)
        return urllib.request.Request(url, data=data, headers=merged, method=method)

    def request(
        self,
        method,
        path,
        params=None,
        body=None,
        headers=None,
        timeout=60,
        data=None,
        content_type=None,
    ):
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            content_type = "application/json"
        request = self._request_object(method, path, params, headers, data, content_type)
        try:
            response = urllib.request.urlopen(request, timeout=timeout)
        except urllib.error.HTTPError as exc:
            raise ApiError(exc.code, extract_error(exc))
        except urllib.error.URLError as exc:
            raise ApiError(0, "connection failed: %s" % exc.reason)
        try:
            status = response.status
            response_headers = response.headers
            raw = response.read()
        finally:
            response.close()
        if status == 204 or not raw:
            parsed = {"ok": True} if status == 204 else None
            return Response(status, response_headers, parsed)
        content = response_headers.get_content_type()
        if content == "application/json":
            try:
                parsed = json.loads(raw.decode("utf-8"))
            except ValueError as exc:
                raise ApiError(status, "invalid JSON in response: %s" % exc)
        else:
            parsed = raw
        return Response(status, response_headers, parsed)

    def stream(self, path, params=None):
        request = self._request_object(
            "GET",
            path,
            params,
            {"Accept": "text/event-stream"},
            None,
            None,
        )
        try:
            return urllib.request.urlopen(request, timeout=None)
        except urllib.error.HTTPError as exc:
            raise ApiError(exc.code, extract_error(exc))
        except urllib.error.URLError as exc:
            raise ApiError(0, "connection failed: %s" % exc.reason)


# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------


def emit(data, table=False, columns=None):
    if table:
        print_table(data, columns)
        return
    if data is None:
        return
    sys.stdout.write(json.dumps(data, indent=2, sort_keys=False, default=str) + "\n")


def stringify(value):
    if value is None:
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (list, tuple)):
        return ", ".join(stringify(item) for item in value)
    if isinstance(value, dict):
        return json.dumps(value, sort_keys=True, separators=(",", ":"), default=str)
    return str(value)


def truncate(text, width=60):
    if len(text) > width:
        return text[: width - 3] + "..."
    return text


def first_value(row, *keys):
    for key in keys:
        value = row.get(key)
        if value not in (None, "", [], {}):
            return value
    return ""


LIST_KEYS = (
    "inboxes",
    "messages",
    "threads",
    "drafts",
    "events",
    "send_requests",
    "items",
    "results",
    "labels",
    "attachments",
)


def extract_rows(data):
    if isinstance(data, list):
        return data
    if isinstance(data, dict):
        for key in LIST_KEYS:
            value = data.get(key)
            if isinstance(value, list):
                return value
    return None


def print_table(data, columns=None):
    rows = extract_rows(data)
    if rows is None:
        if isinstance(data, dict):
            for key, value in data.items():
                sys.stdout.write("%s: %s\n" % (key, stringify(value)))
        elif data is not None:
            sys.stdout.write(stringify(data) + "\n")
        return
    if not rows:
        sys.stdout.write("(no results)\n")
        return
    if columns is None:
        if isinstance(rows[0], dict):
            columns = [(key, key) for key in rows[0].keys()]
        else:
            columns = [("value", None)]
    headers = [column[0] for column in columns]
    matrix = []
    for row in rows:
        if isinstance(row, dict):
            cells = []
            for _, getter in columns:
                if callable(getter):
                    value = getter(row)
                elif getter is None:
                    value = row
                else:
                    value = row.get(getter, "")
                cells.append(truncate(stringify(value)))
        else:
            cells = [truncate(stringify(row))]
        matrix.append(cells)
    widths = [len(header) for header in headers]
    for cells in matrix:
        for index, cell in enumerate(cells):
            if index < len(widths):
                widths[index] = max(widths[index], len(cell))
    sys.stdout.write("  ".join(header.ljust(widths[i]) for i, header in enumerate(headers)).rstrip() + "\n")
    sys.stdout.write("  ".join("-" * widths[i] for i in range(len(headers))) + "\n")
    for cells in matrix:
        line = "  ".join(cells[i].ljust(widths[i]) for i in range(len(headers)))
        sys.stdout.write(line.rstrip() + "\n")


def inbox_columns():
    return [
        ("id", "id"),
        ("address", "address"),
        ("name", "display_name"),
        ("enabled", "enabled"),
    ]


def message_columns():
    return [
        ("id", "id"),
        (
            "from",
            lambda row: (row.get("from") or {}).get("address", "")
            if isinstance(row.get("from"), dict)
            else row.get("from", ""),
        ),
        ("subject", "subject"),
        ("read", "read"),
        ("att", lambda row: "yes" if row.get("has_attachments") else ""),
        ("date", lambda row: first_value(row, "received_at", "created_at", "sent_at")),
        ("labels", "labels"),
    ]


def outbox_columns():
    return [
        ("id", "id"),
        ("subject", "subject"),
        ("to", "to"),
        ("status", "status"),
        ("attempts", "attempts"),
        ("last_error", "last_error"),
        ("next_retry", "next_retry"),
        ("date", lambda row: first_value(row, "created_at", "received_at")),
    ]


def thread_columns():
    return [
        ("id", "id"),
        ("inbox_id", "inbox_id"),
        ("subject", "subject"),
        ("messages", "message_count"),
        ("last_message_at", "last_message_at"),
    ]


def draft_columns():
    return [
        ("id", "id"),
        ("inbox_id", "inbox_id"),
        ("to", "to"),
        ("subject", "subject"),
        ("status", "status"),
        ("updated_at", "updated_at"),
    ]


def event_columns():
    return [
        ("cursor", "cursor"),
        ("type", "type"),
        ("entity_id", "entity_id"),
        ("inbox_id", "inbox_id"),
        ("created_at", "created_at"),
    ]


def send_request_columns():
    return [
        ("id", "id"),
        ("draft_id", "draft_id"),
        ("inbox_id", "inbox_id"),
        ("status", "status"),
        ("delivery_status", "delivery_status"),
        ("approver_email", "approver_email"),
        ("requested_at", "requested_at"),
    ]


def label_columns():
    return [
        ("label", lambda row: row if not isinstance(row, dict) else row.get("label", "")),
    ]


# ---------------------------------------------------------------------------
# Argument helpers
# ---------------------------------------------------------------------------


def split_values(values):
    out = []
    for value in values or []:
        if value is None:
            continue
        for piece in str(value).split(","):
            piece = piece.strip()
            if piece:
                out.append(piece)
    return out


def add_inbox(parser):
    parser.add_argument(
        "--inbox",
        metavar="ID",
        default=argparse.SUPPRESS,
        help="inbox id (overrides the global --inbox; defaults to it)",
    )


def add_limit(parser):
    parser.add_argument(
        "--limit",
        type=int,
        metavar="N",
        default=None,
        help="maximum number of results (server default 100)",
    )


def add_table(parser):
    parser.add_argument(
        "--table",
        action="store_true",
        default=argparse.SUPPRESS,
        help="print a simple columnar table instead of JSON",
    )


def idem_headers(args):
    if getattr(args, "no_idem", False):
        return {}
    key = getattr(args, "idempotency_key", None) or str(uuid.uuid4())
    return {"Idempotency-Key": key}


def add_idem(parser):
    parser.add_argument(
        "--idempotency-key",
        metavar="KEY",
        default=None,
        help="explicit Idempotency-Key header (default: a random UUID)",
    )
    parser.add_argument(
        "--no-idem",
        action="store_true",
        help="do not send an Idempotency-Key header",
    )


def read_file(path):
    try:
        with open(path, "rb") as handle:
            return handle.read()
    except OSError as exc:
        raise ClientError("cannot read %s: %s" % (path, exc))


def load_attachment_json(paths):
    attachments = []
    for path in paths or []:
        content = read_file(path)
        content_type = mimetypes.guess_type(path)[0] or "application/octet-stream"
        attachments.append(
            {
                "filename": os.path.basename(path),
                "content_type": content_type,
                "content": base64.b64encode(content).decode("ascii"),
            }
        )
    return attachments


def load_attachment_files(paths):
    files = []
    for path in paths or []:
        content = read_file(path)
        content_type = mimetypes.guess_type(path)[0] or "application/octet-stream"
        files.append((os.path.basename(path), content_type, content))
    return files


def encode_multipart(files, field="attachments"):
    boundary = "----gatehouse" + uuid.uuid4().hex
    parts = []
    for filename, content_type, content in files:
        safe_name = filename.replace('"', "%22")
        parts.append(("--" + boundary).encode("ascii"))
        parts.append(
            (
                'Content-Disposition: form-data; name="%s"; filename="%s"'
                % (field, safe_name)
            ).encode("utf-8")
        )
        parts.append(("Content-Type: " + content_type).encode("ascii"))
        parts.append(b"")
        parts.append(content)
    parts.append(("--" + boundary + "--").encode("ascii"))
    parts.append(b"")
    body = b"\r\n".join(parts)
    return body, "multipart/form-data; boundary=" + boundary


# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------


def cmd_bootstrap(client, args):
    response = client.request("GET", "/v1/bootstrap")
    emit(response.data, args.table, inbox_columns())
    return EXIT_OK


def cmd_inboxes(client, args):
    response = client.request("GET", "/v1/inboxes")
    emit(response.data, args.table, inbox_columns())
    return EXIT_OK


def cmd_list(client, args):
    params = {
        "inbox": client.inbox,
        "label": split_values(args.label),
        "from": args.from_,
        "to": args.to,
        "unread": "true" if args.unread else None,
        "has_attachment": "true" if args.has_attachment else None,
        "before": args.before,
        "limit": args.limit,
    }
    response = client.request("GET", "/v1/messages", params=params)
    emit(response.data, args.table, message_columns())
    return EXIT_OK


def cmd_get(client, args):
    response = client.request("GET", "/v1/messages/" + args.id)
    emit(response.data, args.table, message_columns())
    return EXIT_OK


def cmd_search(client, args):
    params = {
        "q": args.query,
        "inbox": client.inbox,
        "label": split_values(args.label),
        "from": args.from_,
        "to": args.to,
        "has_attachment": "true" if args.has_attachment else None,
        "limit": args.limit,
    }
    response = client.request("GET", "/v1/search", params=params)
    emit(response.data, args.table, message_columns())
    return EXIT_OK


def cmd_threads(client, args):
    params = {"inbox": client.inbox, "limit": args.limit}
    response = client.request("GET", "/v1/threads", params=params)
    emit(response.data, args.table, thread_columns())
    return EXIT_OK


def cmd_thread(client, args):
    path = "/v1/threads/" + args.id
    if args.messages:
        path += "/messages"
    response = client.request("GET", path)
    emit(response.data, args.table, thread_columns() if args.messages else None)
    return EXIT_OK


def cmd_labels(client, args):
    response = client.request("GET", "/v1/labels")
    emit(response.data, args.table, label_columns())
    return EXIT_OK


def cmd_send(client, args):
    body = {"to": split_values(args.to)}
    cc = split_values(args.cc)
    bcc = split_values(args.bcc)
    if cc:
        body["cc"] = cc
    if bcc:
        body["bcc"] = bcc
    if args.subject is not None:
        body["subject"] = args.subject
    if args.text is not None:
        body["text"] = args.text
    if args.html is not None:
        body["html"] = args.html
    if args.sender:
        body["sender"] = args.sender
    if client.inbox:
        body["inbox_id"] = client.inbox
    if args.attach:
        body["attachments"] = load_attachment_json(args.attach)
    params = {"wait": "true"} if args.wait else None
    response = client.request(
        "POST", "/v1/send", params=params, body=body, headers=idem_headers(args)
    )
    emit(response.data, args.table)
    return EXIT_OK


def cmd_reply(client, args):
    body = {}
    if args.text is not None:
        body["text"] = args.text
    if args.html is not None:
        body["html"] = args.html
    if args.sender:
        body["sender"] = args.sender
    if args.attach:
        body["attachments"] = load_attachment_json(args.attach)
    response = client.request(
        "POST",
        "/v1/messages/" + args.id + "/reply",
        body=body,
        headers=idem_headers(args),
    )
    emit(response.data, args.table)
    return EXIT_OK


def _mark_read(client, args, read):
    response = client.request(
        "PATCH", "/v1/messages/" + args.id, body={"read": read}
    )
    emit(response.data, args.table, message_columns())
    return EXIT_OK


def cmd_mark_read(client, args):
    return _mark_read(client, args, True)


def cmd_mark_unread(client, args):
    return _mark_read(client, args, False)


def cmd_label(client, args):
    if args.clear_labels:
        labels = []
    else:
        labels = split_values(args.labels)
    if not labels and not args.clear_labels:
        raise UsageError("label needs --labels a,b or --clear-labels")
    response = client.request(
        "PATCH", "/v1/messages/" + args.id, body={"labels": labels}
    )
    emit(response.data, args.table, message_columns())
    return EXIT_OK


def cmd_delete(client, args):
    response = client.request("DELETE", "/v1/messages/" + args.id)
    emit(response.data, args.table)
    return EXIT_OK


def cmd_outbox(client, args):
    params = {"inbox": client.inbox, "limit": args.limit}
    response = client.request("GET", "/v1/outbox", params=params)
    emit(response.data, args.table, outbox_columns())
    return EXIT_OK


def cmd_retry(client, args):
    response = client.request("POST", "/v1/outbox/" + args.id + "/retry")
    emit(response.data, args.table)
    return EXIT_OK


def cmd_cancel(client, args):
    response = client.request("DELETE", "/v1/outbox/" + args.id)
    emit(response.data, args.table)
    return EXIT_OK


def _draft_fields(args):
    body = {}
    to = split_values(getattr(args, "to", None))
    cc = split_values(getattr(args, "cc", None))
    bcc = split_values(getattr(args, "bcc", None))
    if to:
        body["to"] = to
    if cc:
        body["cc"] = cc
    if bcc:
        body["bcc"] = bcc
    if getattr(args, "subject", None) is not None:
        body["subject"] = args.subject
    if getattr(args, "text", None) is not None:
        body["text"] = args.text
    if getattr(args, "html", None) is not None:
        body["html"] = args.html
    if getattr(args, "from_addr", None):
        body["from_address"] = args.from_addr
    if getattr(args, "reply_to", None):
        body["reply_to_message_id"] = args.reply_to
    if getattr(args, "attach", None):
        body["attachments"] = load_attachment_json(args.attach)
    return body


def cmd_drafts(client, args):
    write = bool(
        args.create
        or args.action
        or args.to
        or args.cc
        or args.bcc
        or args.subject is not None
        or args.text is not None
        or args.html is not None
        or args.from_addr
        or args.reply_to
        or args.attach
    )
    if write:
        body = _draft_fields(args)
        if client.inbox:
            body["inbox_id"] = client.inbox
        if args.action:
            body["action"] = args.action
        response = client.request(
            "POST", "/v1/drafts", body=body, headers=idem_headers(args)
        )
    else:
        params = {"inbox": client.inbox, "before": args.before, "limit": args.limit}
        response = client.request("GET", "/v1/drafts", params=params)
    emit(response.data, args.table, draft_columns())
    return EXIT_OK


def cmd_draft(client, args):
    path = "/v1/drafts/" + args.id
    if args.delete:
        response = client.request("DELETE", path)
        emit(response.data, args.table)
        return EXIT_OK
    body = _draft_fields(args)
    if args.action:
        body["action"] = args.action
    if body:
        response = client.request(
            "PATCH", path, body=body, headers=idem_headers(args)
        )
    else:
        response = client.request("GET", path)
    emit(response.data, args.table, draft_columns())
    return EXIT_OK


def cmd_draft_attach(client, args):
    path = "/v1/drafts/" + args.id + "/attachments"
    if args.list:
        response = client.request("GET", path)
        emit(response.data, args.table)
        return EXIT_OK
    if not args.attach:
        raise UsageError("draft-attach needs one or more --attach FILE (or --list)")
    body, content_type = encode_multipart(load_attachment_files(args.attach))
    response = client.request("POST", path, data=body, content_type=content_type)
    emit(response.data, args.table)
    return EXIT_OK


def cmd_request_send(client, args):
    body = {"external": True} if args.external else None
    response = client.request(
        "POST",
        "/v1/drafts/" + args.id + "/request-send",
        body=body,
        headers=idem_headers(args),
    )
    emit(response.data, args.table, draft_columns())
    return EXIT_OK


def cmd_approve(client, args):
    body = {"feedback": args.feedback} if args.feedback is not None else None
    response = client.request(
        "POST",
        "/v1/drafts/" + args.id + "/approve",
        body=body,
        headers=idem_headers(args),
    )
    emit(response.data, args.table)
    return EXIT_OK


def cmd_reject(client, args):
    body = {"feedback": args.feedback} if args.feedback is not None else None
    response = client.request("POST", "/v1/drafts/" + args.id + "/reject", body=body)
    emit(response.data, args.table, draft_columns())
    return EXIT_OK


def cmd_cancel_request(client, args):
    response = client.request(
        "POST", "/v1/drafts/" + args.id + "/cancel-send-request"
    )
    emit(response.data, args.table, draft_columns())
    return EXIT_OK


def cmd_send_requests(client, args):
    params = {
        "inbox": client.inbox,
        "active": "true" if args.active else None,
        "limit": args.limit,
    }
    response = client.request("GET", "/v1/send-requests", params=params)
    emit(response.data, args.table, send_request_columns())
    return EXIT_OK


def cmd_events(client, args):
    params = {"after": args.after, "inbox": client.inbox, "limit": args.limit}
    response = client.request("GET", "/v1/events", params=params)
    emit(response.data, args.table, event_columns())
    return EXIT_OK


def emit_event(data_lines):
    payload = "\n".join(data_lines)
    try:
        line = json.dumps(json.loads(payload), separators=(",", ":"), default=str)
    except ValueError:
        line = payload
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def cmd_watch(client, args):
    params = {"after": args.after, "inbox": client.inbox}
    stream = client.stream("/v1/events/stream", params)
    data_lines = []
    try:
        for raw in stream:
            try:
                chunk = raw.decode("utf-8")
            except UnicodeDecodeError:
                continue
            # Real urllib iteration yields one line per item, but be tolerant of
            # transports (or tests) that hand back several lines at once.
            for line in chunk.split("\n"):
                line = line.rstrip("\r")
                if line == "":
                    if data_lines:
                        emit_event(data_lines)
                        data_lines = []
                    continue
                if line.startswith(":"):
                    continue
                field, _, value = line.partition(":")
                if value.startswith(" "):
                    value = value[1:]
                if field == "data":
                    data_lines.append(value)
    finally:
        stream.close()
        if data_lines:
            emit_event(data_lines)
    return EXIT_OK


def cmd_wait(client, args):
    params = {"inbox": client.inbox, "after": args.after, "timeout": args.timeout}
    response = client.request(
        "GET", "/v1/messages/wait", params=params, timeout=args.timeout + 15
    )
    if response.status == 204:
        err("no new message before timeout (%ss)" % args.timeout)
        return EXIT_OK
    emit(response.data, args.table, message_columns())
    return EXIT_OK


# ---------------------------------------------------------------------------
# Parser
# ---------------------------------------------------------------------------


def build_parser():
    parser = argparse.ArgumentParser(
        prog="gatehouse",
        description=(
            "Gatehouse Mail agent client. JSON to stdout, diagnostics to stderr."
        ),
        epilog=EPILOG,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument(
        "--base",
        metavar="URL",
        default=None,
        help=(
            "API base URL (default: $GATEHOUSE_BASE_URL, then "
            "~/.gatehouse/client.json, then %s)" % DEFAULT_BASE_URL
        ),
    )
    parser.add_argument(
        "--key",
        metavar="KEY",
        default=None,
        help="API key (default: $GATEHOUSE_API_KEY, then ~/.gatehouse/client.json)",
    )
    parser.add_argument(
        "--inbox",
        metavar="ID",
        default=None,
        help="default inbox id for commands that use one",
    )
    parser.add_argument(
        "--table",
        action="store_true",
        help="print a simple columnar table instead of JSON",
    )
    parser.add_argument(
        "--version",
        action="version",
        version="gatehouse.py 1.0",
    )
    subparsers = parser.add_subparsers(dest="command", metavar="<command>")
    subparsers.required = True

    def command(name, help_text, handler):
        sub = subparsers.add_parser(
            name,
            help=help_text,
            description=help_text,
            formatter_class=argparse.RawDescriptionHelpFormatter,
        )
        sub.set_defaults(func=handler)
        add_table(sub)
        return sub

    command("bootstrap", "GET /v1/bootstrap - account, roles and inboxes.", cmd_bootstrap)
    command("inboxes", "GET /v1/inboxes - list accessible inboxes.", cmd_inboxes)

    p = command(
        "list",
        "GET /v1/messages - list messages with filters.",
        cmd_list,
    )
    add_inbox(p)
    p.add_argument(
        "--label",
        action="append",
        metavar="LABEL",
        help="only messages carrying this label; repeatable or comma-separated "
        "(messages must carry all listed labels)",
    )
    p.add_argument("--from", dest="from_", metavar="ADDR", help="filter by sender address")
    p.add_argument("--to", metavar="ADDR", help="filter by recipient address")
    p.add_argument("--unread", action="store_true", help="only unread messages")
    p.add_argument(
        "--has-attachment", action="store_true", help="only messages with attachments"
    )
    p.add_argument("--before", metavar="ID", help="keyset pagination: older than this id")
    add_limit(p)

    p = command("get", "GET /v1/messages/{id} - one message.", cmd_get)
    p.add_argument("id", metavar="ID", help="message id")

    p = command(
        "search",
        "GET /v1/search?q= - FTS5 full-text search.",
        cmd_search,
    )
    p.add_argument("query", metavar="QUERY", help="search text")
    add_inbox(p)
    p.add_argument(
        "--label",
        action="append",
        metavar="LABEL",
        help="filter by label; repeatable or comma-separated",
    )
    p.add_argument("--from", dest="from_", metavar="ADDR", help="filter by sender address")
    p.add_argument("--to", metavar="ADDR", help="filter by recipient address")
    p.add_argument(
        "--has-attachment", action="store_true", help="only messages with attachments"
    )
    add_limit(p)

    p = command("threads", "GET /v1/threads - list threads.", cmd_threads)
    add_inbox(p)
    add_limit(p)

    p = command(
        "thread",
        "GET /v1/threads/{id} - thread detail; --messages lists its messages.",
        cmd_thread,
    )
    p.add_argument("id", metavar="ID", help="thread id")
    p.add_argument(
        "--messages",
        action="store_true",
        help="list the thread's messages instead of the thread summary",
    )

    command("labels", "GET /v1/labels - distinct labels currently in use.", cmd_labels)

    p = command(
        "send",
        "POST /v1/send - send as an Owner; enqueues and returns unless --wait.",
        cmd_send,
    )
    add_inbox(p)
    p.add_argument(
        "--to",
        action="append",
        required=True,
        metavar="ADDR",
        help="recipient; repeatable or comma-separated (required)",
    )
    p.add_argument(
        "--cc", action="append", metavar="ADDR", help="cc; repeatable or comma-separated"
    )
    p.add_argument(
        "--bcc", action="append", metavar="ADDR", help="bcc; repeatable or comma-separated"
    )
    p.add_argument("--subject", metavar="TEXT", default=None, help="subject line")
    p.add_argument("--text", metavar="TEXT", default=None, help="plain-text body")
    p.add_argument("--html", metavar="HTML", default=None, help="HTML body")
    p.add_argument(
        "--attach",
        action="append",
        metavar="FILE",
        help="attach a file (repeatable); base64-encoded into the JSON body",
    )
    p.add_argument(
        "--sender",
        metavar="ADDR",
        help="From identity: the inbox primary or one of its aliases",
    )
    p.add_argument(
        "--wait",
        action="store_true",
        help="block until delivery instead of returning as soon as it is queued",
    )
    add_idem(p)

    p = command(
        "reply",
        "POST /v1/messages/{id}/reply - reply from the message's inbox (Owner).",
        cmd_reply,
    )
    p.add_argument("id", metavar="ID", help="message id to reply to")
    p.add_argument("--text", metavar="TEXT", default=None, help="plain-text body")
    p.add_argument("--html", metavar="HTML", default=None, help="HTML body")
    p.add_argument(
        "--attach",
        action="append",
        metavar="FILE",
        help="attach a file (repeatable); base64-encoded into the JSON body",
    )
    p.add_argument(
        "--sender",
        metavar="ADDR",
        help="From identity: the inbox primary or one of its aliases",
    )
    add_idem(p)

    p = command(
        "mark-read",
        "PATCH /v1/messages/{id} - set read=true.",
        cmd_mark_read,
    )
    p.add_argument("id", metavar="ID", help="message id")

    p = command(
        "mark-unread",
        "PATCH /v1/messages/{id} - set read=false.",
        cmd_mark_unread,
    )
    p.add_argument("id", metavar="ID", help="message id")

    p = command(
        "label",
        "PATCH /v1/messages/{id} - replace the label set.",
        cmd_label,
    )
    p.add_argument("id", metavar="ID", help="message id")
    p.add_argument(
        "--labels",
        action="append",
        metavar="LABEL",
        help="new label set; repeatable or comma-separated (replaces the set)",
    )
    p.add_argument(
        "--clear-labels", action="store_true", help="remove every label from the message"
    )

    p = command("delete", "DELETE /v1/messages/{id} - delete a message.", cmd_delete)
    p.add_argument("id", metavar="ID", help="message id")

    p = command(
        "outbox",
        "GET /v1/outbox - pending and failed outbound messages.",
        cmd_outbox,
    )
    add_inbox(p)
    add_limit(p)

    p = command(
        "retry",
        "POST /v1/outbox/{id}/retry - re-queue a failed send.",
        cmd_retry,
    )
    p.add_argument("id", metavar="ID", help="outbox message id")

    p = command(
        "cancel",
        "DELETE /v1/outbox/{id} - cancel a pending send or discard a failed one.",
        cmd_cancel,
    )
    p.add_argument("id", metavar="ID", help="outbox message id")

    p = command(
        "drafts",
        "GET/POST /v1/drafts - list drafts, or create one when write flags are given.",
        cmd_drafts,
    )
    add_inbox(p)
    p.add_argument(
        "--create", action="store_true", help="force draft creation even with no fields"
    )
    p.add_argument("--before", metavar="ID", help="list: keyset pagination cursor")
    add_limit(p)
    p.add_argument(
        "--to", action="append", metavar="ADDR", help="write: recipients (repeatable/CSV)"
    )
    p.add_argument(
        "--cc", action="append", metavar="ADDR", help="write: cc recipients (repeatable/CSV)"
    )
    p.add_argument(
        "--bcc", action="append", metavar="ADDR", help="write: bcc recipients (repeatable/CSV)"
    )
    p.add_argument("--subject", metavar="TEXT", default=None, help="write: subject")
    p.add_argument("--text", metavar="TEXT", default=None, help="write: plain-text body")
    p.add_argument("--html", metavar="HTML", default=None, help="write: HTML body")
    p.add_argument(
        "--from",
        dest="from_addr",
        metavar="ADDR",
        help="write: From identity (primary or an alias)",
    )
    p.add_argument(
        "--reply-to", metavar="MSG_ID", help="write: reply-to message id"
    )
    p.add_argument(
        "--attach",
        action="append",
        metavar="FILE",
        help="write: attach a file (repeatable); base64-encoded into the JSON body",
    )
    p.add_argument(
        "--action",
        choices=["draft", "request-send", "send"],
        default=None,
        help="write: after saving, leave as draft (default), request approval, or send",
    )
    add_idem(p)

    p = command(
        "draft",
        "GET/PATCH/DELETE /v1/drafts/{id} - read, edit or delete a draft.",
        cmd_draft,
    )
    p.add_argument("id", metavar="ID", help="draft id")
    p.add_argument("--delete", action="store_true", help="delete the draft")
    p.add_argument(
        "--to", action="append", metavar="ADDR", help="patch: recipients (repeatable/CSV)"
    )
    p.add_argument(
        "--cc", action="append", metavar="ADDR", help="patch: cc recipients (repeatable/CSV)"
    )
    p.add_argument(
        "--bcc", action="append", metavar="ADDR", help="patch: bcc recipients (repeatable/CSV)"
    )
    p.add_argument("--subject", metavar="TEXT", default=None, help="patch: subject")
    p.add_argument("--text", metavar="TEXT", default=None, help="patch: plain-text body")
    p.add_argument("--html", metavar="HTML", default=None, help="patch: HTML body")
    p.add_argument(
        "--from",
        dest="from_addr",
        metavar="ADDR",
        help="patch: From identity (primary or an alias)",
    )
    p.add_argument(
        "--reply-to", metavar="MSG_ID", help="patch: reply-to message id"
    )
    p.add_argument(
        "--attach",
        action="append",
        metavar="FILE",
        help="patch: attach a file (repeatable); base64-encoded into the JSON body",
    )
    p.add_argument(
        "--action",
        choices=["draft", "request-send", "send"],
        default=None,
        help="patch: after saving, leave as draft, request approval, or send",
    )
    add_idem(p)

    p = command(
        "draft-attach",
        "POST /v1/drafts/{id}/attachments - multipart upload; field 'attachments'.",
        cmd_draft_attach,
    )
    p.add_argument("id", metavar="ID", help="draft id")
    p.add_argument(
        "--attach",
        action="append",
        metavar="FILE",
        help="file to upload (repeatable); sent as multipart field 'attachments'",
    )
    p.add_argument("--list", action="store_true", help="list attachments instead of uploading")

    p = command(
        "request-send",
        "POST /v1/drafts/{id}/request-send - submit a draft for approval (Assistant).",
        cmd_request_send,
    )
    p.add_argument("id", metavar="ID", help="draft id")
    p.add_argument(
        "--external",
        action="store_true",
        help="request email approval (errors if the inbox has no approver configured)",
    )
    add_idem(p)

    p = command(
        "approve",
        "POST /v1/drafts/{id}/approve - approve and enqueue a frozen draft (Owner).",
        cmd_approve,
    )
    p.add_argument("id", metavar="ID", help="draft id")
    p.add_argument("--feedback", metavar="TEXT", default=None, help="optional feedback")
    add_idem(p)

    p = command(
        "reject",
        "POST /v1/drafts/{id}/reject - reject a pending request (Owner).",
        cmd_reject,
    )
    p.add_argument("id", metavar="ID", help="draft id")
    p.add_argument("--feedback", metavar="TEXT", default=None, help="optional feedback")

    p = command(
        "cancel-request",
        "POST /v1/drafts/{id}/cancel-send-request - withdraw a pending request.",
        cmd_cancel_request,
    )
    p.add_argument("id", metavar="ID", help="draft id")

    p = command(
        "send-requests",
        "GET /v1/send-requests - list draft approval requests.",
        cmd_send_requests,
    )
    add_inbox(p)
    p.add_argument("--active", action="store_true", help="only outstanding requests")
    add_limit(p)

    p = command(
        "events",
        "GET /v1/events?after= - one page of durable event history.",
        cmd_events,
    )
    p.add_argument("--after", metavar="CURSOR", help="return events after this cursor")
    add_inbox(p)
    add_limit(p)

    p = command(
        "watch",
        "GET /v1/events/stream - STREAM one JSON line per event until interrupted.",
        cmd_watch,
    )
    p.add_argument("--after", metavar="CURSOR", help="start after this cursor")
    add_inbox(p)

    p = command(
        "wait",
        "GET /v1/messages/wait - BLOCK for a new message or until --timeout.",
        cmd_wait,
    )
    add_inbox(p)
    p.add_argument("--after", metavar="CURSOR", help="start after this event cursor")
    p.add_argument(
        "--timeout",
        type=int,
        default=60,
        metavar="SECONDS",
        help="maximum seconds to block (default 60, server cap 600)",
    )

    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        client = resolve_client(args)
    except ClientError as exc:
        err(str(exc))
        return EXIT_ERROR
    if not client.key:
        err(
            "no API key: pass --key, set GATEHOUSE_API_KEY, or write "
            "~/.gatehouse/client.json"
        )
        return EXIT_ERROR
    try:
        return args.func(client, args)
    except ApiError as exc:
        err(exc.message)
        return EXIT_ERROR
    except ClientError as exc:
        err(str(exc))
        return EXIT_ERROR
    except UsageError as exc:
        err(str(exc))
        parser.print_usage(sys.stderr)
        return EXIT_USAGE
    except BrokenPipeError:
        try:
            sys.stdout.close()
        except Exception:
            pass
        return EXIT_OK
    except KeyboardInterrupt:
        return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
