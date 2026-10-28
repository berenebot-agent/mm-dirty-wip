# Testing

This document describes how tests are run, how output is captured, and where to
find logs. The short version: **run everything through `./tests/run.sh`, and
the complete output is saved for each run.**

## The unified runner

`./tests/run.sh` is the single entry point. Every invocation writes a complete,
self-contained run folder under `tests/logs/runs/<run-id>/` and prints only a
one-line-per-tier summary to stdout.

```bash
./tests/run.sh                      # default preset: unit + vet + fmt + lint + js (CI parity)
./tests/run.sh --unit --vet         # pick specific tiers
./tests/run.sh --all                # every tier, including smoke
./tests/run.sh --smoke              # slow gated tests only (opt-in)
./tests/run.sh --last               # print the newest run's summary + paths (no run)
./tests/run.sh --prune --keep 20    # reclaim space (runs are kept in full by default)
./tests/run.sh --list               # list tiers and flags
```

The default preset is the local CI-parity set — it includes the **lint** and
**js** tiers as well as unit, vet and fmt. The `js` tier needs Node on the host.

`make` targets route through the runner so logs are always captured:

```bash
make test        # ./tests/run.sh --unit
make test-all    # ./tests/run.sh --unit --smoke
make smoke       # ./tests/run.sh --smoke
make vet         # ./tests/run.sh --vet
make fmt-tests   # ./tests/run.sh --fmt   (CI gofmt -l gate)
make fmt         # gofmt -w (rewrites files; not the gate)
```

## Tiers

| Tier | What it runs |
|---|---|
| `unit` | `go test -race -count=1 -timeout=60s -v ./...` |
| `vet` | `go vet ./...` |
| `fmt` | `gofmt -l cmd internal tests dialmx` (fails on output) |
| `lint` | `staticcheck -checks U1000 ./...` (dead code; fails on output) |
| `js` | Tusk dialog, DKIM toggle, copy-controls, empty-trash, passkey and release-config checks |
| `smoke` | slow gated tests with `MAIL_SMOKE=1` |

The `smoke` tier holds tests that are valuable but expensive, so they are **not**
in the default preset:

- `TestBinaryHTTP2AndSMTP` — compiles and runs the real `dialmx/cmd/receiver`
  binary and drives it over HTTP/2 and SMTP. The local smoke tier runs only this
  receiver package (`./tests/unit/dialmx/receiver/`).

They gate on `tests/support/smoke.Require`, which skips unless
`MAIL_SMOKE=1`. CI runs them as an explicit step so coverage is never
silently lost, and that step adds one thing the local tier does not:
`internal/auth`'s gated production-cost Argon2 tests
(`MAIL_SMOKE=1 go test -race -count=1 -timeout=120s ./internal/auth ./tests/unit/dialmx/receiver/`).
Run that command directly if you need the auth smoke locally.

## Run folder layout

Every run produces a folder named `<UTC timestamp>-<pid>-<tiers>`:

```
tests/logs/runs/20261004T043014-1613210-unit,vet,fmt/
  summary.txt      L1: per-tier PASS/FAIL + elapsed + first error + detail path
  timings.txt      L2: per-tier elapsed + per-test breakdown (each test tier)
  full.log         L3: every tier's complete output, concatenated
  meta.txt         run id, tiers, git SHA/branch/dirty, toolchain image
  unit/out.log     raw output for the unit tier
  vet/out.log
  fmt/out.log
```

- **L1** is what reaches stdout / the terminal. It is deliberately terse.
- **L2** ranks tests by elapsed time so the slowest are obvious.
- **L3** is the full captured stream.

## Reading logs without re-running

`tests/logs/latest` is a symlink to the newest run folder:

```bash
cat  tests/logs/latest/summary.txt      # pass/fail overview
cat  tests/logs/latest/timings.txt      # slowest tests first
cat  tests/logs/latest/meta.txt         # which commit was tested
tail -n 200 tests/logs/latest/unit/out.log   # raw tail of the unit tier
grep -n '--- FAIL' tests/logs/latest/full.log
```

`./tests/run.sh --last` prints the newest summary, the exact paths, and the last
few ledger entries in one shot.

## Profiling where the unit tier spends its time

`timings.txt` gives per-test times for a run, but to optimise the suite you want
per-package attribution and a machine-readable source. That is what
`tests/scripts/go-test-timings.sh` is for: it runs `go test -json ./...` and
reports total wall time, the slowest packages, and the slowest tests.

```bash
./tests/scripts/go-test-timings.sh              # -race -count=1 -timeout=60s (tier parity)
./tests/scripts/go-test-timings.sh --no-race    # faster pass; relative hotspots only
./tests/scripts/go-test-timings.sh --save-json  # keep raw go test -json under tests/logs/
```

It defaults to the unit tier's CI command (`-race -count=1 -timeout=60s`) so the
numbers match what CI and `run.sh` actually pay, and it exits non-zero if any
test failed. Note that package wall times overlap (packages run in parallel), so
their sum exceeds the tier's wall time — use the per-package ranking, not the sum.

## Retention

Runs are **retained in full** — there is no automatic pruning, so an earlier
run is always available to dig into. Reclaim space explicitly:

```bash
./tests/run.sh --prune --keep 20
```

`tests/logs/history.tsv` is an append-only ledger (timestamp, SHA, branch, dirty
state, tiers, result, elapsed, run dir) that survives pruning — use it for
trends and regression hunting:

```bash
column -t tests/logs/history.tsv | tail -20
```

The prune script never removes the ledger, the `latest` symlink, or the
`tests/fixtures/mammothmail-mx` test fixture.

## CI

CI (`.github/workflows/ci.yml`) runs on native Go, not the Docker runner, for
speed. Each step tees its full output to `ci-logs/` and the folder is uploaded
as the `ci-logs` artifact with `if: always()`, so a failed run's full log is
downloadable without re-running.

CI, release and development-image publish gates also install the locked
OpenClaw plugin dependencies, compile the TypeScript and run its Node tests.
Run the same checks from the repository root in Docker (Node 24):

```bash
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD:/src" -w /src/plugins/openclaw/openclaw-plugin \
  node:24-bookworm sh -ec 'npm ci && npm run build && npm test'
```

These plugin checks are separate from `tests/run.sh`. Its `js` tier checks
that all three workflows retain the plugin gate and that SMTP port 25 remains
an explicit opt-in in the default Compose file.

## Fixtures

- `tests/fixtures/mammothmail-webhook-contract.json` — the Mammoth Mail/Billbot
  webhook wire contract.
- `tests/fixtures/mammothmail-mx` — the receiver binary measured by the launcher
  RSS test. Gitignored and built on demand with `make mx-fixture`; the test
  skips when it is absent.

## Writing tests

- Go test source lives under `tests/unit/<package>/` as external
  `<package>_test` packages (black-box, public API only). Exceptions that need
  unexported access are documented in-file (e.g. `cmd/server/mxruntime_test.go`).
- Every public API behaviour and every transport adapter needs tests. Critical
  flows need integration tests (inbound delivery, duplicate webhooks, event
  replay, scoped authorization, thread grouping, send/reply, Hermes Relay,
  storage quota enforcement).
- Prefer the smallest tier that would catch a regression.
- Tests that need a database should use `tests/support/testdb`, which hands out
  isolated stores backed by a once-migrated template instead of applying the
  baseline per fixture. Do **not** use it in tests that assert schema/baseline
  behaviour or need a genuinely fresh database.
- Public schema initialization, restart/data preservation and unsupported-schema
  rejection are covered in `tests/unit/dbschema`. Its mail workflow checks run
  against a fresh public database. `internal/dbschema/schema_test.go` exercises
  future additive migrations and rollback through the private runner, without
  production test hooks. These tests require no historical development schema.
- Deliberately slow tests belong behind `tests/support/smoke.Require` so they
  run only in the `smoke` tier.

## Release checks

These are manual and end-to-end checks to run before a release. Automated unit, integration and smoke tests remain the normal development gate. Record any external-provider checks that could not be completed.

### Accounts and permissions

- Create an administrator and multiple inboxes. Verify Read, Assistant and Owner keys have the expected access and cannot reach unrelated inboxes.
- Verify CSRF, secure cookies and session revocation. Create a mailbox user with restricted inbox access.
- Request a send as Assistant, then approve and reject drafts. Verify a request cannot be approved twice and remote-draft mode puts the draft in the connected provider's Drafts folder.

### Receiving and connected mailboxes

- Receive test mail through Direct MX and Tusk MX. Confirm DNS setup, status, inbox routing and unknown-recipient handling.
- Exercise at least one authenticated webhook receiving provider (Mailgun, Cloudflare, Resend or Postmark), including a duplicate delivery.
- Connect Gmail, Microsoft and a generic IMAP mailbox. Check initial sync, later arrivals, folders, labels, attachments and disconnect/reconnect behavior. Test One/Gmail if available.

### Sending and integrations

- Send using configured outbound providers and an attachment. Check sent history and idempotency.
- Verify uncertain outbound delivery is not automatically retried.
- Connect Hermes and OpenClaw, deliver email, reply and reconnect after an interruption.
- Verify webhook notification and raw MIME forward modes, authentication, duplicate handling and skipped messages.
- Give an API client only the base URL and key; verify it can discover permitted operations.

### Recovery and release

- Back up and restore `/data` and any separately supplied encryption key.
- Check the basic resource profile under idle and ingest load, and investigate unexpected memory or CPU growth.
- Record the commit tested, test results and any skipped provider checks in the release notes.
