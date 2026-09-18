package mxagent_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"gatehouse-mail/internal/mxagent"
)

// testEdgeSecret is a fixed 32-byte (256-bit) hex secret: operator
// MX_EDGE_SECRET values must meet the mxwire entropy bar.
const testEdgeSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestLoadStagingDefaultsAndValidation(t *testing.T) {
	t.Setenv("GATEHOUSE_INGEST_URL", "http://core:8082")
	t.Setenv("MX_EDGE_KEY_ID", "edge")
	t.Setenv("MX_EDGE_SECRET", testEdgeSecret)
	t.Setenv("MX_MAX_MESSAGE_BYTES", "")
	t.Setenv("MX_STAGING_BYTES", "")
	cfg, err := mxagent.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxStagingBytes != 256<<20 {
		t.Fatalf("staging default %d", cfg.MaxStagingBytes)
	}
	// Staging smaller than a single message is a configuration error.
	t.Setenv("MX_MAX_MESSAGE_BYTES", "10485760")
	t.Setenv("MX_STAGING_BYTES", "1048576")
	if _, err := mxagent.Load(); err == nil {
		t.Fatal("expected error when MX_STAGING_BYTES < MX_MAX_MESSAGE_BYTES")
	}
}

func TestLoadRejectsWeakEdgeSecret(t *testing.T) {
	t.Setenv("GATEHOUSE_INGEST_URL", "http://core:8082")
	t.Setenv("MX_EDGE_KEY_ID", "edge")
	for _, weak := range []string{"", "secret", "0123456789abcdef"} {
		t.Setenv("MX_EDGE_SECRET", weak)
		if _, err := mxagent.Load(); err == nil {
			t.Fatalf("weak MX_EDGE_SECRET %q accepted", weak)
		}
	}
}

func TestLoadRequireTLSValidation(t *testing.T) {
	t.Setenv("GATEHOUSE_INGEST_URL", "http://core:8082")
	t.Setenv("MX_EDGE_KEY_ID", "edge")
	t.Setenv("MX_EDGE_SECRET", testEdgeSecret)
	t.Setenv("MX_REQUIRE_TLS", "true")
	t.Setenv("MX_TLS_CERT", "")
	t.Setenv("MX_TLS_KEY", "")
	if _, err := mxagent.Load(); err == nil {
		t.Fatal("MX_REQUIRE_TLS without a certificate must be rejected")
	}
	t.Setenv("MX_TLS_CERT", "/tmp/cert.pem")
	t.Setenv("MX_TLS_KEY", "/tmp/key.pem")
	cfg, err := mxagent.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireTLS {
		t.Fatal("RequireTLS not parsed")
	}
}

func TestWatchShutdownFDCancelsOnEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Put the read end on fd 3 so WatchShutdownFD wraps it, then close the
	// writer to deliver EOF, mirroring the embedded parent closing its pipe.
	if err := syscall.Dup3(int(r.Fd()), 3, 0); err != nil {
		t.Skipf("cannot place pipe on fd 3: %v", err)
	}
	r.Close()
	t.Setenv(mxagent.ShutdownFDEnv, "3")
	ctx, cancel := mxagent.WatchShutdownFD(context.Background())
	defer cancel()
	_ = w.Close()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context not cancelled after EOF")
	}
}

func TestWatchShutdownFDNoEnv(t *testing.T) {
	t.Setenv(mxagent.ShutdownFDEnv, "")
	ctx, cancel := mxagent.WatchShutdownFD(context.Background())
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("context should be live with no fd")
	}
}

func TestWatchShutdownFDInvalid(t *testing.T) {
	t.Setenv(mxagent.ShutdownFDEnv, "not-a-number")
	ctx, cancel := mxagent.WatchShutdownFD(context.Background())
	defer cancel()
	if ctx.Err() != nil {
		t.Fatal("context should be live with an invalid fd")
	}
}
