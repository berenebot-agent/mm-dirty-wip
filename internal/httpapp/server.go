package httpapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
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

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/auth"
	"gatehouse-mail/internal/hermesrelay"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

//go:embed assets/app.js
var appJS []byte

//go:embed assets/cloudflare-worker.js
var cloudflareWorkerTemplate []byte

//go:embed assets/logo-horizontal.png
var logoHorizontalPNG []byte

//go:embed assets/favicon.ico
var faviconICO []byte

//go:embed assets/favicon.svg
var faviconSVG []byte

//go:embed assets/apple-touch-icon.png
var appleTouchIconPNG []byte

type Server struct {
	Service         *app.Service
	Relay           *hermesrelay.Server
	Log             *slog.Logger
	loginLimiter    *limiter
	sendLimiter     *limiter
	unroutedLim     *limiter
	passwordLimiter *limiter
	registerLimiter *limiter
	flashes         *flashStore
	assetVersion    string
	inboundSem      chan struct{}
	mxReplay        *mxReplayCache
}

type ctxKey int

const principalKey ctxKey = 1
const csrfKey ctxKey = 2

func New(svc *app.Service, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	h := sha256.New()
	for _, b := range [][]byte{appJS, logoHorizontalPNG, faviconICO, faviconSVG, appleTouchIconPNG} {
		_, _ = h.Write(b)
	}
	sum := h.Sum(nil)
	conc := svc.Config.InboundConcurrency
	if conc < 1 {
		conc = 32
	}
	return &Server{Service: svc, Relay: hermesrelay.New(svc), Log: log,
		loginLimiter:    newLimiter(svc.Config.LoginLimitPerMinute, time.Minute),
		sendLimiter:     newLimiter(svc.Config.SendLimitPerMinute, time.Minute),
		unroutedLim:     newLimiter(1, time.Minute),
		passwordLimiter: newLimiter(svc.Config.LoginLimitPerMinute, time.Minute),
		registerLimiter: newLimiter(svc.Config.RegisterLimitPerMinute, time.Minute),
		flashes:         newFlashStore(64, 64<<20),
		assetVersion:    fmt.Sprintf("%x", sum[:6]),
		inboundSem:      make(chan struct{}, conc)}
}

// assetURL returns a content-hashed asset path so a rebuilt binary always
// serves fresh JS instead of a stale browser cache.
func (s *Server) assetURL(name string) string {
	return "/assets/" + name + "?v=" + s.assetVersion
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	s.registerInbound(m)
	m.HandleFunc("GET /assets/app.js", s.asset)
	m.HandleFunc("GET /assets/logo-horizontal.png", s.showLogo)
	m.HandleFunc("GET /favicon.ico", s.showFaviconICO)
	m.HandleFunc("GET /favicon.svg", s.showFaviconSVG)
	m.HandleFunc("GET /apple-touch-icon.png", s.showAppleTouchIcon)
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
	m.HandleFunc("GET /account", s.withSession(s.settingsGet))
	m.HandleFunc("POST /ui/account/account", s.withSession(s.withCSRF(s.uiSettingsAccount)))
	m.HandleFunc("POST /ui/account/email", s.withSession(s.withCSRF(s.uiSettingsEmail)))
	m.HandleFunc("POST /ui/account/password", s.withSession(s.withCSRF(s.uiSettingsPassword)))
	m.HandleFunc("POST /ui/domains", s.withSession(s.withCSRF(s.uiCreateDomain)))
	m.HandleFunc("POST /ui/domains/{id}/catchall", s.withSession(s.withCSRF(s.uiDomainCatchAll)))
	m.HandleFunc("POST /ui/domains/{id}/sending", s.withSession(s.withCSRF(s.uiDomainSending)))
	m.HandleFunc("POST /ui/domains/{id}/sending/clear", s.withSession(s.withCSRF(s.uiDomainSendingClear)))
	m.HandleFunc("POST /ui/domains/{id}/receiving", s.withSession(s.withCSRF(s.uiDomainReceiving)))
	m.HandleFunc("POST /ui/domains/{id}/receiving/clear", s.withSession(s.withCSRF(s.uiDomainReceivingClear)))
	m.HandleFunc("POST /ui/domains/{id}/receiving/regenerate", s.withSession(s.withCSRF(s.uiDomainReceivingRegenerate)))
	m.HandleFunc("GET /ui/domains/{id}/sending/deliveries", s.withSession(s.domainDeliveries))
	m.HandleFunc("POST /ui/domains/{id}/delete", s.withSession(s.withCSRF(s.uiDeleteDomain)))
	m.HandleFunc("POST /ui/inboxes", s.withSession(s.withCSRF(s.uiCreateInbox)))
	m.HandleFunc("POST /ui/inboxes/{id}/edit", s.withSession(s.withCSRF(s.uiUpdateInbox)))
	m.HandleFunc("POST /ui/inboxes/{id}/delete", s.withSession(s.withCSRF(s.uiDeleteInbox)))
	m.HandleFunc("POST /ui/keys", s.withSession(s.withCSRF(s.uiCreateKey)))
	m.HandleFunc("POST /ui/keys/{id}/edit", s.withSession(s.withCSRF(s.uiUpdateKey)))
	m.HandleFunc("POST /ui/keys/{id}/rotate", s.withSession(s.withCSRF(s.uiRotateKey)))
	m.HandleFunc("POST /ui/keys/{id}/delete", s.withSession(s.withCSRF(s.uiDeleteKey)))
	m.HandleFunc("POST /ui/hermes/{id}/edit", s.withSession(s.withCSRF(s.uiUpdateHermes)))
	m.HandleFunc("POST /ui/hermes/{id}/delete", s.withSession(s.withCSRF(s.uiDeleteHermes)))
	m.HandleFunc("GET /ui/messages/{id}", s.withSession(s.uiMessage))
	m.HandleFunc("GET /ui/inboxes/{id}", s.withSession(s.uiInbox))
	m.HandleFunc("GET /ui/inboxes/{id}/sent", s.withSession(s.uiSent))
	m.HandleFunc("GET /ui/inboxes/{id}/spam", s.withSession(s.uiSpam))
	m.HandleFunc("GET /ui/inboxes/{id}/drafts", s.withSession(s.uiDrafts))
	m.HandleFunc("GET /ui/inboxes/{id}/drafts/{draftId}/edit", s.withSession(s.uiDraftEdit))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/save", s.withSession(s.withCSRF(s.uiDraftSave)))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/delete", s.withSession(s.withCSRF(s.uiDraftDelete)))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/request-send", s.withSession(s.withCSRF(s.uiDraftRequestSend)))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/approve", s.withSession(s.withCSRF(s.uiDraftApprove)))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/reject", s.withSession(s.withCSRF(s.uiDraftReject)))
	m.HandleFunc("POST /ui/inboxes/{id}/drafts/{draftId}/cancel-send-request", s.withSession(s.withCSRF(s.uiDraftCancelSendRequest)))
	m.HandleFunc("GET /ui/inboxes/{id}/outbox", s.withSession(s.uiOutbox))
	m.HandleFunc("POST /ui/inboxes/{id}/outbox/{msgId}/retry", s.withSession(s.withCSRF(s.uiOutboxRetry)))
	m.HandleFunc("POST /ui/inboxes/{id}/outbox/{msgId}/delete", s.withSession(s.withCSRF(s.uiOutboxDelete)))
	m.HandleFunc("GET /ui/inboxes/{id}/compose", s.withSession(s.uiCompose))
	m.HandleFunc("POST /ui/inboxes/{id}/send", s.withSession(s.withCSRF(s.uiComposeSend)))
	m.HandleFunc("POST /ui/inboxes/{id}/bulk", s.withSession(s.withCSRF(s.uiBulk)))
	m.HandleFunc("GET /ui/messages/{id}/reply", s.withSession(s.uiReplyForm))
	m.HandleFunc("POST /ui/messages/{id}/reply", s.withSession(s.withCSRF(s.uiReplySend)))
	m.HandleFunc("GET /ui/messages/{id}/forward", s.withSession(s.uiForwardForm))
	m.HandleFunc("POST /ui/messages/{id}/forward", s.withSession(s.withCSRF(s.uiForwardSend)))
	m.HandleFunc("POST /ui/messages/{id}/delete", s.withSession(s.withCSRF(s.uiMessageDelete)))
	m.HandleFunc("POST /ui/messages/{id}/read", s.withSession(s.withCSRF(s.uiMessageRead)))
	m.HandleFunc("POST /ui/messages/{id}/spam", s.withSession(s.withCSRF(s.uiMessageSpam)))
	m.HandleFunc("POST /ui/messages/{id}/labels", s.withSession(s.withCSRF(s.uiMessageLabels)))
	m.HandleFunc("GET /ui/messages/{id}/html", s.withSession(s.uiMessageHTML))
	m.HandleFunc("GET /ui/attachments/{id}", s.withSession(s.uiAttachment))
	m.HandleFunc("GET /ui/attachments/{id}/inline", s.withSession(s.uiAttachmentInline))

	// Discovery.
	m.HandleFunc("GET /.well-known/gatehouse", s.discovery)
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
	m.HandleFunc("GET /v1/labels", api(s.apiLabels))
	m.HandleFunc("GET /v1/events", api(s.apiEvents))
	m.HandleFunc("GET /v1/events/wait", api(s.apiEventsWait))
	m.HandleFunc("GET /v1/events/stream", api(s.apiEventsStream))
	m.HandleFunc("POST /v1/send", api(s.apiSend))

	m.HandleFunc("GET /v1/drafts", api(s.apiDrafts))
	m.HandleFunc("POST /v1/drafts", api(s.apiDrafts))
	m.HandleFunc("GET /v1/drafts/{id}", api(s.apiDraft))
	m.HandleFunc("PATCH /v1/drafts/{id}", api(s.apiDraft))
	m.HandleFunc("DELETE /v1/drafts/{id}", api(s.apiDraft))
	m.HandleFunc("POST /v1/drafts/{id}/send", api(s.apiDraftSend))
	m.HandleFunc("POST /v1/drafts/{id}/request-send", api(s.apiDraftRequestSend))
	m.HandleFunc("POST /v1/drafts/{id}/cancel-send-request", api(s.apiDraftCancelSendRequest))
	m.HandleFunc("POST /v1/drafts/{id}/approve", api(s.apiDraftApprove))
	m.HandleFunc("POST /v1/drafts/{id}/reject", api(s.apiDraftReject))
	m.HandleFunc("GET /v1/drafts/{id}/send-request", api(s.apiDraftSendRequest))
	m.HandleFunc("POST /v1/drafts/{id}/attachments", api(s.apiDraftAttachments))
	m.HandleFunc("GET /v1/drafts/{id}/attachments", api(s.apiDraftAttachments))
	m.HandleFunc("DELETE /v1/drafts/{id}/attachments/{attId}", api(s.apiDraftAttachment))
	m.HandleFunc("GET /v1/send-requests", api(s.apiSendRequests))

	m.HandleFunc("GET /v1/outbox", api(s.apiOutbox))
	m.HandleFunc("POST /v1/outbox/{id}/retry", api(s.apiOutboxRetry))
	m.HandleFunc("DELETE /v1/outbox/{id}", api(s.apiOutboxDelete))

	m.HandleFunc("GET /v1/admin/domains", api(s.apiDomains))
	m.HandleFunc("POST /v1/admin/domains", api(s.apiDomains))
	m.HandleFunc("PATCH /v1/admin/domains/{id}", api(s.apiDomain))
	m.HandleFunc("DELETE /v1/admin/domains/{id}", api(s.apiDomain))
	m.HandleFunc("GET /v1/admin/keys", api(s.apiKeys))
	m.HandleFunc("POST /v1/admin/keys", api(s.apiKeys))
	m.HandleFunc("DELETE /v1/admin/keys/{id}", api(s.apiKey))
	m.HandleFunc("GET /v1/admin/domains/{id}/sending", api(s.apiDomainSending))
	m.HandleFunc("PUT /v1/admin/domains/{id}/sending", api(s.apiDomainSending))
	m.HandleFunc("DELETE /v1/admin/domains/{id}/sending", api(s.apiDomainSending))
	m.HandleFunc("GET /v1/admin/domains/{id}/receiving", api(s.apiDomainReceiving))
	m.HandleFunc("PUT /v1/admin/domains/{id}/receiving", api(s.apiDomainReceiving))
	m.HandleFunc("DELETE /v1/admin/domains/{id}/receiving", api(s.apiDomainReceiving))
	m.HandleFunc("GET /v1/admin/domains/{id}/sending/deliveries", api(s.apiDomainSendingDeliveries))
	m.HandleFunc("GET /v1/admin/domains/{id}/receiving/deliveries", api(s.apiDomainReceivingDeliveries))
	m.HandleFunc("POST /v1/admin/hermes/enroll", api(s.apiHermesEnroll))
	m.HandleFunc("GET /v1/admin/hermes", api(s.apiHermesList))
	m.HandleFunc("PUT /v1/admin/hermes/{id}", api(s.apiHermesConnection))
	m.HandleFunc("DELETE /v1/admin/hermes/{id}", api(s.apiHermesDelete))

	return s.securityHeaders(s.recoverer(m))
}

// InboundHandler serves only the health check and the authenticated provider
// webhook ingest routes. An operator can expose a dedicated port for this
// handler so inbound mail providers reach the ingest connector without the
// API, UI, or Relay WebSocket being available on that port.
func (s *Server) InboundHandler() http.Handler {
	m := http.NewServeMux()
	s.registerInbound(m)
	// MX routes are only on the dedicated inbound connector, never the main
	// API/UI listener, so an operator can expose just the connector to the edge.
	s.registerMX(m)
	return s.securityHeaders(s.recoverer(m))
}

func (s *Server) registerInbound(m *http.ServeMux) {
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"status": "ok"}) })
	// Canonical Mailgun receive endpoint. The suffix selects raw MIME delivery.
	m.HandleFunc("POST /internal/ingest/mailgun/raw-mime", s.mailgunIngest)
	m.HandleFunc("POST /internal/ingest/{provider}", s.ingestInbound)
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if x := recover(); x != nil {
				// Log only the panic type: the raw value can embed request data
				// (and therefore secrets), and logs are a common export channel.
				s.Log.Error("panic recovered", "type", fmt.Sprintf("%T", x))
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("Link", `</.well-known/gatehouse>; rel="help"; title="Agents: GET /.well-known/gatehouse for API reference"`)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(appJS)
}

func serveBlob(w http.ResponseWriter, contentType, cacheControl string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = w.Write(body)
}

func (s *Server) showLogo(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "image/png", "public, max-age=31536000, immutable", logoHorizontalPNG)
}
func (s *Server) showFaviconICO(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "image/x-icon", "public, max-age=86400", faviconICO)
}
func (s *Server) showFaviconSVG(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "image/svg+xml", "public, max-age=86400", faviconSVG)
}
func (s *Server) showAppleTouchIcon(w http.ResponseWriter, r *http.Request) {
	serveBlob(w, "image/png", "public, max-age=86400", appleTouchIconPNG)
}

func principal(r *http.Request) model.Principal {
	v, _ := r.Context().Value(principalKey).(model.Principal)
	return v
}
func csrf(r *http.Request) string { v, _ := r.Context().Value(csrfKey).(string); return v }

func (s *Server) setPreAuthCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("ghm_csrf"); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok, err := auth.RandomToken(24)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: "ghm_csrf", Value: tok, Path: "/", HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: 3600})
	return tok
}
func preAuthCSRF(r *http.Request) string {
	c, err := r.Cookie("ghm_csrf")
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
		c, err := r.Cookie("ghm_session")
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
	http.SetCookie(w, &http.Cookie{Name: "ghm_session", Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: int(s.Service.Config.SessionTTL.Seconds())})
}
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "ghm_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode})
}

// cookieSecure marks cookies Secure only when the deployment is HTTPS
// (BaseURL) and the current connection actually arrived over TLS, directly
// or via a trusted proxy. This keeps direct plain-HTTP access (self-hosted
// LAN, healthchecks) working: browsers drop Secure cookies set over HTTP,
// which previously broke setup/login CSRF validation entirely.
func (s *Server) cookieSecure(r *http.Request) bool {
	if !strings.HasPrefix(strings.ToLower(s.Service.Config.BaseURL), "https://") {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if s.Service.Config.IsTrustedProxy(r.RemoteAddr) {
		if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); strings.EqualFold(proto, "https") {
			return true
		}
	}
	return false
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
	return decodeJSONLimit(w, r, v, 2<<20)
}
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
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

// maxTrackedIPs bounds the number of distinct rate-limit keys held at once so
// a caller rotating X-Forwarded-For cannot grow the map without limit.
const maxTrackedIPs = 1 << 16

type limiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	m         map[string]*limitEntry
	lastSweep time.Time
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
	// Opportunistically drop entries whose window has elapsed so long-lived
	// processes do not accumulate one entry per client ever seen.
	if now.Sub(l.lastSweep) >= l.window {
		l.sweepLocked(now)
		l.lastSweep = now
	}
	if e := l.m[k]; e != nil {
		if now.Sub(e.start) >= l.window {
			l.m[k] = &limitEntry{start: now, n: 1}
			return true
		}
		if e.n >= l.max {
			return false
		}
		e.n++
		return true
	}
	if len(l.m) >= maxTrackedIPs {
		l.sweepLocked(now)
		if len(l.m) >= maxTrackedIPs {
			// Fail closed: refuse new keys rather than grow without bound.
			return false
		}
	}
	l.m[k] = &limitEntry{start: now, n: 1}
	return true
}

// sweepLocked removes entries whose window has elapsed. Callers must hold mu.
func (l *limiter) sweepLocked(now time.Time) {
	for k, e := range l.m {
		if now.Sub(e.start) >= l.window {
			delete(l.m, k)
		}
	}
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
