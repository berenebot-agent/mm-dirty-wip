package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/logging"
)

// logLine renders one record through the production handler and returns the
// line without its timestamp prefix, so assertions pin the message and
// attributes rather than the clock.
func logLine(t *testing.T, msg string, args ...any) string {
	t.Helper()
	var buf bytes.Buffer
	logging.New(&buf, slog.LevelInfo, logging.PrefixCore).Info(msg, args...)
	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("log line not newline-terminated: %q", line)
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		t.Fatalf("unexpected log line shape: %q", line)
	}
	// Drop "<timestamp> [Core] INFO " and the trailing newline.
	return strings.TrimSuffix(line[strings.Index(line, "INFO ")+len("INFO "):], "\n")
}

func TestLiteralWritesValueVerbatim(t *testing.T) {
	got := logLine(t, "listening",
		"listener", "receiver-only",
		"origin", "https://gatehouse-email.bw.xiat.net",
		logging.Literal("note", "(Incoming mail webhooks only)"),
	)
	want := "listening listener=receiver-only origin=https://gatehouse-email.bw.xiat.net note=(Incoming mail webhooks only)"
	if got != want {
		t.Fatalf("log line =\n  %q\nwant\n  %q", got, want)
	}
}

func TestStringValuesStillQuotedWhenAmbiguous(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "receiver-only", want: "receiver-only"},
		{name: "space", value: "(Incoming mail webhooks only)", want: `"(Incoming mail webhooks only)"`},
		{name: "empty", value: "", want: `""`},
		{name: "equals", value: "a=b", want: `"a=b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logLine(t, "listening", "value", tc.value)
			want := "listening value=" + tc.want
			if got != want {
				t.Fatalf("log line = %q, want %q", got, want)
			}
		})
	}
}
