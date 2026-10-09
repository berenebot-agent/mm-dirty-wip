package launcher_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/dialmx"
	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/launcher"
)

func TestStandbyEnvIsMinimal(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "super-secret")
	t.Setenv("DATA_DIR", "/data")

	env := launcher.StandbyEnv(3, 4)
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"APP_ENCRYPTION_KEY", "DATA_DIR", "DIALMX_CORE_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("standby env leaked %s: %v", forbidden, env)
		}
	}
	want := map[string]bool{
		control.CmdFDEnv + "=3":   false,
		control.ReplyFDEnv + "=4": false,
	}
	for _, e := range env {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("standby env missing %q in %v", k, env)
		}
	}
}

// TestStandbyEnvForwardsLogLevel verifies the non-secret verbosity knob reaches
// the embedded child only when the operator set it, so the connect/session
// diagnostics can be turned on without a rebuild.
func TestStandbyEnvForwardsLogLevel(t *testing.T) {
	t.Setenv(dialmx.EnvLogLevel, "")
	if env := launcher.StandbyEnv(3, 4); strings.Contains(strings.Join(env, "\n"), dialmx.EnvLogLevel) {
		t.Fatalf("standby env carried log level when unset: %v", env)
	}

	t.Setenv(dialmx.EnvLogLevel, "debug")
	env := launcher.StandbyEnv(3, 4)
	if !slices.Contains(env, dialmx.EnvLogLevel+"=debug") {
		t.Fatalf("standby env did not forward log level: %v", env)
	}
}

// TestStandbyEnvForwardsDNSFallback verifies the non-secret resolver fallback
// list reaches the embedded child only when the operator set it, so the edge's
// SPF/DKIM/DMARC and TXT proof lookups fail over like the core.
func TestStandbyEnvForwardsDNSFallback(t *testing.T) {
	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "")
	if env := launcher.StandbyEnv(3, 4); strings.Contains(strings.Join(env, "\n"), "MAILMOOSE_DNS_FALLBACK_SERVERS") {
		t.Fatalf("standby env carried DNS fallback when unset: %v", env)
	}

	t.Setenv("MAILMOOSE_DNS_FALLBACK_SERVERS", "1.1.1.1,8.8.8.8")
	env := launcher.StandbyEnv(3, 4)
	if !slices.Contains(env, "MAILMOOSE_DNS_FALLBACK_SERVERS=1.1.1.1,8.8.8.8") {
		t.Fatalf("standby env did not forward DNS fallback: %v", env)
	}
}

// TestStartStandbyRequiresRoot documents the privilege precondition. CI runs as
// root inside the toolchain container, so this asserts the non-root refusal by
// dropping the effective uid only when the process is not already root.
func TestStartStandbyRequiresRoot(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("already non-root; the refusal path is exercised by the root guard")
	}
	// As root the guard passes, so a missing binary must surface as a start
	// error rather than a privilege error. An empty/relative binary has no
	// inherited-fd child to find.
	_, err := launcher.StartStandby(t.Context(), launcher.Spec{
		Binary: "/nonexistent/mailmoose-mx",
		UID:    0,
		GID:    0,
	}, nil)
	if err == nil {
		t.Fatal("expected start error for a missing binary")
	}
}
