#!/bin/bash
# tests/scripts/prune-test-logs.sh — bound tests/logs/ via LRU (mtime) pruning.
#
# The containerised test runners emit per-run artifacts under tests/logs/:
# the Go wrapper leaves a dated .log per invocation, and the unified runner
# leaves a dated run dir per invocation. Runs are RETAINED IN FULL by default
# (tests/run.sh no longer prunes automatically) so an agent can always dig
# into an earlier run; this script reclaims space only when invoked explicitly.
#
# It is deliberately mtime-based (LRU), not count-based on any per-test logic,
# so a preserved failed-run artifact is kept if recent and only evicted once it
# becomes the oldest.
#
# Source (`.`) this from a runner with KEEP_GO / KEEP_RUNS set; or invoke it
# directly as a script. In the direct form, pass an optional cap as $1.
#
# Never touches:
#   - tests/logs/history.tsv   (append-only ledger)
#   - tests/logs/mailmoose-mx  (test fixture binary, used by the launcher RSS test)
#   - tests/logs/latest        (symlink pointer)

set -eu

# Defaults: how many of the most recent entries per glob to keep.
KEEP_GO=${KEEP_GO:-10}
KEEP_RUNS=${KEEP_RUNS:-10}

# Direct-invocation form: an optional numeric $1 overrides both caps. When the
# script is sourced (`.`), "$@" belongs to the caller, so skip this entirely.
if [ "${BASH_SOURCE[0]}" = "${0}" ] && [ "$#" -ge 1 ] && [ -n "${1:-}" ]; then
    KEEP_GO="$1"
    KEEP_RUNS="$1"
fi

logs_dir="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]:-$0}")/../.." && pwd)/tests/logs"

# Collect entries (relative to logs_dir) that are beyond the keep cap.
evict=()
collect_evictions() {
    local glob="$1" keep="$2"
    local base
    base="$(dirname "$glob")"
    [ -d "$logs_dir/$base" ] || return 0
    local -a entries dirs
    mapfile -t entries < <(ls -1dt "$logs_dir"/$glob 2>/dev/null || true)
    local count=${#entries[@]}
    local i rel
    for ((i = keep; i < count; i++)); do
        rel="${entries[$i]#"$logs_dir"/}"
        case "$rel" in "") ;; *) evict+=("$rel") ;; esac
    done
}

collect_evictions 'mailmoose-go/*.log' "$KEEP_GO"
collect_evictions 'gatehouse-go/*.log' "$KEEP_GO"
collect_evictions 'runs/*' "$KEEP_RUNS"

if [ "${#evict[@]}" -eq 0 ]; then
	(return 0 2>/dev/null) || exit 0
fi

for rel in "${evict[@]}"; do
    rm -rf -- "$logs_dir/$rel" 2>/dev/null || true
done
