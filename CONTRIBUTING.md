# Contributing to MailMoose

Thanks for your interest. This is a V1 project with a deliberately small scope.

Please also read `SECURITY.md` before reporting a security issue, and never open
a public issue for a suspected vulnerability.

## Prerequisites

Docker is required. Go is intentionally **not** installed on the host; all Go
commands run in a container through `./mailmoose-go.sh`.

## Development commands

```bash
./mailmoose-go.sh test -race -count=1 ./...   # tests
./mailmoose-go.sh vet ./...                   # static analysis
./mailmoose-go.sh build ./cmd/...             # build server and mx
./mailmoose-go.sh gofmt -w cmd internal       # format
```

For tiered runs with log capture and CI parity, use the unified runner:

```bash
./tests/run.sh              # default: unit + vet + fmt
./tests/run.sh --list       # list available tiers
```

## Tests

- Go test source lives under `tests/unit/<package>/` as external
  `<package>_test` packages (black-box, public API only).
- Every public API behaviour and every transport adapter needs tests. Critical
  flows need integration tests (inbound delivery, duplicate webhooks, event
  replay, scoped authorization, thread grouping, send/reply, Hermes Relay,
  storage quota enforcement).
- Propose the smallest tier that would catch a regression rather than running
  everything by default.

## Formatting gate

CI fails on unformatted Go files. Before pushing, confirm the gate is clean:

```bash
./mailmoose-go.sh gofmt -l cmd internal tests
```

Write Go with tabs, never spaces, and never collapse a block onto one line.

## Dependencies

Only the dependencies listed as approved in `docs/DECISIONS.md` may be used. Adding a
new dependency, service, or infrastructure component requires an explicit,
named request from a maintainer, plus an entry in `THIRD_PARTY_NOTICES.md` and a
decision recorded in `docs/DECISIONS.md`.

## Sending changes

1. Work on a branch; do not commit directly to the default branch.
2. Keep commits focused and write a clear message describing the change.
3. Open a pull request against `dellarb/mailmoose:main`.
4. For architectural changes, include a `docs/DECISIONS.md` entry following the
   existing format.

## Licence

MailMoose is licensed under the GNU Affero General Public License v3.0
(AGPL-3.0). By contributing, you agree that your contributions are licensed
under the same terms.
