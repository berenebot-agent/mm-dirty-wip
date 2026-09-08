package httpapp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-agent-inbox/open-agent-inbox/internal/app"
	"github.com/open-agent-inbox/open-agent-inbox/internal/auth"
	"github.com/open-agent-inbox/open-agent-inbox/internal/hermesrelay"
	"github.com/open-agent-inbox/open-agent-inbox/internal/model"
	"github.com/open-agent-inbox/open-agent-inbox/internal/store"
)

type Server struct {
	Service      *app.Service
	Relay        *hermesrelay.Server
	Log          *slog.Logger
	loginLimiter *limiter
	sendLimiter  *limiter
}

type ctxKey int

const principalKey ctxKey = 1
const csrfKey ctxKey = 2

func New(svc *app.Service, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Service: svc, Relay: hermesrelay.New(svc), Log: log,
		loginLimiter: newLimiter(svc.Config.LoginLimitPerMinute, time.Minute),
		sendLimiter:  newLimiter(svc.Config.SendLimitPerMinute, time.Minute)}
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"status": "ok"}) })
	m.HandleFunc("POST /internal/ingest/mailgun", s.mailgunIngest)
	m.HandleFunc("POST /relay/enroll", s.Relay.Enroll)
	m.HandleFunc("GET /relay", s.Relay.ServeWebSocket)

	// Human UI.
	m.HandleFunc("GET /", s.home)
	m.HandleFunc("GET /setup", s.setupGet)
	m.HandleFunc("POST /setup", s.withPreAuthCSRF(s.setupPost))
	m.HandleFunc("GET /register", s.registerGet)
	m.HandleFunc("POST /register", s.withPreAuthCSRF(s.registerPost))
	m.HandleFunc("GET /login", s.loginGet)
	m.HandleFunc("POST /login", s.withPreAuthCSRF(s.loginPost))
	m.HandleFunc("POST /logout", s.withSession(s.withCSRF(s.logoutPost)))
	m.HandleFunc("GET /dashboard", s.withSession(s.dashboard))
	m.HandleFunc("POST /ui/domains", s.withSession(s.withCSRF(s.uiCreateDomain)))
	m.HandleFunc("POST /ui/domains/{id}/catchall", s.withSession(s.withCSRF(s.uiCatchAll)))
	m.HandleFunc("POST /ui/inboxes", s.withSession(s.withCSRF(s.uiCreateInbox)))
	m.HandleFunc("POST /ui/keys", s.withSession(s.withCSRF(s.uiCreateKey)))
	m.HandleFunc("POST /ui/outbound", s.withSession(s.withCSRF(s.uiOutbound)))
	m.HandleFunc("POST /ui/hermes", s.withSession(s.withCSRF(s.uiHermes)))
	m.HandleFunc("GET /ui/messages/{id}", s.withSession(s.uiMessage))

	// Discovery.
	m.HandleFunc("GET /.well-known/agent-inbox", s.discovery)
	m.HandleFunc("GET /agent", s.agentGuide)
	m.HandleFunc("GET /openapi.json", s.openapi)
	m.HandleFunc("GET /examples/python", s.pythonExample)
	m.HandleFunc("GET /examples/curl", s.curlExample)

	// Authenticated agent API.
	api := func(h http.HandlerFunc) http.HandlerFunc { return s.withBearer(h) }
	m.HandleFunc("GET /v1/bootstrap", api(s.apiBootstrap))
	m.HandleFunc("GET /v1/inboxes", api(s.apiInboxes))
	m.HandleFunc("POST /v1/inboxes", api(s.apiInboxes))
	m.HandleFunc("GET /v1/inboxes/{id}", api(s.apiInbox))
	m.HandleFunc("PATCH /v1/inboxes/{id}", api(s.apiInbox))
	m.HandleFunc("DELETE /v1/inboxes/{id}", api(s.apiInbox))
	// openagent.email terminology compatibility.
	m.HandleFunc("GET /v1/identities", api(s.apiIdentities))
	m.HandleFunc("POST /v1/identities", api(s.apiIdentities))
	m.HandleFunc("DELETE /v1/identities/{address}", api(s.apiIdentityDelete))

	m.HandleFunc("GET /v1/messages", api(s.apiMessages))
	m.HandleFunc("GET /v1/messages/wait", api(s.apiMessagesWait))
	m.HandleFunc("POST /v1/messages/wait", api(s.apiMessagesWait))
	m.HandleFunc("GET /v1/messages/{id}", api(s.apiMessage))
	m.HandleFunc("PATCH /v1/messages/{id}", api(s.apiMessage))
	m.HandleFunc("DELETE /v1/messages/{id}", api(s.apiMessage))
	m.HandleFunc("POST /v1/messages/{id}/seen", api(s.apiSeen))
	m.HandleFunc("GET /v1/messages/{id}/attachments", api(s.apiMessageAttachments))
	m.HandleFunc("POST /v1/messages/{id}/reply", api(s.apiReply))
	m.HandleFunc("GET /v1/attachments/{id}", api(s.apiAttachment))
	m.HandleFunc("GET /v1/threads", api(s.apiThreads))
	m.HandleFunc("GET /v1/threads/{id}", api(s.apiThread))
	m.HandleFunc("GET /v1/threads/{id}/messages", api(s.apiThreadMessages))
	m.HandleFunc("GET /v1/search", api(s.apiSearch))
	m.HandleFunc("GET /v1/events", api(s.apiEvents))
	m.HandleFunc("GET /v1/events/wait", api(s.apiEventsWait))
	m.HandleFunc("GET /v1/events/stream", api(s.apiEventsStream))
	m.HandleFunc("POST /v1/send", api(s.apiSend))

	m.HandleFunc("GET /v1/drafts", api(s.apiDrafts))
	m.HandleFunc("POST /v1/drafts", api(s.apiDrafts))
	m.HandleFunc("GET /v1/drafts/{id}", api(s.apiDraft))
	m.HandleFunc("PATCH /v1/drafts/{id}", api(s.apiDraft))
	m.HandleFunc("DELETE /v1/drafts/{id}", api(s.apiDraft))

	m.HandleFunc("GET /v1/admin/domains", api(s.apiDomains))
	m.HandleFunc("POST /v1/admin/domains", api(s.apiDomains))
	m.HandleFunc("PATCH /v1/admin/domains/{id}", api(s.apiDomain))
	m.HandleFunc("DELETE /v1/admin/domains/{id}", api(s.apiDomain))
	m.HandleFunc("GET /v1/admin/keys", api(s.apiKeys))
	m.HandleFunc("POST /v1/admin/keys", api(s.apiKeys))
	m.HandleFunc("DELETE /v1/admin/keys/{id}", api(s.apiKey))
	m.HandleFunc("GET /v1/admin/outbound", api(s.apiOutbound))
	m.HandleFunc("POST /v1/admin/outbound", api(s.apiOutbound))
	m.HandleFunc("DELETE /v1/admin/outbound/{id}", api(s.apiOutboundDelete))
	m.HandleFunc("POST /v1/admin/hermes/enroll", api(s.apiHermesEnroll))
	m.HandleFunc("GET /v1/admin/hermes", api(s.apiHermesList))
	m.HandleFunc("DELETE /v1/admin/hermes/{id}", api(s.apiHermesDelete))

	return s.securityHeaders(s.recoverer(m))
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if x := recover(); x != nil {
				s.Log.Error("panic", "error", x)
				writeError(w, 500, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func principal(r *http.Request) model.Principal {
	v, _ := r.Context().Value(principalKey).(model.Principal)
	return v
}
func csrf(r *http.Request) string { v, _ := r.Context().Value(csrfKey).(string); return v }

func (s *Server) setPreAuthCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("oai_csrf"); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok, err := auth.RandomToken(24)
	if err != nil {
		return ""
	}
	secure := strings.HasPrefix(strings.ToLower(s.Service.Config.BaseURL), "https://")
	http.SetCookie(w, &http.Cookie{Name: "oai_csrf", Value: tok, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 3600})
	return tok
}
func preAuthCSRF(r *http.Request) string {
	c, err := r.Cookie("oai_csrf")
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *Server) withPreAuthCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got := r.Form.Get("_csrf")
		want := preAuthCSRF(r)
		if got == "" || want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) withBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
			writeError(w, 401, "bearer API key required")
			return
		}
		p, err := s.Service.Store.APIKeyPrincipal(r.Context(), strings.TrimSpace(h[7:]))
		if err != nil {
			writeError(w, 401, "invalid API key")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	}
}

func (s *Server) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("oai_session")
		if err != nil {
			redirectLogin(w, r)
			return
		}
		p, cval, err := s.Service.Store.SessionPrincipal(r.Context(), c.Value)
		if err != nil {
			redirectLogin(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, p)
		ctx = context.WithValue(ctx, csrfKey, cval)
		next(w, r.WithContext(ctx))
	}
}
func (s *Server) withCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got := r.Form.Get("_csrf")
		if got == "" {
			got = r.Header.Get("X-CSRF-Token")
		}
		want := csrf(r)
		if len(got) == 0 || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := strings.HasPrefix(strings.ToLower(s.Service.Config.BaseURL), "https://")
	http.SetCookie(w, &http.Cookie{Name: "oai_session", Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(s.Service.Config.SessionTTL.Seconds())})
}
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "oai_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
func redirectLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not found")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, 403, "forbidden")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "conflict")
	case errors.Is(err, store.ErrQuota):
		writeError(w, 507, "storage quota exceeded")
	default:
		writeError(w, 400, err.Error())
	}
}
func adminOnly(w http.ResponseWriter, p model.Principal) bool {
	if !p.Admin {
		writeError(w, 403, "admin required")
		return false
	}
	return true
}
func intParam(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return v
}
func boolQuery(r *http.Request, name string) *bool {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil
	}
	return &b
}

func clientIP(r *http.Request, trust bool) string {
	if trust {
		if x := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); x != "" {
			return x
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*limitEntry
}
type limitEntry struct {
	start time.Time
	n     int
}

func newLimiter(max int, w time.Duration) *limiter {
	if max <= 0 {
		max = 1 << 30
	}
	return &limiter{max: max, window: w, m: map[string]*limitEntry{}}
}
func (l *limiter) Allow(k string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.m[k]
	if e == nil || now.Sub(e.start) >= l.window {
		l.m[k] = &limitEntry{start: now, n: 1}
		return true
	}
	if e.n >= l.max {
		return false
	}
	e.n++
	return true
}

func baseWSURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/")
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/relay"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func idemKey(r *http.Request) string { return strings.TrimSpace(r.Header.Get("Idempotency-Key")) }
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
