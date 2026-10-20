package dialmx

import (
	"log/slog"
	"os"
	"strings"
)

// EnvLogLevel is the environment variable that selects the receiver's log
// verbosity. Unset or any unrecognised value means Info; "debug" additionally
// emits the per-connection and per-session transport chatter (SMTP and HTTPS
// connection open/close, STARTTLS, session hello, reply transport writes) that
// is noise for a managed one-core receiver. The name carries no provider
// secret, so the launcher forwards it to the included child verbatim.
const EnvLogLevel = "DIALMX_LOG_LEVEL"

// LevelFromEnv maps EnvLogLevel to a slog level. Only "debug" lowers the level;
// everything else (including empty) is Info, so an operator sets this solely to
// opt into the verbose connect/session diagnostics.
func LevelFromEnv() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvLogLevel))) {
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}
