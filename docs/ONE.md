# One / Gmail compatibility gate

**Status: implemented locally; not yet released or deployed.** The
connector is the fourth choice under **Add inbox → Standalone inbox**,
alongside Google / Gmail, Microsoft / Outlook and IMAP + SMTP. It will reuse
Gmail mailbox workflows, not create a separate inbox or agent API.

Do not expose a fully functional connector until the mandatory live gate passes.
The probe below is isolated from the MailMoose database, application endpoints,
normal outbox and deployed image. It does not import any mailbox or change an
existing provider. No new dependencies or services are required.

## Developer-owned live check

Use your own One personal account with a disposable connected Gmail mailbox.
Connect Gmail at <https://app.withone.ai/> under **Connected Apps**, then create a
standard REST key under **Developers → API Keys**. Requests and Gmail data pass
through One's cloud: this route is not fully self-hosted.

From the repository root, read the key without placing it in shell history:

```bash
read -rsp 'One API key: ' ONE_API_KEY
export ONE_API_KEY
./mailmoose-go.sh run ./cmd/oneprobe
unset ONE_API_KEY
```

The default invocation is read-only. If more than one usable connection exists,
set `ONE_CONNECTION_ID` to the UUID selected in your One account; the tool never
guesses which mailbox to use. It resolves the opaque passthrough connection key
from the vault. UUID and connection key are not interchangeable.

To explicitly allow creation, readback, update and deletion of **one
probe-owned disposable draft**, add `-drafts`. The draft contains a generated
attachment with synthetic bytes. Only this draft is deleted; existing drafts,
messages and upstream connections are not deleted or revoked. The tool also
fetches the attachment and checks byte equality, then drains history from the
pre-write cursor. An unknown creation outcome is never automatically retried.

```bash
./mailmoose-go.sh run ./cmd/oneprobe -drafts
```

Sending is a separate manual approval. Obtain human approval for the **exact
recipient and content** before setting `ONE_PROBE_SEND_TO` and using
`-drafts -send-approved`. The send fixture is:

- Subject: `MailMoose One compatibility probe — approved test send`
- Body: `MailMoose One compatibility probe. Disposable test data only.`
- Attachment: none.
- Sender: the actual connected Gmail profile address.

The probe submits one message, then verifies its provider ID is visible with the
`SENT` label. It never retries the send and never deletes the sent message.
This is explicit developer tooling, **not** an application approval bypass;
the eventual connector must pass through existing MailMoose approval gates.

All Go commands use Docker through `mailmoose-go.sh`. The wrapper forwards the
three One variables **only** for `run ./cmd/oneprobe`, by environment variable
name, not by placing credential values in process arguments or log filenames.
Unset the variables afterwards. Never put live keys in fixtures or CI.

## Output and safety

The JSON report records operation, method, canonical path template, discovered
action `systemId`, HTTP status, latency and selected quota headers. It excludes
keys, connection selectors, personal addresses, resource IDs, cursors and email
content. Normal wrapper logs under `tests/logs/mailmoose-go/` therefore contain
only this safe report. Raw provider error messages are not retained.

- Requests have a fixed `https://api.withone.ai` origin and 30-second timeout.
- Redirects are not followed, including redirects on the same host.
- Response bodies are bounded to 4 MiB for these small test fixtures.
- Requests are paced at 15/minute, including discovery and action metadata.
- Actions match exact Gmail HTTP method and path template; missing/duplicate
  mappings stop the probe. Higher-level One convenience actions are not used.
- No automatic retries occur, including after 429; `Retry-After` is captured.
- A `cleanup_required` result means a disposable draft may remain. Inspect
  Gmail manually before repeating; never assume an interrupted write failed.
- `release_gate: false` means the full gate has **not** passed, even if all
  reads succeeded. An invocation without approved sending cannot pass it.

The probe discovers all required action mappings on every run, including
labels CRUD, modification, trash/untrash and draft-send metadata. Catalogue
presence alone does not prove write permissions. It executes only the documented
read checks, the opted-in draft/attachment lifecycle and the approved
`messages.send` check. Mutation behaviours still need connector fixture and
regression coverage; this tool is not a claim that all catalogue operations have
been exercised live.

## Observations from read-only preflight (2026-10-11)

Personal-scope vault discovery returned one active, operational Gmail connection
with environment **`live`**, a UUID and opaque connection key. Its vault identity
was not an email address and did not match the profile email. Always resolve and
verify the actual mailbox through `users.getProfile`, even when vault identity
is present.

Native passthrough paths worked without encoding the whole path as one segment:
`/v1/passthrough/gmail/v1/users/me/...`. All successful checks below returned
HTTP 200. Provider action IDs are observations, **not** permanent constants;
re-discover by method/path before use.

| Operation | Canonical upstream path | Observed action systemId |
|---|---|---|
| Profile | `GET /gmail/v1/users/{{userId}}/profile` | `conn_mod_def::GJ3oa8bJZNA::ZS9kXmE_Rzi4utfESor_ng` |
| List messages / next page | `GET /gmail/v1/users/{{userId}}/messages` | `conn_mod_def::GJ3odOE-fdw::ijLww5s-SCSplLQtLpxkrw` |
| Get full / raw MIME | `GET /gmail/v1/users/{{userId}}/messages/{{id}}` | `conn_mod_def::GJ3ocvMGOS8::D__3BgQSSzWtDUoOqLuX2A` |
| History | `GET /gmail/v1/users/{{userId}}/history` | `conn_mod_def::GJ3obKaXB1U::JWUEs9nESz2__4shMOpyIg` |
| Labels list | `GET /gmail/v1/users/{{userId}}/labels` | `conn_mod_def::GJ3obtut_gA::L4R17Zu8QL-awsrk5_4b8w` |
| Drafts list | `GET /gmail/v1/users/{{userId}}/drafts` | `conn_mod_def::GJ3oamEcU-s::uNEB0VuLRKyJNVX530aUHA` |

The subsequent Go `-drafts` run also passed these live checks:

| Operation | Canonical upstream path | Observed action systemId | Result |
|---|---|---|---|
| Create disposable draft | `POST /gmail/v1/users/{{userId}}/drafts` | `conn_mod_def::GJ3oaMOtsBE::KznPt5lMTxmqAv5VYAqxwQ` | HTTP 200 |
| Get draft raw / full / updated | `GET /gmail/v1/users/{{userId}}/drafts/{{id}}` | `conn_mod_def::GJ3oadCawlA::wsmeD4FhT4qWwCaA-xUuog` | HTTP 200; MIME readback verified |
| Fetch disposable attachment | `GET /gmail/v1/users/{{userId}}/messages/{{messageId}}/attachments/{{id}}` | `conn_mod_def::GJ3ocG2ED_w::LGrrJyM-QFmKBaHwvMWwzQ` | HTTP 200; 87,000 bytes matched exactly |
| Update disposable draft | `PUT /gmail/v1/users/{{userId}}/drafts/{{id}}` | `conn_mod_def::GJ3oa1OxwjE::08EvDsLsTSSJ6QhfdP_gdA` | HTTP 200; updated subject read back |
| Delete disposable draft | `DELETE /gmail/v1/users/{{userId}}/drafts/{{id}}` | `conn_mod_def::GJ3oaUdVmW4::w5IE55OoRc2lm6sylOqk5Q` | HTTP 204; no cleanup outstanding |
| History after writes | `GET /gmail/v1/users/{{userId}}/history` | `conn_mod_def::GJ3obKaXB1U::JWUEs9nESz2__4shMOpyIg` | HTTP 200; six pages drained with `maxResults=1` |

**Approved sending passed on 2026-10-11.** One `messages.send` request returned
HTTP 200; its returned message was read back with the `SENT` label. The full
live gate returned `release_gate: true`, with no cleanup outstanding.
The send action was `conn_mod_def::GJ3odhCpd3I::gujvYoneSk6NFWltse9bGg`.
The redacted run report is retained locally under
`tests/logs/mailmoose-go/20261011T160410-go-run_._cmd_oneprobe_-drafts_-send-approved-1945347.log`.
That log is gitignored; this matrix records the relevant evidence without
publishing credentials or mailbox data.

Decoded raw MIME matched full-message provider/thread IDs and Message-ID header.
Successful Gmail passthrough calls took approximately 0.47–0.54 seconds in this
small sample. An expired cursor returned native Gmail HTTP 404 with `error.code`,
`error.status` and reason, whereas a missing One route returned HTTP 404 with
`key`, `type`, `status` and `correlationId`. A One gateway 404 must not trigger a
Gmail history reset.

The current free plan advertises 1M requests/month and **20/minute per One
account across all keys**. Observed headers had different limit values; do not
increase pacing or assume a free/paid tier from those headers. No deliberate
live quota-exhaustion test has been performed. Keep 429/backoff fixture coverage
separate from any optional, explicitly approved quota-exhaustion experiment.

## Fit to the existing architecture

The current Gmail connector stores metadata locally and fetches bodies and
attachments live (`GOOGLE.md`), not a durable full-email archive. Preserve that
behaviour for One. Existing mailbox roles, search, local drafts, RemoteDraft
handoff, events and approval/outbox flows remain authoritative.

Both request paths in `internal/transport/gmail` now share an injected execution
boundary: ordinary JSON operations and streamed MIME uploads. The ordinary
outbox resolves a live bound client immediately before submission. Queued
sending configuration contains only a One-binding marker, never the API key.

Migration 066 copies existing Gmail sync cursors into `inbox_gmail_sync`, leaving
native OAuth grants intact. Migration 067 moves One keys into each inbox's
connection row and removes the shared credential table. Existing ciphertext is
copied with its original AAD so the SQL upgrade does not need the root encryption
key; replacement keys use inbox-specific AAD. Connection selectors remain
inbox-encrypted. No fake Google credentials are created.

One history processing uses bounded batches and persisted metadata-only page
snapshots/offsets. Backfill uses ten-message pages with persisted snapshots and
per-item offsets. An interrupted pass resumes without repeatedly spending the
quota on the first messages. Native Google retains its existing batch sizes.

## Connector setup and operation

Choose **Add inbox → Standalone inbox → One (Easy Gmail Setup)**, or open
`/ui/connections/one` as a signed-in account administrator. Save your personal
One REST key, then check only the Gmail mailboxes to import. **Next** automatically
validates the key and starts an encrypted, user-scoped setup lasting 30 minutes.
The next wizard page shows mailbox selection only, with **Cancel** and
**Refresh** on the left and **Import selected** on the right. Key entry is
hidden and empty imported-mailbox sections are not displayed. Successful import
shows a completion panel with the imported inbox names, addresses and **Open
inbox** links, plus a right-aligned **Close** button back to the dashboard.
API keys are visible during entry only; saved credentials are never redisplayed.
Successful multi-import creates an independently encrypted key and connection
for every selected inbox, then removes the temporary setup. Partial failures
retain the setup for retry until expiry. There is no global One configuration.
Discovery verifies actual Gmail profile identities rather than trusting
opaque vault identities. Inactive connections and unsupported platforms are
visible but cannot be imported. Duplicate addresses require explicit migration;
they never replace an existing native connector.

Every imported mailbox uses the normal mailbox UI, canonical APIs, roles,
metadata cache, live MIME/attachments, RemoteDraft handoff and sending gates.
Inboxes imported with the same key share conservative 15/minute pacing,
with user operations ahead of background work. 429 imposes an account cooldown;
unknown send outcomes are never automatically retried. Polling defaults to five
minutes and is configurable from two to thirty minutes. One has no push setup.

Each inbox's connection settings expose **Save**, sync status, polling and
**Delete connection**. Saving replaces or reconnects only that inbox, with
automatic validation of its original connection UUID, Gmail identity and action
map. Deletion clears only that inbox's credentials, never its siblings' keys.
New imports resolve the configured Gmail sender display name through the optional
native `GET /gmail/v1/users/{{userId}}/settings/sendAs` action, matching the
verified profile address rather than another alias. If One lacks the action or
Gmail settings permission, or Gmail returns an empty inherited name, up to three
recent Sent-message metadata headers are checked for a named From address matching
the verified mailbox. No bodies are downloaded. With no matching name, the address
is the fallback, never a provider-suffixed name. Existing automatic names are refreshed during reconnect or the next
connection revalidation; custom inbox names are preserved.
A non-secret disconnected marker prevents fallback to IMAP; local metadata and
drafts remain. Nothing is revoked or deleted upstream. Live bodies/attachments
require an active connection. There are no separate test buttons or account-wide
removal controls. Key rotation retains the imported quota group; One's actual
quota is account-wide even when different keys are used.
Protect and back up `APP_ENCRYPTION_KEY` separately from `/data`.

## Deterministic tests

```bash
./mailmoose-go.sh test -race -count=1 -timeout=60s ./tests/unit/oneprobe ./cmd/oneprobe
./mailmoose-go.sh test -race -count=1 -timeout=60s ./tests/unit/one
./mailmoose-go.sh test -race -count=1 -timeout=60s ./tests/unit/httpapp -run TestOne
./mailmoose-go.sh vet ./...
./mailmoose-go.sh gofmt -l cmd internal tests dialmx
```

Tests use a local fixture HTTP server and injected clock; never live keys.
They cover native read schemas, pagination, exact action matching, draft
lifecycle/attachment equality, explicit send opt-in and Sent visibility,
identity mismatch, redaction, size bounds, redirects, safe error/429 handling
and cancellation. Connector fixtures additionally cover native streamed sends
and drafts, fixed-host/allowlist enforcement, action ambiguity, gateway versus
Gmail errors, account cooldown/cancellation/priority, selective idempotent import,
encrypted storage, scoped mailbox operations, ordinary outbox delivery, durable
history resume, per-inbox replacement/deletion isolation, multi-import credential
fan-out, upgrade preservation, fourth-provider UI and CSRF.

References: [Vault](https://www.withone.ai/docs/api-reference/vault/list_vault_connections),
[Actions](https://www.withone.ai/docs/api-reference/actions/list_actions),
[Passthrough](https://www.withone.ai/docs/api-reference/passthrough/passthrough),
[Pricing](https://www.withone.ai/pricing).
