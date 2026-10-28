// Package launcher builds and supervises the standby MX edge child of the
// single-container mode. cmd/server starts the edge as a child process under
// a separate uid (so it cannot read /data or the app's encryption key) and
// then drops its own privileges; the child is controlled over an inherited
// pipe pair, which is the only cross-uid channel available after the drop.
package launcher

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultAppBinary and DefaultMXBinary are the image install paths.
const (
	DefaultAppBinary = "/usr/local/bin/mammothmail"
	DefaultMXBinary  = "/usr/local/bin/mammothmail-mx"
)

// Spec is everything needed to spawn the embedded edge.
type Spec struct {
	// Binary is the mammothmail-mx executable.
	Binary string
	// UID/GID the edge runs as, separate from the app runtime user.
	UID int
	GID int
	// Env is the fully scrubbed environment for the child.
	Env []string
}

// ResolveBinary returns the edge binary path, honouring an override for tests.
func ResolveBinary() string {
	if v := strings.TrimSpace(os.Getenv("MAMMOTHMAIL_MX_BIN")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		// Prefer a sibling mammothmail-mx next to the running app binary.
		sibling := filepath.Join(filepath.Dir(exe), "mammothmail-mx")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	return DefaultMXBinary
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
