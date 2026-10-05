// Package logging builds the process loggers used by the core and the MX edge.
// Both write to the same container stream in the embedded deployment, so every
// record is prefixed with a short service tag ("[Core]" or "[MX]") that keeps
// the two services distinguishable. Output is compact plain text, one line per
// record.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// Prefix tags. They are the only thing that separates the core's and the edge's
// lines when both write to one container's stdout/stderr.
const (
	PrefixCore = "[Core]"
	PrefixMX   = "[MX]"
)

// timeLayout is the fixed-width UTC timestamp that leads every line.
const timeLayout = "2006-01-02T15:04:05"

// textHandler renders each record as one line:
//
//	2026-09-13T15:04:40 [Core] INFO message key=value ...
//
// It serialises writes so concurrent records cannot interleave.
type textHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	level  slog.Level
	tag    string
	attrs  []slog.Attr
	groups []string
}

// New returns a compact text logger writing to w at the given level, with every
// line prefixed by tag.
func New(w io.Writer, level slog.Level, tag string) *slog.Logger {
	return slog.New(&textHandler{mu: &sync.Mutex{}, w: w, level: level, tag: tag})
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(timeLayout))
	b.WriteByte(' ')
	b.WriteString(h.tag)
	b.WriteByte(' ')
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	prefix := strings.Join(h.groups, ".")
	for _, a := range h.attrs {
		h.appendAttr(&b, prefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&b, prefix, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

// appendAttr writes one "key=value" pair, flattening groups with a dotted key.
func (h *textHandler) appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		next := prefix
		if a.Key != "" {
			if next != "" {
				next += "."
			}
			next += a.Key
		}
		for _, ga := range a.Value.Group() {
			h.appendAttr(b, next, ga)
		}
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + a.Key
	}
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(formatValue(a.Value))
}

// literal marks a value that is already formatted for a human reader, so
// formatValue writes it verbatim instead of quoting it as a string.
type literal string

// Literal returns an attribute written verbatim. formatValue quotes string
// values containing spaces, so a short human-facing note would otherwise come
// out quoted; use it only for text that is already final for the log reader.
func Literal(key, value string) slog.Attr {
	return slog.Any(key, literal(value))
}

// formatValue renders an attribute value, quoting strings only when a bare
// token would be ambiguous (empty, whitespace, '=' or a quote).
func formatValue(v slog.Value) string {
	if v.Kind() == slog.KindAny {
		if lit, ok := v.Any().(literal); ok {
			return string(lit)
		}
	}
	switch v.Kind() {
	case slog.KindString:
		return quoteIfNeeded(v.String())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(timeLayout)
	default:
		return quoteIfNeeded(fmt.Sprintf("%v", v.Any()))
	}
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r <= ' ' || r == '=' || r == '"' || r == '\\' {
			return strconv.Quote(s)
		}
	}
	return s
}
