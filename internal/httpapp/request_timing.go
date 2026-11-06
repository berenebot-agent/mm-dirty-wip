package httpapp

import (
	"log/slog"
	"net/http"
	"time"
)

// slowRequestThreshold is the duration above which a completed request is
// reported. It is a diagnostic line, not a limit: no request is aborted or
// delayed at this duration.
const slowRequestThreshold = 400 * time.Millisecond

// slowRequestExempt reports whether a path is a long-lived stream rather than a
// request that is expected to finish. An open SSE stream or long poll returns
// after minutes by design, so timing it would report every connection as slow
// and bury the renders that matter.
func slowRequestExempt(path string) bool {
	switch path {
	case "/relay",
		"/ui/events/stream",
		"/v1/events/stream",
		"/v1/events/wait",
		"/v1/messages/wait":
		return true
	}
	return false
}

// withRequestTiming reports requests that took longer than slowRequestThreshold
// on the ordinary process log, so an operator sees a slow page with no extra
// configuration. The record carries the method, the path and the status, which
// is enough to attribute a slow page to its handler.
//
// Only the path is logged: flash tokens and one-time secrets travel in the
// query string. Streams are skipped (slowRequestExempt).
func withRequestTiming(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slowRequestExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &timingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if d := time.Since(start); d > slowRequestThreshold {
			log.Info("slow request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", d.Milliseconds())
		}
	})
}

// timingWriter records the status a handler wrote and forwards the optional
// interfaces a handler may assert: Flush for streaming responses and Unwrap so
// http.ResponseController (or another wrapper) can still reach the original.
type timingWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *timingWriter) WriteHeader(code int) {
	if !w.written {
		w.status, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timingWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *timingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *timingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
