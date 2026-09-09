#!/bin/bash
# tests/scripts/prune-test-logs.sh — bound tests/logs/ via LRU (mtime) pruning.
#
# The containerised test runners emit per-run artifacts under tests/logs/:
# the Go wrapper leaves a dated .log per invocation, and the unified runner
# leaves a dated run dir per invocation. Successful runs are kept (they are
# small), but killed/interrupted runs pile up and there is no bound.
#
# Run the prune BEFORE a run starts: keep the N most recently modified
# entries per glob, remove the rest. It is deliberately mtime-based (LRU),
# not count-based on any per-test logic, so a preserved failed-run artifact is
# kept if recent and only evicted once it becomes the oldest.
#
# Source (`.`) this from a runner; or invoke it directly as a script. In the
# direct form, pass an optional cap as $1, defaulting to KEEP_GO.

set -eu

# Defaults: how many of the most recent entries per glob to keep.
KEEP_GO=${KEEP_GO:-10}
KEEP_RUNS=${KEEP_RUNS:-10}

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

collect_evictions 'gatehouse-go/*.log' "$KEEP_GO"
collect_evictions 'runs/*' "$KEEP_RUNS"

if [ "${#evict[@]}" -eq 0 ]; then
	(return 0 2>/dev/null) || exit 0
fi

for rel in "${evict[@]}"; do
    rm -rf -- "$logs_dir/$rel" 2>/dev/null || true
done
