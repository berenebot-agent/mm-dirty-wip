#!/bin/bash
# mailmoose-go.sh — run Go commands inside Docker (build/test/vet/tidy) with a
# persistent cache so repeated runs are fast, and libsqlite3-dev pre-baked so
# every run doesn't pay apt-get (internal/sqlite3driver is cgo).
#
# Usage:
#   ./mailmoose-go.sh <go args...>     e.g. ./mailmoose-go.sh test -race -count=1 ./...
#   ./mailmoose-go.sh vet ./...
#   ./mailmoose-go.sh mod tidy
#   ./mailmoose-go.sh gofmt -l cmd internal   (direct gofmt binary, for the CI fmt gate)
#
# Caches (bind-mounted, NOT named volumes):
#   gomod   = ~/.cache/mailmoose-go/mod      module download cache (/go/pkg/mod)
#   gobuild = ~/.cache/mailmoose-go/build    compiled package cache (/root/.cache/go-build)
#
# Toolchain image:
#   Base is pinned to match the Dockerfile build stage (golang:1.27-bookworm).
#   The wrapper builds a local cached image (mailmoose-go:1.27) with
#   libsqlite3-dev preinstalled on first use, then reuses it. Override with
#   MAILMOOSE_GO_IMAGE to point at another image (e.g. the bare base).
#
# RAM safety:
#   --memory/-m limits the container's hard memory ceiling (OOM inside, not host OOM)
#   --memory-swap = same value => no swap growth for this container
#   GOFLAGS=-p=2 caps compiler parallelism to cut peak RAM further

set -eu

GO_BASE="golang:1.27-bookworm"
GO_IMAGE="${MAILMOOSE_GO_IMAGE:-mailmoose-go:1.27}"
CACHE_DIR="${MAILMOOSE_GO_CACHE:-$HOME/.cache/mailmoose-go}"
MOD_CACHE="$CACHE_DIR/mod"
BUILD_CACHE="$CACHE_DIR/build"
MEM_LIMIT="${MAILMOOSE_GO_MEM:-2g}"   # hard RAM cap for the container (-race needs headroom)

# Create cache dirs if absent
mkdir -p "$MOD_CACHE" "$BUILD_CACHE"

# Repo root = this script's own directory (mailmoose-go.sh lives in the repo
# root, the dir that contains go.mod). Resolve symlinks + get absolute path.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT="$SCRIPT_DIR"

# Build the cached toolchain image once (libsqlite3-dev pre-baked for cgo).
if ! docker image inspect "$GO_IMAGE" >/dev/null 2>&1; then
    echo "mailmoose-go.sh: building cached image $GO_IMAGE ..." >&2
    docker build -f "$REPO_ROOT/tests/docker/Dockerfile.gocache" \
        --build-arg "GO_BASE=$GO_BASE" \
        -t "$GO_IMAGE" "$REPO_ROOT/tests/docker" >&2
fi

# A container without a TTY/terminal shouldn't try to allocate one
TTY_FLAG=""
if [ -t 1 ]; then TTY_FLAG="-it"; fi

ENV_FLAGS="-e GOFLAGS=-p=2 -e CGO_ENABLED=1"

# Log capture: always write full output to a repo-local log file under
# tests/logs/ (gitignored, persistent across runs). The summary printed at
# the end shows the path; on failure the first FAIL line is also inlined.
#
# When wrapped by tests/run.sh, MAILMOOSE_TEST_DIR is set: write straight to
# <dir>/out.log so the unified runner owns the run folder. When unset, use
# the dated per-invocation file (direct-invocation behavior is unchanged).
if [ -n "${MAILMOOSE_TEST_DIR:-}" ]; then
    LOG_DIR="$MAILMOOSE_TEST_DIR"
else
    LOG_DIR="tests/logs/mailmoose-go"
fi
mkdir -p "$LOG_DIR"
if [ -n "${MAILMOOSE_TEST_DIR:-}" ]; then
    LOG_FILE="$LOG_DIR/out.log"
else
    LOG_FILE="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-go-$(echo "$*" | tr ' /' '__').log"
fi
ts=$(date +%s)

# gofmt passthrough: `go fmt` is not `gofmt -l`, and the CI gate checks the
# latter, so run the gofmt binary directly when asked.
if [ "${1:-}" = "gofmt" ]; then
    docker run $TTY_FLAG --rm \
        --memory="$MEM_LIMIT" \
        --memory-swap="$MEM_LIMIT" \
        $ENV_FLAGS \
        -v "$MOD_CACHE:/go/pkg/mod" \
        -v "$BUILD_CACHE:/root/.cache/go-build" \
        -v "$REPO_ROOT:/src" \
        -w /src \
        "$GO_IMAGE" \
        gofmt "${@:2}" 2>&1 | tee "$LOG_FILE"
else
    docker run $TTY_FLAG --rm \
        --memory="$MEM_LIMIT" \
        --memory-swap="$MEM_LIMIT" \
        $ENV_FLAGS \
        -v "$MOD_CACHE:/go/pkg/mod" \
        -v "$BUILD_CACHE:/root/.cache/go-build" \
        -v "$REPO_ROOT:/src" \
        -w /src \
        "$GO_IMAGE" \
        go "$@" 2>&1 | tee "$LOG_FILE"
fi
rc=${PIPESTATUS[0]}
te=$(date +%s)

echo "==> mailmoose-go.sh: rc=$rc, $((te - ts))s elapsed, log: $LOG_FILE" >&2
if [ "$rc" -ne 0 ]; then
    first_fail=$(grep -m1 -E '^--- FAIL|^FAIL\b' "$LOG_FILE" 2>/dev/null | head -c 400 || true)
    [ -n "$first_fail" ] && echo "    first failure: $first_fail" >&2
fi
exit "$rc"
