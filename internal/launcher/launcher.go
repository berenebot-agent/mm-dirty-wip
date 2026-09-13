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
	"sort"
	"strings"
)

// DefaultAppBinary and DefaultMXBinary are the image install paths.
const (
	DefaultAppBinary = "/usr/local/bin/gatehouse-mail"
	DefaultMXBinary  = "/usr/local/bin/gatehouse-mx"
)

// Spec is everything needed to spawn the embedded edge.
type Spec struct {
	// Binary is the gatehouse-mx executable.
	Binary string
	// UID/GID the edge runs as, separate from the app runtime user.
	UID int
	GID int
	// KeyID/Secret are the credential the edge signs core requests with. They
	// are derived from the app's MX_EDGE_KEYS when present, or generated.
	KeyID  string
	Secret string
	// Hostname is the SMTP greeting hostname.
	Hostname string
	// Env is the fully scrubbed environment for the child.
	Env []string
}

// ResolveEdgeCredential picks the edge credential. When the operator supplied
// MX_EDGE_KEYS, the lexicographically smallest key id is used (deterministic
// across restarts); otherwise a fresh key is generated. The core is told the
// same credential via EdgeKeysEnv.
func ResolveEdgeCredential(edgeKeys map[string]string) (keyID, secret string) {
	if len(edgeKeys) > 0 {
		ids := make([]string, 0, len(edgeKeys))
		for id := range edgeKeys {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids[0], edgeKeys[ids[0]]
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	return "edge-1", hex.EncodeToString(b[:])
}

// EdgeEnv builds the child's environment. It is an allowlist: the edge never
// receives APP_ENCRYPTION_KEY, DATA_DIR or MX_EDGE_KEYS, and staging is
// in-memory so no filesystem path is needed. GATEHOUSE_INGEST_URL points at the
// core's loopback inbound connector.
func EdgeEnv(hostname, keyID, secret string, shutdownFD int) []string {
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"MX_EDGE_KEY_ID=" + keyID,
		"MX_EDGE_SECRET=" + secret,
		"GATEHOUSE_INGEST_URL=http://127.0.0.1:8082",
		"MX_LISTEN_ADDR=:2525",
		fmt.Sprintf("MX_SHUTDOWN_FD=%d", shutdownFD),
	}
	if strings.TrimSpace(hostname) == "" {
		hostname = "gatehouse-mx"
	}
	env = append(env, "MX_HOSTNAME="+hostname)
	// Forward the operator-tunable MX edge settings, but never the core's
	// secrets or paths.
	for _, name := range []string{
		"MX_EDGE_NAME",
		"MX_TLS_CERT", "MX_TLS_KEY",
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
	if v := strings.TrimSpace(os.Getenv("GATEHOUSE_MX_BIN")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		// Prefer a sibling gatehouse-mx next to the running app binary.
		sibling := filepath.Join(filepath.Dir(exe), "gatehouse-mx")
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
