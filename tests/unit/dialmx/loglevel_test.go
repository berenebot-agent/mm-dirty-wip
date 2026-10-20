package dialmx_test

import (
	"log/slog"
	"testing"

	"github.com/dellarb/mailmoose/dialmx"
)

// TestLevelFromEnvDefaultIsInfo pins the default: with the knob unset, and for
// any unrecognised value, the receiver logs at Info so the connect/session
// chatter stays hidden.
func TestLevelFromEnvDefaultIsInfo(t *testing.T) {
	t.Setenv(dialmx.EnvLogLevel, "")
	if got := dialmx.LevelFromEnv(); got != slog.LevelInfo {
		t.Fatalf("unset level = %v, want Info", got)
	}
	t.Setenv(dialmx.EnvLogLevel, "verbose")
	if got := dialmx.LevelFromEnv(); got != slog.LevelInfo {
		t.Fatalf("unknown level = %v, want Info", got)
	}
}

// TestLevelFromEnvDebug verifies the one opt-in value lowers the level so the
// per-connection and per-session transport events become visible.
func TestLevelFromEnvDebug(t *testing.T) {
	t.Setenv(dialmx.EnvLogLevel, "debug")
	if got := dialmx.LevelFromEnv(); got != slog.LevelDebug {
		t.Fatalf("debug level = %v, want Debug", got)
	}
	t.Setenv(dialmx.EnvLogLevel, "  DEBUG ")
	if got := dialmx.LevelFromEnv(); got != slog.LevelDebug {
		t.Fatalf("padded debug level = %v, want Debug", got)
	}
}
