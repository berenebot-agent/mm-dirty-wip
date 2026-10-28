#!/bin/bash
# tests/scripts/go-test-timings.sh — run `go test -json ./...` and report the
# slowest packages and tests.
#
# This is the profiling companion to ./tests/run.sh. run.sh answers "does the
# suite pass, and what did each tier take"; this answers "where did the Go unit
# tier actually spend its time", which is what you need before optimising it
# (parallelisation, fixture cost, per-package timeout margins).
#
# Usage:
#   ./tests/scripts/go-test-timings.sh               # run + summary (race, CI parity)
#   ./tests/scripts/go-test-timings.sh --no-race     # faster pass, no race detector
#   ./tests/scripts/go-test-timings.sh --save-json   # keep the raw go test -json under tests/logs/
#
# Defaults to `-race -count=1 -timeout=60s`, matching the unit tier's CI command
# so the numbers correspond to what CI/run.sh actually pays. --no-race drops the
# detector when you only want relative hotspots.
#
# Exit status mirrors the underlying go test run (0 = all pass) so this can be
# used as a gate. Parsing failures never mask a failure: if go test reports any
# failing action we exit non-zero regardless of what the summary printed.

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

save_json=false
use_race=true
for arg in "$@"; do
    case "$arg" in
        --save-json) save_json=true ;;
        --no-race) use_race=false ;;
        *) echo "usage: $0 [--save-json] [--no-race]" >&2; exit 2 ;;
    esac
done

# The single Go entrypoint: runs Go inside the pinned image with persistent
# caches (and, since the suite is SQLite-heavy, an in-RAM scratch /tmp).
go_wrapper="$repo_root/mammothmail-go.sh"
if [ ! -x "$go_wrapper" ]; then
    echo "go-test-timings.sh: missing or non-executable $go_wrapper" >&2
    exit 2
fi

test_args=(test -count=1 -json -timeout=60s)
if $use_race; then
    # -race is a `go test` flag, so it must follow the subcommand.
    test_args=(test -race "${test_args[@]:1}")
fi

json_file=""
if $save_json; then
    mkdir -p "$repo_root/tests/logs/go-test-timings"
    json_file="$repo_root/tests/logs/go-test-timings/$(date -u +%Y%m%dT%H%M%SZ)-go-test.json"
fi

# Run the suite, capturing the JSON stream. `|| true` lets us report a summary
# even on failure; the real exit code is recovered from the parsed actions.
output="$(mktemp)"
trap 'rm -f "$output"' EXIT

start=$(date +%s.%N)
"$go_wrapper" "${test_args[@]}" ./... 2>&1 | tee "$output" >/dev/null || true
end=$(date +%s.%N)
elapsed_total=$(awk "BEGIN {printf \"%.3f\", $end - $start}")

if $save_json && [ -n "$json_file" ]; then
    # Keep only the go test JSON lines; the wrapper emits its own `==>` summary
    # lines that are not part of the stream.
    grep -E '^\{' "$output" > "$json_file" || true
    echo "raw JSON: $json_file" >&2
fi

# Any failing action (test or package) means the run failed.
if grep -q '"Action":"fail"' "$output"; then
    run_rc=1
else
    run_rc=0
fi

python3 - "$output" "$elapsed_total" "$use_race" <<'PY'
import json
import sys

path, total, race = sys.argv[1], sys.argv[2], sys.argv[3] == "true"

pkgs = {}
tests = {}
failed = []
with open(path) as fh:
    for line in fh:
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            evt = json.loads(line)
        except json.JSONDecodeError:
            continue
        action = evt.get("Action", "")
        if action not in ("pass", "fail"):
            continue
        pkg = evt.get("Package", "")
        elapsed = evt.get("Elapsed")
        test = evt.get("Test", "")
        if pkg and isinstance(elapsed, (int, float)) and not test:
            # Package-level result (no Test field): its own wall time.
            pkgs[pkg] = pkgs.get(pkg, 0.0) + elapsed
        if test and isinstance(elapsed, (int, float)):
            # Test-level result. Test events also carry Package+Elapsed, so they
            # must NOT be added to the package total above.
            tests[f"{pkg}::{test}"] = elapsed
        if action == "fail":
            failed.append(f"{pkg}::{test}" if test else pkg)

def short(name):
    # Trim the module prefix so lines stay readable.
    return name.replace("github.com/dellarb/mammothmail/", "")

sorted_pkgs = sorted(pkgs.items(), key=lambda kv: kv[1], reverse=True)
sorted_tests = sorted(tests.items(), key=lambda kv: kv[1], reverse=True)

pkg_sum = sum(pkgs.values())

print("Go test timing summary")
print("----------------------")
print(f"Total wall time:  {total}s")
print(f"Package time sum: {pkg_sum:.1f}s across {len(pkgs)} packages, {len(tests)} tests")
print("                  (sum of per-package wall times; runs overlap, so this unit exceeds the tier wall time)")
print(f"Race detector:    {'on' if race else 'off'}")
print()
print("Slowest packages:")
for pkg, secs in sorted_pkgs[:12]:
    print(f"  {secs:8.3f}s  {short(pkg)}")
print()
print("Slowest tests:")
for name, secs in sorted_tests[:20]:
    print(f"  {secs:8.3f}s  {short(name)}")
if failed:
    print()
    print(f"FAILURES ({len(failed)}):")
    for name in failed[:25]:
        print(f"  {short(name)}")
PY

exit "$run_rc"
