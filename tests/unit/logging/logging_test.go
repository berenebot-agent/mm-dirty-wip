package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gatehouse-mail/internal/logging"
)

func TestPrefixAndJSON(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo, logging.PrefixCore)
	log.Info("hello", "k", "v")

	line := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, line)
	}
	msg, _ := rec["msg"].(string)
	if !strings.HasPrefix(msg, "[Core] ") {
		t.Fatalf("missing core prefix: %q", msg)
	}
	if rec["k"] != "v" {
		t.Fatalf("attribute lost: %v", rec)
	}
}

func TestMXPrefix(t *testing.T) {
	var buf bytes.Buffer
	logging.New(&buf, slog.LevelInfo, logging.PrefixMX).Warn("deferred")
	if !strings.Contains(buf.String(), mxTag) {
		t.Fatalf("missing mx prefix: %s", buf.String())
	}
}

func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(&buf, slog.LevelInfo, logging.PrefixCore)
	log.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug record should be filtered: %s", buf.String())
	}
}

const mxTag = `"[MX] deferred"`
