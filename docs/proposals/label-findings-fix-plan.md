# Plan — fix the two label-system findings

Source findings come from the labels test run on 2026-10-13 (98 assertions, 1 known
limitation). Investigation for this plan was **read-only**; no code changed.

- Finding 1 (functional gap): a message moved into a custom folder by a folder-backed label
  cannot be moved back out through the API.
- Finding 2 (defect): `GET /v1/search` without a text term stalls at the 45 s client timeout.

Evidence, file:line anchors and exact repro commands are in "Evidence" below.

---

## Finding 2 first — it is a one-line fix and it invalidates part of the old diagnosis

The earlier note ("the account-wide merge search is slow without `q=`") is **wrong and must be
corrected**. Live measurement on the public host:

```
45.04s    0  /v1/search                     (timeout)
45.06s    0  /v1/search?label=test          (timeout)
45.02s    0  /v1/search?q=x                 (timeout)   <- even with a text term
 9.84s  200  /v1/search?q=x&label=test      items=0
45.06s    0  /v1/search?label=test&limit=5  (timeout)
45.05s    0  /v1/search?q=&label=test       (timeout)
45.05s    0  /v1/search?folder=Inbox&label=test (timeout)
12.77s  200  /v1/search?unread=true&label=test  items=0
 0.07s  200  /v1/search?inbox=in_06g9pe441nr9gmm6yjzz9afygg&label=test  items=0
 0.03s  200  /v1/inboxes/in_06g9pe441nr9gmm6yjzz9afygg/messages?label=test  items=0
```

Read the pattern: the requests that succeed all carry a **short-circuit filter** — `inbox=`
(scopes the fan-out to one inbox, `readableInboxes` still returns the whole set but the store query
and remote selection narrow) and `unread=true` (a filter the *remote* leg can express). The requests
that stall all have **local-only or absent filters**, so the merge loop walks every configured
remote inbox.

### Root cause

`mergeSearch` (internal/httpapp/mailbox_merge.go:413) fans out serially over every routed remote
mailbox:

```go
for _, mb := range boxes {                       // :446
    if !mb.routed || !mb.remoteConfigured() { continue }
    ...
    for scan := 0; scan < remoteFilterScanMaxPages && collected <= limit; scan++ {   // :483
        res, e := mb.remote.SearchRemote(ctx, p, mb.inbox.ID, app.RemoteSearchQuery{...})
```

- `remoteFilterScanPage = 50` (api.go:662) and `remoteSearchDefaultLimit = 200`
  (remote_mailbox_more.go:153) — the scan can iterate up to `remoteFilterScanMaxPages = 20`
  (api.go:670) pages **per inbox**, and each page is a live IMAP `Search` plus a per-UID metadata
  hydration (`viewsForUIDs`, remote_mailbox_more.go:~215) that *fetches live headers for any UID
  with no cached row* (`// A UID with no cached row is fetched live in one header pass so a newly
  arrived message is still visible`).
- The filter that makes a request fast is which **remote** leg it can satisfy. `has_attachment` is
  applied in-memory only (`// The has_attachment filter is applied in-memory`) and `label` on a
  local/domain inbox is a local-store concept, so the remote legs cannot narrow and the scan runs to
  its page budget.
- The request has **no envelope-level deadline**. `ctx` is `r.Context()`, so a client that times out
  leaves the server scanning; nothing bounds the loop, so the caller sees a client-side 45 s abort
  (score `0` in the table is `urlopen` timeout, not an HTTP status) with no `errors[]` entry, no
  partial results and no way to tell "still working" from "broken".

The correlated `label` + remote-fan-out shape is what makes this look label-specific.

### Fix 2 — bound and narrow the fan-out, and never leave the caller empty-handed

Three small, independent changes (each shippable alone; 2a alone converts the stall into a usable
partial answer):

**2a. Per-search budget in `mergeSearch`.** Wrap the remote loops in a deadline derived from the
request, e.g. a new package var `remoteSearchBudget = 8 * time.Second` (mirroring the
`remoteFilterScanPage`/`remoteFilterScanMaxPages` test-seam pattern at api.go:662–683):

```go
budgetCtx, cancel := context.WithTimeout(ctx, remoteSearchBudget)
defer cancel()
for _, mb := range boxes {
    if budgetCtx.Err() != nil {
        failures = append(failures, model.NewInboxFailure(mb.inbox.ID, errSearchBudgetExhausted))
        completeness = model.CompletenessPartial
        continue                       // keep the inbox in the envelope as a failure, never silent
    }
    ...existing loop...
}
```

Inbox loops that do not finish inside the budget report as failures, so the envelope stays honest
(`completeness: partial` + an `errors[]` entry) instead of hanging. `pageMerged` already turns
partial completeness + failures into partial results (mailbox_merge.go:552–555).

**2b. Make the slow path cheap: skip the live-header hydration in a *filtered scan*.** When the
request is a filter-only scan (no `q`), the caller only needs matches, not fresh bodies. Add a
`SkipHydration bool` to `app.RemoteSearchQuery` and pass it from `mergeSearch` when `q == ""`; in
`SearchRemote` skip the "UID with no cached row" fetch for those UIDs (drop uncached UIDs and mark
the result `CompletenessPartial` rather than paying a header fetch per message). This removes the
dominant cost of a broad scan without changing correctness — completness honestly degrades to
partial for uncached items only.

**2c. Fail fast in the *local store* instead of scanning.** `SearchMessagesFiltered`
(internal/store/messages.go:1580) already short-circuits `q == ""` to `nil` — so with no text term
the local leg returns instantly and the whole cost is remote, which is exactly what 2a/2b address.
No change needed here; recorded so the plan reflects the verified split.

**Tests (2):**

- Unit: `mergeSearch` with a stubbed remote that sleeps past `remoteSearchBudget` returns within the
  budget, with `completeness: partial` and a non-empty `errors[]` (use the existing
  `setRemoteFilterScanLimits`-style seam, api.go:676–683, to keep it deterministic).
- Unit: `SkipHydration=true` skips the live fetch for uncached UIDs and yields
  `CompletenessPartial`, not an error.
- Contract/e2e (behind the existing `MAILMOOSE_BASE_URL` gate): `GET /v1/search?label=<x>` against
  the public host answers `200` with an envelope within ~10 s, and `GET /v1/search` with no filters
  answers `200` with partial completeness rather than timing out.
- Regression: `GET /v1/search?inbox=<id>&label=<x>` and `.../inboxes/<id>/search?...` stay instant
  and complete (they must not regress to partial).

**Docs:** `docs/API-REFERENCE.md` (generated) gets the budget/partial-completeness note; the labels
reference (`~/.hermes/skills/open-agent-inbox/references/labels-system.md`) is amended to drop the
wrong "slow without `q=`" claim.

---

## Finding 1 — the move-out gap

### What the code actually does (correcting the earlier summary)

- The 409 message `"folder-backed labels cannot be removed; move the message instead"` is **not** a
  store guard — it is a *controller* decision in `labelAction`
  (internal/httpapp/unified_labels.go:188–197): for a folder-backed label with
  `operation:"remove"`, it refuses and tells the caller to move. When the operation is `add` it
  simply returns a `move` action (`store.MessageActionInput{Kind: "move", Destination: ...}`).
- `MoveMessageToFolder` (internal/store/folders.go:499) already supports moving **out**: `dest :=
  strings.TrimSpace(destFolderID)`; when `dest == ""` it sets `destArg = nil`
  (folders.go:537–541) and writes `UPDATE messages SET mailbox_id=NULL` — i.e. back to the inbox
  root. The event carries `new_folder_id: ""`.
- The action pipeline accepts it: `EnqueueInboxAction` validates `case "move": if in.Destination ==
  "" { return ErrNotFound }` (internal/app/message_actions.go:325–328). **This is the one blocking
  line** — it treats an empty destination as invalid, which is precisely the "move back to Inbox"
  case the store supports.
- `POST /v1/messages/{id}` PATCH (api.go:814 `apiMessage`) does not accept `mailbox_id`, and there
  is no `POST /v1/messages/{id}/move` route in the table (server.go:473–482).
- The UI `POST /ui/inboxes/{id}/bulk action=move dest=<folder path>` (inbox_ui.go) *is*
  session-guarded, so API keys cannot use it. That part of the earlier finding stands.

So the gap is **one validation rule plus one thin API route** — not a missing store capability. The
earlier claim "no move-out route exists and removal 409s by design" was right about the outward
behaviour and wrong about the depth of the missing work.

### Fix 1

**1a. Allow an empty destination to mean "inbox root" in the action layer.**
internal/app/message_actions.go:325–328 — keep rejecting an *absent* field but allow an explicit
empty-destination move. Preferred shape: change the validation to accept a documented sentinel
rather than "anything empty", so an accidental empty body is still a 400:

```go
case "move":
    if in.Destination == "" && !in.MoveToRoot {
        return store.MessageAction{}, store.ErrNotFound
    }
```

with `MoveToRoot bool` on `store.MessageActionInput` (persisted alongside the other action fields),
and the worker (`applyMessageAction`, message_actions.go:253) mapping it to
`MoveMessageToFolder(ctx, p, id, "")`. That keeps the existing "empty destination is a client bug"
guard for every current caller.

**1b. Teach the labels route to contract a folder-backed label into a move-out.**
In `labelAction` (unified_labels.go:188), instead of refusing `remove` for a folder-backed label,
return the same `move` action with the root destination *when the message is currently in that
folder*. Concretely:

```go
if target.Kind == "folder" {
    if operation == "remove" {
        // Re-adding the label you are inside is a no-op; removing it means
        // "leave this folder", which the store expresses as an empty destination.
        if !messageIsInFolder(target) { return store.MessageActionInput{}, "label not applied to message", nil }
        return store.MessageActionInput{Kind: "move", MoveToRoot: true}, "", nil
    }
    ...
}
```

This needs the resolved message passed into `labelAction` (it currently receives the mailbox and
label parts only; `apiUnifiedMessageLabels` already has `m` in scope at unified_labels.go:238 and
can pass `m.MailboxID`/`m.FolderPath` in). Keep the 409 for the genuinely-ambiguous case where a
client tries to remove a folder label the message is *not* in — that is still a client bug.

**1c. Add the first-class REST route so plain API clients are not forced through the labels
grammar.** New `POST /v1/messages/{id}/move` (and optionally
`POST /v1/inboxes/{id}/messages/{messageId}/move` for symmetry with the labels route), body
`{"destination":"<folder_id>"}` or `{"destination":""}`/"inbox" for the root:

- Register in the route table near server.go:477 (`apiMessageRestore`).
- Handler resolves the message (`resolveMessageAny`, mailbox_access.go:155), authorizes with
  `p.CanAssist`, maps the destination: empty/"inbox" → `MoveToRoot`, otherwise the folder id is
  validated by the store (unknown id → 404, non-selectable → 409, both already implemented at
  folders.go:518–527).
- Reuses `submitMessageAction`/`EnqueueInboxAction`, so events (`message.folder_changed`) and the
  action-state polling (`?wait=true`) come for free.
- Role: `assistant` (same tier as labels), documented in `internal/apispec/spec.go` so the generated
  `docs/API-REFERENCE.md` picks it up.

**1d. Make the 409 message actionable.** If 1b lands, the `"folder-backed labels cannot be removed;
move the message instead"` string disappears from the labels path. If it does not land, append the
route name (`... move the message instead: POST /v1/messages/{id}/move`) so the error is
self-service. Do not leave the current dead-end text.

**Optional (UI parity, separate change):** surface a Move control for local custom folders in
`inbox_ui.go`/`assets/app.js` so the browser has the same capability. Out of scope for the API fix;
note it as follow-up.

### Tests (1)

- Store: `MoveMessageToFolder(ctx, p, id, "")` moves a message from a folder back to the root and
  emits `message.folder_changed` with `new_folder_id: ""` (guard: currently untested for the empty
  destination).
- Action: `EnqueueInboxAction` with `MoveToRoot` is accepted and executed; an empty destination with
  `MoveToRoot=false` is still rejected (the existing regression).
- Labels: `POST .../labels {"label_id":"folder:<id>","operation":"remove"}` on a message inside that
  folder now returns 200 and the message leaves the folder; on a message *not* in the folder it
  still 409s.
- REST: `POST /v1/messages/{id}/move {"destination":""}` returns the message at the root; unknown
  folder id 404; non-selectable 409; `?wait=true` reflects the move in the response (same queued-
  semantics caveat as labels, unified_labels.go testing note).
- E2E on the local standalone inboxes: the exact round trip that failed in the suite — apply
  `folder:<id>`, then move back to root — asserted via `GET /v1/inboxes/<id>/messages?folder=<id>`
  (now empty) and the message visible in the inbox listing. This is the assertion the suite could
  not make without a row-level SQL revert; after the fix the suite drops
  `labels_revert_folder_move.py` entirely.

**Docs:** `docs/API-REFERENCE.md` (generated from apispec), `docs/UNIFIED_LABELS.md` and the
labels-system reference both get the new move route plus a corrected "one location" note: a
folder-backed label is entered by adding it and left by moving out, not by removing it.

---

## Sequencing and open questions

Order: **2a → 2c/2b → 1a → 1b/1c → docs**. 2a is the smallest change with the largest user-visible
win and no API surface change. 1a is a prerequisite for both 1b and 1c.

Open questions for Ben before implementation:

1. Budget value for 2a (`8s` proposed) and whether an exhausted budget should be a hard `partial`
   or should still attempt one more page when no results were found yet.
2. Sentinel naming for the move-to-root contract: `MoveToRoot bool` (proposed), or overload
   `Destination: ""` and accept the looser validation?
3. Whether to ship 1b (labels-grammar contract) at all, or only 1c (explicit move route) and leave
   the folder-label remove returning a 409 that names the new route. 1c alone is a smaller, cleaner
   API; 1b makes the existing labels vocabulary self-consistent.
4. Whether the UI move control (1d/optional) is in scope now or a follow-up.

---

## Finding 3 (new, 2026-10-13) — an applied folder label's *move* fails on every provider

Reported after a run of RemoteDraft handoffs on standalone inboxes: "update labels action cannot be
completed on IMAP, Outlook, Gmail and Orwan", plus a stuck "publication queued" badge.

Two **separate** defects produce that experience. The stuck "publication queued" is **not** this
finding — it was a standalone-inbox `domain_id` NULL scan that failed every workflow notification;
that is fixed and recorded as `D119`. This finding is the action failure itself and is **still
open**.

### What was measured (live, public host, 2026-10-13)

Applying a folder-backed label queues a `move` action, which then fails on all four connector types:

| Inbox | Connector | Result of the queued `move` |
|---|---|---|
| berenebot@gmail.com | `imap` (imap.gmail.com) | `running` past the 30 s `?wait` window, then `failed` |
| b.dellar@outlook.com | `microsoft` | `failed` |
| donnasterling96@gmail.com | `one` (Gmail via One) | fails |
| teamberene@gmail.com | `google` | fails |

The IMAP inbox's label catalog exposed Gmail's `Important` and `Starred` as **custom** folder
labels, and a folder-label `remove` on all of them returns the documented `409 ... move the
message instead`. The queue rows for the failing Outlook inbox were a `move` to
`fld_06gjpwmmgw15p0694d4jyzjnt8` and a `label add FolderX`.

### Candidate root causes (needs the raw provider error to decide)

The real provider error is swallowed by the `execute` fallback that Finding 3's sibling fix in
`D119` just made visible — after `D119` a fresh attempt records the cause in the action's
`error`, so reproduce once more and read `GET /v1/actions/{id}` to choose between:

- **3a. Gmail/One destination mismatch.** `labelAction` (`internal/httpapp/unified_labels.go:188`)
  sends `Destination = target.FolderPath` for a standalone non-Microsoft inbox, but the Google
  branch of `mailboxLabels` (`unified_labels.go:54-63`) rewrites the entry to
  `ID = "tag:" + folder.Name` and **never assigns `FolderPath`**. So `moveGoogle`
  (`internal/app/google_mailbox.go:541`) receives an empty/`Name` destination and its
  `GetFolder`/`modifyGoogle` step fails. `IsOne` inboxes take the same `moveGoogle` path.
- **3b. IMAP destination scope/name.** `labelAction` sets `Destination = target.FolderPath`; for
  the IMAP inbox the paths came from Gmail's IMAP namespace, which may be outside the `INBOX`
  scope `folderInScope` admits (`internal/app/remote_mailbox_ops.go:919`), or the folder is a
  Gmail system label the server refuses to move into.

### Fix sketch (do not implement blind)

1. Reproduce once per connector with `?wait=true`, read the now-visible `error` from
   `GET /v1/actions/{id}`, and confirm 3a/3b against the real text.
2. Make `labelAction` pass a value each provider's move path actually accepts: a `folder:` **ID**
   for Google/One (mirroring the Microsoft branch, `unified_labels.go:193`), and a scoped,
   verified **path** for raw IMAP. Never a `Name` with an unset `FolderPath`.
3. Add a per-provider regression: apply a folder-backed label with `?wait=true` on each of
   `imap`, `one`, `google` and `microsoft`, and assert the queued `move` settles `done` and the
   message's folder actually changes.

### Tests

- HTTP/app: `labelAction` returns a non-empty, provider-appropriate `Destination` for a
  folder-backed label on each connector type (a unit guard that would have caught 3a).
- Integration: the four-connector `?wait=true` round trip above.

### Docs

- Correct the "one location" note in the labels-system reference so applying a folder label is
  described as a move that must resolve to a provider-valid destination, once the fix shape is
  known.
- Cross-reference from Finding 1: the move-*out* route (1c) and this move-*in* fix share the
  `move` action's destination contract, so land them together.

---

## Evidence

Repo: `/home/ben/projects/mailmoose/repo` (HEAD `27a7a4e`). Scratch harness (gitignored,
`.hermes-tmp/`): `label_suite.py`, `labels_revert_folder_move.py`, `label_suite_results.json`.

Key anchors:

- `internal/httpapp/mailbox_merge.go:413` `mergeSearch`; `:446` inbox loop; `:483` scan loop;
  `:507` hydration-adjacent mapping; `:552` `pageMerged`.
- `internal/httpapp/api.go:662` `remoteFilterScanPage = 50`; `:670` `remoteFilterScanMaxPages = 20`;
  `:676–683` test seam; `:581` `apiMessages`; `:814` `apiMessage`.
- `internal/app/remote_mailbox_more.go:153` `remoteSearchDefaultLimit = 200`; `:156` `SearchRemote`;
  `viewsForUIDs` live-fetch comment (~:215).
- `internal/httpapp/unified_labels.go:188–197` folder-label add→move / remove→409;
  `:223` `apiUnifiedMessageLabels`; `:256` 409 mapping.
- `internal/store/folders.go:499` `MoveMessageToFolder`; `:515–529` destination validation;
  `:537–541` empty destination → `NULL`; `:544` `message.folder_changed` event.
- `internal/app/message_actions.go:248–255` worker move case; `:315–331` `EnqueueInboxAction`;
  `:325–328` the `Destination == ""` rejection.
- `internal/store/messages.go:1580` `SearchMessagesFiltered` (empty-`q` short-circuit at `:1582`).
- `internal/httpapp/server.go:444–482` route table; `:456` labels route; `:473–482` message routes.
- `internal/apispec/spec.go:150` labels route doc (the shape a new move route should match).

Repro for the stall (uses the repo's `.env`; never prints the key):

```bash
python3 - <<'PY'
import os,time,urllib.request,urllib.error
env=dict(l.strip().split('=',1) for l in open('/home/ben/.hermes/.env') if '=' in l and not l.startswith('#'))
base=env['MAILMOOSE_BASE_URL'].rstrip('/'); key=env['MAILMOOSE_API_KEY'].strip().strip('"')
for p in ('/v1/search?label=x','/v1/search?inbox=in_06g9pe441nr9gmm6yjzz9afygg&label=x'):
    t=time.time()
    try:
        req=urllib.request.Request(base+p); req.add_header('Authorization','Bearer '+key)
        print(p, urllib.request.urlopen(req,timeout=45).status, '%.2fs'%(time.time()-t))
    except Exception as e:
        print(p, type(e).__name__, '%.2fs'%(time.time()-t))
PY
```

### Corrections this plan makes to the standing notes

- "`GET /v1/search?label=` times out because the account-wide merge is slow without `q=`" → both
  halves are wrong: it stalls *with* a `q=` too, and what matters is whether the remote leg can
  narrow (short-circuit filters `inbox=`/`unread=`), not whether `q` is present.
- "A folder-backed label cannot be removed; that is by design" → it is a controller choice in
  `labelAction`, and the store underneath already supports moving out (empty destination → `NULL`).
  The true gap is one validation rule and one missing route.

---

## Confirmed decisions (signed off before implementation)

- **Dec 1 — search budget:** `remoteSearchBudget = 8 * time.Second`, returned as `partial` +
  `errors[]` immediately. No "one more page" retry. Package var with a test seam mirroring
  `setRemoteFilterScanLimits`.
- **Dec 2 — move-to-root contract:** explicit `MoveToRoot bool` sentinel. An empty `Destination`
  with `MoveToRoot=false` stays a rejection, so a malformed body cannot silently teleport a message
  to the inbox root.
- **Dec 3 — scope:** ship both 1b (labels-grammar contracts a folder-label `remove` into a move-out)
  and 1c (explicit `POST /v1/messages/{id}/move`). The route alone leaves the API self-inconsistent.
- **Dec 4 — UI:** the browser Move control is a **follow-up**, not this change. The session-guarded
  bulk endpoint already serves the browser; no user is blocked there.
- **Dec 5 — PR structure (Ben's steer):** deliver as a **fully validated, working feature**, ideally
  **one PR**, with complete documentation and tests included. Not a plan-only hand-off. If review
  volume argues for a split, the natural seam is search (Dec 1) vs move-out (Dec 2/3), but default
  to one coherent PR.

## Documentation and agent-guide changes required

The labels system has **four documentation surfaces**, one of which is consumed directly by agents
at runtime. All must land with the code, in the same PR.

### Runtime agent-facing (highest priority — no repo doc)

- **`/agent` route** — served live by `internal/httpapp/api.go:76`
  (`apispec.RenderAgentGuide(apispec.Routes())`); advertised in the `/v1/bootstrap` payload
  (api.go:43–45) and linked from the sign-in page. Source: `internal/apispec/agent.go`. It is
  *rendered from the route table*, so a new route appears automatically — but only if the route is
  registered in `internal/apispec/spec.go`. Nothing to hand-edit here, which is exactly why the
  spec entry is mandatory (below).

### Generated (CI-diff enforced — must be regenerated)

- **`internal/apispec/spec.go`** — register the new `POST /v1/messages/{id}/move` Route
  (Summary, Description, Role `assistant`, Group `Labels`), matching the shape at spec.go:150.
- **`internal/apispec/route_io.go`** — add the request/response binding for the new path
  (`{Request: "MessageMoveBody", Response: "MessageActionResult", Query: []Param{qWait}}`),
  alongside the existing `POST /v1/messages/{id}/restore` line at route_io.go:103.
- **`internal/apispec/schemas.go`** — add a `MessageMoveBody` schema (`destination` string,
  optional; documents that an empty/absent destination with the move-to-root intent returns the
  message to the inbox root). Also amend the label schema note at schemas.go:214/226 so the
  `exclusive` membership description states that leaving a folder-backed label is a move, not a
  remove.
- **`docs/API-REFERENCE.md`** — regenerate via `make docs` (`./mailmoose-go.sh run ./cmd/docsgen >`).
  **CI fails on drift**: `.github/workflows/ci.yml:52–53` regenerates and `diff`s the file, and
  `tests/unit/apispec/markdown_test.go:27` asserts `RenderMarkdown` matches the table.
- **`tests/unit/apispec/spec_test.go:14`** — `TestRoutesCount` **pins the table at 120 routes**.
  Adding one route makes this test fail until the constant is bumped to `121`. This is a required
  edit, not an incidental one — miss it and the PR is red.

### Hand-maintained docs (must be corrected — they currently document the broken behaviour as
design)

- **`docs/UNIFIED_LABELS.md`** — lines 33–52. Two corrections and one addition:
  - Line 46–47 (`"Folder-backed labels create a move action; attempting to remove an exclusive
    folder label without another destination is rejected"`) — replace: removing a folder-backed
    label a message is inside now **moves it back to the inbox root**; the 409 survives only for
    removing a folder label the message is not in.
  - Add the new `POST /v1/messages/{id}/move` to the Canonical API list (lines 35–42).
  - Line 10–11 and 49–52 stay accurate; the "single free-text Add label field" UI description is
    unchanged by this work (Dec 4 defers the UI move control).
- **`docs/API.md`** — section 6 (Search, lines 544–562) needs the budget/partial-completeness
  behaviour documented: a broad search fans out over every authorized remote inbox, is bounded by
  a budget, and reports `completeness: partial` with `errors[]` entries for inboxes that did not
  answer in time. Optionally add the move route to section 4.
- **`docs/DECISIONS.md`** — add an entry as **D120** (D119 is now used by the standalone
  workflow-delivery fix; the file's highest number before that was D118; the labels block at
  lines 1071–1090 is D044-adjacent and only documents the older label design).
  Record: (a) the move-to-root sentinel and why an explicit flag beat overloading an empty
  destination; (b) that a folder label's `remove` contracts to a move-out; (c) the search budget
  value and that exhaustion is reported as partial, not an error. Match the existing bullet style
  at lines 1071–1090.
- **`docs/SPEC.md`** — line 461 lists `/v1/search`. No change needed to the path list; if the
  direction section (453–468) is treated as normative, add one line that cross-inbox search is
  best-effort/partial by design. Low priority — flag for Ben.

### Changelog

- **`CHANGELOG.md`** — add an `[Unreleased] ### Fixed` entry for the search stall and a
  `### Added`/`### Changed` entry for the move route and the folder-label remove semantics. Note
  the drift rule in the Makefile: `make changelog` copies `CHANGELOG.md` into
  `internal/httpapp/assets/CHANGELOG.md` (served at `/changelog`) and **a test fails when the two
  drift** — so both must be synced.

### Agent-guide / skill surfaces outside the repo

- **`~/.hermes/skills/open-agent-inbox/references/labels-system.md`** (already written) — amend the
  "The gap: into a folder, not back out" section: it becomes an *historical* note describing the
  pre-fix behaviour, and gains the working move-out recipe (move route, or the label `remove`
  contract). Also replace the **Known limitation** section (the wrong "slow without `q=`" claim)
  with the corrected remote-fan-out explanation and the budget behaviour.
- **`~/.hermes/skills/open-agent-inbox/SKILL.md`** — still lacks a pointer to that reference file;
  the skill security scanner blocked the edit (it flags the pre-existing host line as exfiltration,
  a false positive). **Flag for Ben:** the reference is on disk but not linked, so it will not be
  discovered by an agent loading the skill. Options: resolve the scanner false positive, or place
  the pointer in a way the scanner accepts. Its route table (line 41–46) also gains the move route
  and a note that `/v1/search` is budget-bounded and may return `partial`.
- **`~/.hermes/skills/communication/mailmoose-email/SKILL.md`** — **contains a stale, now-wrong
  claim** at lines 194–197: *"Labels apply to **sent messages only** via
  `PATCH /v1/messages/{id}` … Drafts do not support labels."* That predates the unified labels
  system and will mislead any agent that loads this skill. Correct it to point at the label-catalog
  routes and the label grammar (`name`/`label_id` + `operation`), note that labels apply to received
  messages as well as sent, and summarise the folder-backed-label move semantics. This is an agent
  guide, not a repo doc, so the PR does not cover it — do it as a separate skill edit alongside.
- **Repo `CONTRIBUTING.md` / `docs/TESTING.md`** — no change required; the tests follow the existing
  black-box `tests/unit/<package>/` convention, which CONTRIBUTING already documents.

### Test surfaces that must change (not docs, listed for completeness)

- `tests/unit/apispec/spec_test.go` route-count bump (120 → 121), plus a
  new move-route assertion.
- `tests/unit/httpapp/api_surface_test.go` — asserts `/openapi.json` covers **every** entry in
  `apispec.Routes()`, so the new move route must have a matching OpenAPI operation or this fails.
- New black-box tests under `tests/unit/httpapp/` and `tests/unit/store/` per the Tests sections
  above, and a contract/e2e case behind the existing `MAILMOOSE_BASE_URL` gate.
- The `.hermes-tmp/label_suite.py` harness: after the fix it drops `labels_revert_folder_move.py`
  and asserts the round trip in-band (see Finding 1 tests).

### Ordering for the docs work

Regenerate-and-diff surfaces (`spec.go` → `route_io.go` → `schemas.go` → `make docs` → route-count
test) first, because CI is red until they agree. Then the hand-maintained narrative docs
(`UNIFIED_LABELS.md`, `API.md`, `DECISIONS.md`, `SPEC.md`), then `CHANGELOG.md` + `make changelog`,
then the Hermes skill reference. Per the repo's own convention (`docs/TESTING.md`, and the global
AGENTS.md rule), launch the test run in the background as soon as the source is final and do the
markdown edits while it runs — but never edit a file the build compiles during a run.
