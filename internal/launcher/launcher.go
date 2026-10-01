// Package launcher builds and supervises the embedded MX edge for the
// single-container mode. When MX is enabled, cmd/server starts the edge as a
// child process under a separate uid (so it cannot read /data or the app's
// encryption key), then drops its own privileges. The edge is told to stop by
// closing an inherited pipe, because the dropped parent cannot signal a child
// owned by a different uid.
package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultAppBinary and DefaultMXBinary are the image install paths.
const (
	DefaultAppBinary = "/usr/local/bin/mailmoose"
	DefaultMXBinary  = "/usr/local/bin/mailmoose-mx"
)

// Spec is everything needed to spawn the embedded edge.
type Spec struct {
	// Binary is the mailmoose-mx executable.
	Binary string
	// UID/GID the edge runs as, separate from the app runtime user.
	UID int
	GID int
	// Secret authenticates the core's session to this receiver.
	Secret string
	// Hostname is the SMTP greeting hostname.
	Hostname string
	// Env is the fully scrubbed environment for the child.
	Env []string
}

// ResolveCoreKey preserves an operator key or generates a fresh random key.
func ResolveCoreKey(key string) (string, error) {
	if key != "" {
		return key, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("launcher: generate core key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// EdgeEnv builds the child's environment. It is an allowlist: the edge never
// receives APP_ENCRYPTION_KEY or DATA_DIR. The session listener binds loopback
// and staging is in-memory, so no writable filesystem is needed.
func EdgeEnv(hostname, secret string, shutdownFD int) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"DIALMX_MODE=single",
		"DIALMX_CORE_KEY=" + secret,
		"DIALMX_LISTEN_ADDR=127.0.0.1:8443",
		"MX_LISTEN_ADDR=:2525",
		fmt.Sprintf("MX_SHUTDOWN_FD=%d", shutdownFD),
	}
	if strings.TrimSpace(hostname) == "" {
		hostname = "mailmoose-mx"
	}
	env = append(env, "MX_HOSTNAME="+hostname)
	// Forward the operator-tunable MX edge settings, but never the core's
	// secrets or paths.
	for _, name := range []string{
		"MX_TLS_CERT", "MX_TLS_KEY", "MX_REQUIRE_TLS",
		"MX_VERIFY_SPF", "MX_VERIFY_DKIM", "MX_VERIFY_DMARC",
		"MX_DNS_RESOLVER", "MX_DNS_TIMEOUT_SECONDS",
		"MX_MAX_MESSAGE_BYTES", "MX_STAGING_BYTES",
		"MX_MAX_RECIPIENTS", "MX_MAX_CONNECTIONS",
		"MX_READ_TIMEOUT_SECONDS", "MX_WRITE_TIMEOUT_SECONDS", "MX_DATA_TIMEOUT_SECONDS",
		"MX_HEALTH_ADDR",
	} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// ResolveBinary returns the edge binary path, honouring an override for tests.
func ResolveBinary() string {
	if v := strings.TrimSpace(os.Getenv("MAILMOOSE_MX_BIN")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		// Prefer a sibling mailmoose-mx next to the running app binary.
		sibling := filepath.Join(filepath.Dir(exe), "mailmoose-mx")
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
