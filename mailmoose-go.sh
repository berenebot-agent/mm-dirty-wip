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
#   GOFLAGS=-p=<cores> sets build parallelism and test-binary package concurrency
#   to the host's logical CPU count (nproc), which is also Go's own default for
#   -p/GOMAXPROCS. The heavy packages (httpapp/app/store) overlap enough to keep
#   the tier under the 60s per-binary -timeout. Overrides: MAILMOOSE_GO_PARALLEL
#   sets -p explicitly; MAILMOOSE_GO_MEM sets the RAM cap. By default the cap is
#   half the host's physical RAM (a ceiling, not a reservation).
#
# Scratch disk:
#   The suite is SQLite-heavy: every store-backed test creates and migrates a DB
#   under TMPDIR and fsyncs it. Without help that lands on the host's real disk
#   (the container's overlay /tmp), where fsync dominates. The container's /tmp
#   is therefore a tmpfs so those files live in RAM where fsync is free. Its size
#   is a ceiling only (tmpfs allocates lazily), so it can be generous; it is
#   capped so a runaway test cannot exhaust the container. Override with
#   MAILMOOSE_GO_TMPFS (e.g. 8g); set MAILMOOSE_GO_TMPFS=0 to disable and use
#   the container's normal /tmp.

set -eu

GO_BASE="golang:1.27-bookworm"
GO_IMAGE="${MAILMOOSE_GO_IMAGE:-mailmoose-go:1.27}"
CACHE_DIR="${MAILMOOSE_GO_CACHE:-$HOME/.cache/mailmoose-go}"
MOD_CACHE="$CACHE_DIR/mod"
BUILD_CACHE="$CACHE_DIR/build"

# Logical CPU count, used as the default -p.
CORES="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)"
case "$CORES" in
    ''|*[!0-9]*) CORES=2 ;;
esac
[ "$CORES" -lt 1 ] && CORES=1
GO_PARALLEL="${MAILMOOSE_GO_PARALLEL:-$CORES}"
case "$GO_PARALLEL" in
    ''|*[!0-9]*) GO_PARALLEL=$CORES ;;
esac

# Hard RAM cap for the container: half the host's physical RAM by default (a
# ceiling, not a reservation), floored at 2g so a small box still builds -race.
if [ -n "${MAILMOOSE_GO_MEM:-}" ]; then
    MEM_LIMIT="$MAILMOOSE_GO_MEM"
else
    MEM_TOTAL_KB="$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null || echo 0)"
    case "$MEM_TOTAL_KB" in
        ''|*[!0-9]*) MEM_TOTAL_KB=0 ;;
    esac
    if [ "$MEM_TOTAL_KB" -gt 0 ]; then
        HALF_G=$(( MEM_TOTAL_KB / 2 / 1024 / 1024 ))
        [ "$HALF_G" -lt 2 ] && HALF_G=2
        MEM_LIMIT="${HALF_G}g"
    else
        MEM_LIMIT="4g"
    fi
fi

# Numeric GiB of the RAM cap (for sizing the scratch tmpfs below).
MEM_LIMIT_G="$(printf '%s' "$MEM_LIMIT" | tr -dc '0-9')"
case "$MEM_LIMIT_G" in
    ''|*[!0-9]*) MEM_LIMIT_G=4 ;;
esac
[ "$MEM_LIMIT_G" -lt 1 ] && MEM_LIMIT_G=1

# Scratch tmpfs: a ceiling, not a reservation (tmpfs allocates lazily), so it can
# be generous. Default is the whole RAM cap: the suite's temp data is small in
# practice, and if it ever did grow, the cgroup would reclaim before the host
# felt it. MAILMOOSE_GO_TMPFS=0 disables the tmpfs (container uses its own /tmp);
# MAILMOOSE_GO_TMPFS=<n>g sets the size explicitly.
if [ "${MAILMOOSE_GO_TMPFS:-auto}" = "0" ]; then
    SCRATCH_FLAGS=""
else
    TMPFS_G="${MAILMOOSE_GO_TMPFS:-auto}"
    if [ "$TMPFS_G" = "auto" ]; then
        TMPFS_G=$MEM_LIMIT_G
    fi
    TMPFS_G="$(printf '%s' "$TMPFS_G" | tr -dc '0-9')"
    case "$TMPFS_G" in
        ''|*[!0-9]*) TMPFS_G=$MEM_LIMIT_G ;;
    esac
    [ "$TMPFS_G" -lt 1 ] && TMPFS_G=1
    # /tmp is the container's TMPDIR: t.TempDir(), os.MkdirTemp and the testdb
    # template all live here, so an in-RAM /tmp makes SQLite fsync ~free. exec is
    # required because Go builds and runs test binaries under $TMPDIR.
    SCRATCH_FLAGS="--tmpfs /tmp:rw,exec,size=${TMPFS_G}g,mode=1777"
fi

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

# Build/test parallelism follows the host core count (or MAILMOOSE_GO_PARALLEL).
#
# -trimpath lives in GOFLAGS, NOT on a command line: this wrapper is a
# pass-through (`go "$@"`), so a build flag added at the call site would be
# missed by every test/vet/gofmt invocation. GOFLAGS applies to all of them.
#
# Why it is here: without it an object's cache key includes its absolute build
# path, so the same source built from a different mount point or workdir gets a
# SEPARATE cache entry. Measured on this repo: adding -trimpath to a warm cache
# created 1,322 new entries while rebuilding without it created 0 — two full
# lineages of the same objects. This repo already builds from more than one
# mount path (see the backup work), so the divergence is live, not theoretical.
ENV_FLAGS=(-e "GOFLAGS=-p=${GO_PARALLEL} -trimpath" -e CGO_ENABLED=1 -e "GOMAXPROCS=${GO_PARALLEL}")

# Optional contract-fixture passthrough: when BILLBOT_CONTRACT_FIXTURE names a
# file, mount its parent directory at the identical container path and pass the
# variable through, so an in-container test that writes the fixture lands on the
# host. Without it the path is invisible inside the container (and the var is
# not exported): the fixture test skips. This is how the mailmoose/Billbot
# webhook wire contract is exported from a real worker test run.
FIXTURE_FLAGS=""
if [ -n "${BILLBOT_CONTRACT_FIXTURE:-}" ]; then
    fixture_dir=$(dirname -- "$BILLBOT_CONTRACT_FIXTURE")
    fixture_dir=$(CDPATH= cd -- "$fixture_dir" && pwd)
    FIXTURE_FLAGS="-e BILLBOT_CONTRACT_FIXTURE=$BILLBOT_CONTRACT_FIXTURE -v $fixture_dir:$fixture_dir"
fi

# Optional smoke-tier passthrough: when MAILMOOSE_SMOKE is set (tests/run.sh
# --smoke), forward it so the gated slow tests run instead of skipping.
SMOKE_FLAGS=""
if [ -n "${MAILMOOSE_SMOKE:-}" ]; then
    SMOKE_FLAGS="-e MAILMOOSE_SMOKE=$MAILMOOSE_SMOKE"
fi

# Manual One compatibility probe only: forward variable names, never embed
# credential values in command arguments, log filenames or test metadata.
ONE_PROBE_FLAGS=""
if [ "${1:-}" = "run" ] && [ "${2:-}" = "./cmd/oneprobe" ]; then
    ONE_PROBE_FLAGS="-e ONE_API_KEY -e ONE_CONNECTION_ID -e ONE_PROBE_SEND_TO"
fi

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
    LOG_SLUG=$(echo "$*" | tr ' /' '__')
    # Package lists can exceed the filesystem's 255-byte filename limit.
    LOG_FILE="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-go-${LOG_SLUG:0:160}-$$.log"
fi
ts=$(date +%s)

# gofmt passthrough: `go fmt` is not `gofmt -l`, and the CI gate checks the
# latter, so run the gofmt binary directly when asked.
if [ "${1:-}" = "gofmt" ]; then
    docker run $TTY_FLAG --rm \
        --memory="$MEM_LIMIT" \
        --memory-swap="$MEM_LIMIT" \
        $SCRATCH_FLAGS \
        "${ENV_FLAGS[@]}" \
        $FIXTURE_FLAGS \
        $SMOKE_FLAGS \
        $ONE_PROBE_FLAGS \
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
        $SCRATCH_FLAGS \
        "${ENV_FLAGS[@]}" \
        $FIXTURE_FLAGS \
        $SMOKE_FLAGS \
        $ONE_PROBE_FLAGS \
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
