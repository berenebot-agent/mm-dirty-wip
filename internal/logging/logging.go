// Package logging builds the process loggers used by the core and the MX edge.
// Both write to the same container stream in the embedded deployment, so every
// record is prefixed with a short service tag ("[Core]" or "[MX]") that keeps
// the two services distinguishable. Output is JSON so the whole stream is one
// parseable format.
package logging

import (
	"context"
	"io"
	"log/slog"
)

// Prefix tags. They are the only thing that separates the core's and the edge's
// lines when both write to one container's stdout/stderr.
const (
	PrefixCore = "[Core]"
	PrefixMX   = "[MX]"
)

// prefixHandler wraps a slog handler and prepends a fixed tag to every message.
type prefixHandler struct {
	inner  slog.Handler
	prefix string
}

// New returns a JSON logger writing to w at the given level, with every message
// prefixed by tag.
func New(w io.Writer, level slog.Level, tag string) *slog.Logger {
	inner := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(&prefixHandler{inner: inner, prefix: tag})
}

func (h *prefixHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *prefixHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, h.prefix+" "+r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(a)
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *prefixHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &prefixHandler{inner: h.inner.WithAttrs(attrs), prefix: h.prefix}
}

func (h *prefixHandler) WithGroup(name string) slog.Handler {
	return &prefixHandler{inner: h.inner.WithGroup(name), prefix: h.prefix}
}
