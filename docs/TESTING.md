# Testing

This document describes how tests are run, how output is captured, and where to
find logs. The short version: **run everything through `./tests/run.sh`, and
never re-run a test just to see the full output — it is always on disk.**

## The unified runner

`./tests/run.sh` is the single entry point. Every invocation writes a complete,
self-contained run folder under `tests/logs/runs/<run-id>/` and prints only a
one-line-per-tier summary to stdout (cheap for agents and CI).

```bash
./tests/run.sh                      # default preset: unit + vet + fmt (CI parity)
./tests/run.sh --unit --vet         # pick specific tiers
./tests/run.sh --all                # every tier, including smoke
./tests/run.sh --smoke              # slow gated tests only (opt-in)
./tests/run.sh --last               # print the newest run's summary + paths (no run)
./tests/run.sh --prune --keep 20    # reclaim space (runs are kept in full by default)
./tests/run.sh --list               # list tiers and flags
```

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
| `smoke` | slow gated tests with `MAILMOOSE_SMOKE=1` |

The `smoke` tier holds tests that are valuable but expensive, so they are **not**
in the default preset:

- `TestBinaryHTTP2AndSMTP` — compiles and runs the real `dialmx/cmd/receiver`
  binary and drives it over HTTP/2 and SMTP.
- `TestLegacyPBKDF2StillVerifies` — a production-cost (310k-iteration) PBKDF2
  round-trip guarding the pre-Argon2id compatibility path.

They gate on `tests/support/smoke.Require`, which skips unless
`MAILMOOSE_SMOKE=1`. CI runs them as an explicit step so coverage is never
silently lost.

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
- **L3** is the full captured stream — the thing to read instead of re-running.

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
`tests/fixtures/mailmoose-mx` test fixture.

## CI

CI (`.github/workflows/ci.yml`) runs on native Go, not the Docker runner, for
speed. Each step tees its full output to `ci-logs/` and the folder is uploaded
as the `ci-logs` artifact with `if: always()`, so a failed run's full log is
downloadable without re-running.

## Fixtures

- `tests/fixtures/mailmoose-webhook-contract.json` — the MailMoose/Billbot
  webhook wire contract.
- `tests/fixtures/mailmoose-mx` — the receiver binary measured by the launcher
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
  isolated stores backed by a migrated template instead of replaying every
  migration per fixture. Do **not** use it in tests that assert migration
  behaviour or need a genuinely fresh database.
- Deliberately slow tests belong behind `tests/support/smoke.Require` so they
  run only in the `smoke` tier.
