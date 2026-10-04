// Package smoke gates deliberately slow tests — process-level smoke checks and
// full-cost crypto round-trips — behind MAILMOOSE_SMOKE=1.
//
// These tests are valuable but expensive: they build a real binary or run a
// production-cost key derivation. Keeping them in the default `unit` tier makes
// that tier pay for them on every run. The `--smoke` tier in tests/run.sh sets
// MAILMOOSE_SMOKE=1 and runs them; CI runs them as an explicit step so coverage
// is never silently lost.
package smoke

import (
	"os"
	"testing"
)

// Require skips t unless MAILMOOSE_SMOKE is set to a non-empty value other than
// "0".
func Require(t *testing.T) {
	t.Helper()
	if !Enabled() {
		t.Skip("slow smoke test; set MAILMOOSE_SMOKE=1 or run ./tests/run.sh --smoke")
	}
}

// Enabled reports whether the smoke tier is selected.
func Enabled() bool {
	v := os.Getenv("MAILMOOSE_SMOKE")
	return v != "" && v != "0"
}
