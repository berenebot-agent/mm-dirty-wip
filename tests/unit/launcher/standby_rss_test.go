package launcher_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/internal/launcher"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// rssKB reads VmRSS (in KiB) for a pid from /proc.
func rssKB(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		t.Fatalf("read proc status for pid %d: %v", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("parse VmRSS %q: %v", line, err)
		}
		return n
	}
	t.Fatalf("no VmRSS line for pid %d", pid)
	return 0
}

// TestStandbyChildResidentMemory measures the real included receiver binary's
// resident memory while it sits in standby (no listeners bound) and while
// active, using /proc/<pid>/VmRSS. It is skipped unless the receiver binary is
// available (MAILMOOSE_MX_BIN, else tests/logs/mailmoose-mx) and the process is
// root, because the launcher requires root to spawn the child.
func TestStandbyChildResidentMemory(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("launcher requires root to spawn the embedded edge")
	}
	bin := os.Getenv("MAILMOOSE_MX_BIN")
	if bin == "" {
		bin = filepath.Join("..", "..", "..", "tests", "logs", "mailmoose-mx")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("receiver binary not available at %s: %v", bin, err)
	}

	log := slog.New(slog.NewTextHandler(discard{}, nil))
	e, err := launcher.StartStandby(context.Background(), launcher.Spec{
		Binary: bin,
		UID:    0,
		GID:    0,
		Env:    []string{},
	}, log)
	if err != nil {
		t.Fatalf("start standby: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	}()
	pid := e.Pid()
	if pid == 0 {
		t.Fatal("no child pid")
	}

	// Let the Go runtime settle; RSS is read after a short pause so startup
	// allocations are reflected.
	time.Sleep(500 * time.Millisecond)
	standby := rssKB(t, pid)
	t.Logf("standby RSS: %d KiB (%.1f MiB)", standby, float64(standby)/1024)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.Configure(ctx, control.Settings{
		Hostname: "mx.test",
		Secret:   "measure-secret",
		SMTP:     mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 2 << 20},
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	active := rssKB(t, pid)
	t.Logf("active RSS: %d KiB (%.1f MiB)", active, float64(active)/1024)

	if _, err := e.Deactivate(ctx); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	backToStandby := rssKB(t, pid)
	t.Logf("standby again RSS: %d KiB (%.1f MiB)", backToStandby, float64(backToStandby)/1024)

	// Sanity bound: the target is low tens of MB; anything near 100 MB is a
	// gross regression worth failing on. This is deliberately generous to avoid
	// flaking on allocator/runtime variance.
	const limitKB = 100 * 1024
	if standby > limitKB {
		t.Fatalf("standby RSS %d KiB exceeds %d KiB budget", standby, limitKB)
	}
}
