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
	"gatehouse-mail/internal/limits"
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

//go:embed assets/gatehouse.py
var pythonClient []byte

//go:embed assets/gatehouse.sh
var bashClient []byte

//go:embed assets/curl-cookbook.txt
var curlCookbook []byte

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
	m.HandleFunc("POST /ui/inboxes/{id}/external-aliases", s.withSession(s.withCSRF(s.uiCreateExternalAlias)))
	m.HandleFunc("GET /ui/inboxes/{id}/external-aliases/{aliasID}", s.withSession(s.uiExternalAlias))
	m.HandleFunc("POST /ui/inboxes/{id}/external-aliases/{aliasID}/edit", s.withSession(s.withCSRF(s.uiUpdateExternalAlias)))
	m.HandleFunc("POST /ui/inboxes/{id}/external-aliases/{aliasID}/sending", s.withSession(s.withCSRF(s.uiExternalAliasSending)))
	m.HandleFunc("POST /ui/inboxes/{id}/external-aliases/{aliasID}/sending/clear", s.withSession(s.withCSRF(s.uiExternalAliasSendingClear)))
	m.HandleFunc("POST /ui/inboxes/{id}/external-aliases/{aliasID}/delete", s.withSession(s.withCSRF(s.uiDeleteExternalAlias)))
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
	m.HandleFunc("GET /docs", s.docsRedirect)
	m.HandleFunc("GET /openapi.json", s.openapi)
	// Versioned health path so /v1/health and /health agree. Unauthenticated
	// and deliberately outside v1Routes, so it is not part of the bearer API.
	m.HandleFunc("GET /v1/health", s.health)
	m.HandleFunc("GET /examples/python", s.pythonExample)
	m.HandleFunc("GET /examples/bash", s.bashExample)
	m.HandleFunc("GET /examples/curl", s.curlExample)

	// Authenticated agent API. The single registration table is also the
	// authoritative surface checked against internal/apispec by the route
	// coverage test; never register a /v1 route outside it.
	api := func(h http.HandlerFunc) http.HandlerFunc { return s.withBearer(h) }
	for _, rt := range v1Routes {
		m.HandleFunc(rt.pattern, api(rt.bind(s)))
	}

	return s.httpsRedirect(s.securityHeaders(s.recoverer(m)))
}

// apiRoute is one authenticated API registration: a ServeMux pattern and the
// method value that serves it.
type apiRoute struct {
	pattern string
	handler func(*Server, http.ResponseWriter, *http.Request)
}

// bind turns a method expression into the handler for one server instance.
func (rt apiRoute) bind(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { rt.handler(s, w, r) }
}

// v1Routes is the single registration list for the authenticated API. It is
// the code-side half of the auto-discovery contract: internal/apispec holds the
// documentation table and a test fails when the two disagree.
var v1Routes = []apiRoute{
	{"GET /v1/bootstrap", (*Server).apiBootstrap},
	{"GET /v1/limits", (*Server).apiLimits},
	{"GET /v1/inboxes", (*Server).apiInboxes},
	{"POST /v1/inboxes", (*Server).apiInboxes},
	{"GET /v1/inboxes/{id}", (*Server).apiInbox},
	{"PATCH /v1/inboxes/{id}", (*Server).apiInbox},
	{"DELETE /v1/inboxes/{id}", (*Server).apiInbox},
	{"GET /v1/admin/inboxes/{id}/external-aliases", (*Server).apiExternalAliases},
	{"POST /v1/admin/inboxes/{id}/external-aliases", (*Server).apiExternalAliases},
	{"PATCH /v1/admin/inboxes/{id}/external-aliases/{aliasID}", (*Server).apiExternalAlias},
	{"DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}", (*Server).apiExternalAlias},
	{"GET /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", (*Server).apiExternalAliasSending},
	{"PUT /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", (*Server).apiExternalAliasSending},
	{"DELETE /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending", (*Server).apiExternalAliasSending},
	{"GET /v1/admin/inboxes/{id}/external-aliases/{aliasID}/sending/deliveries", (*Server).apiExternalAliasDeliveries},
	// openagent.email terminology compatibility.
	{"GET /v1/identities", (*Server).apiIdentities},
	{"POST /v1/identities", (*Server).apiIdentities},
	{"DELETE /v1/identities/{address}", (*Server).apiIdentityDelete},

	{"GET /v1/messages", (*Server).apiMessages},
	{"GET /v1/messages/wait", (*Server).apiMessagesWait},
	{"POST /v1/messages/wait", (*Server).apiMessagesWait},
	{"GET /v1/messages/{id}", (*Server).apiMessage},
	{"PATCH /v1/messages/{id}", (*Server).apiMessage},
	{"DELETE /v1/messages/{id}", (*Server).apiMessage},
	{"POST /v1/messages/{id}/seen", (*Server).apiSeen},
	{"GET /v1/messages/{id}/attachments", (*Server).apiMessageAttachments},
	{"POST /v1/messages/{id}/reply", (*Server).apiReply},
	{"GET /v1/attachments/{id}", (*Server).apiAttachment},
	{"GET /v1/threads", (*Server).apiThreads},
	{"GET /v1/threads/{id}", (*Server).apiThread},
	{"GET /v1/threads/{id}/messages", (*Server).apiThreadMessages},
	{"GET /v1/search", (*Server).apiSearch},
	{"GET /v1/labels", (*Server).apiLabels},
	{"GET /v1/events", (*Server).apiEvents},
	{"GET /v1/events/wait", (*Server).apiEventsWait},
	{"GET /v1/events/stream", (*Server).apiEventsStream},
	{"POST /v1/send", (*Server).apiSend},

	{"GET /v1/drafts", (*Server).apiDrafts},
	{"POST /v1/drafts", (*Server).apiDrafts},
	{"GET /v1/drafts/{id}", (*Server).apiDraft},
	{"PATCH /v1/drafts/{id}", (*Server).apiDraft},
	{"DELETE /v1/drafts/{id}", (*Server).apiDraft},
	{"POST /v1/drafts/{id}/send", (*Server).apiDraftSend},
	{"POST /v1/drafts/{id}/request-send", (*Server).apiDraftRequestSend},
	{"POST /v1/drafts/{id}/cancel-send-request", (*Server).apiDraftCancelSendRequest},
	{"POST /v1/drafts/{id}/approve", (*Server).apiDraftApprove},
	{"POST /v1/drafts/{id}/reject", (*Server).apiDraftReject},
	{"GET /v1/drafts/{id}/send-request", (*Server).apiDraftSendRequest},
	{"POST /v1/drafts/{id}/attachments", (*Server).apiDraftAttachments},
	{"GET /v1/drafts/{id}/attachments", (*Server).apiDraftAttachments},
	{"GET /v1/drafts/{id}/attachments/{attId}", (*Server).apiDraftAttachmentContent},
	{"DELETE /v1/drafts/{id}/attachments/{attId}", (*Server).apiDraftAttachment},
	{"GET /v1/send-requests", (*Server).apiSendRequests},

	{"GET /v1/outbox", (*Server).apiOutbox},
	{"POST /v1/outbox/{id}/retry", (*Server).apiOutboxRetry},
	{"DELETE /v1/outbox/{id}", (*Server).apiOutboxDelete},

	{"GET /v1/admin/domains", (*Server).apiDomains},
	{"POST /v1/admin/domains", (*Server).apiDomains},
	{"PATCH /v1/admin/domains/{id}", (*Server).apiDomain},
	{"DELETE /v1/admin/domains/{id}", (*Server).apiDomain},
	{"GET /v1/admin/keys", (*Server).apiKeys},
	{"POST /v1/admin/keys", (*Server).apiKeys},
	{"DELETE /v1/admin/keys/{id}", (*Server).apiKey},
	{"GET /v1/admin/domains/{id}/sending", (*Server).apiDomainSending},
	{"PUT /v1/admin/domains/{id}/sending", (*Server).apiDomainSending},
	{"DELETE /v1/admin/domains/{id}/sending", (*Server).apiDomainSending},
	{"GET /v1/admin/domains/{id}/receiving", (*Server).apiDomainReceiving},
	{"PUT /v1/admin/domains/{id}/receiving", (*Server).apiDomainReceiving},
	{"DELETE /v1/admin/domains/{id}/receiving", (*Server).apiDomainReceiving},
	{"GET /v1/admin/domains/{id}/sending/deliveries", (*Server).apiDomainSendingDeliveries},
	{"GET /v1/admin/domains/{id}/receiving/deliveries", (*Server).apiDomainReceivingDeliveries},
	{"POST /v1/admin/hermes/enroll", (*Server).apiHermesEnroll},
	{"GET /v1/admin/hermes", (*Server).apiHermesList},
	{"PUT /v1/admin/hermes/{id}", (*Server).apiHermesConnection},
	{"DELETE /v1/admin/hermes/{id}", (*Server).apiHermesDelete},
}

// RegisteredAPIRoutes returns the authenticated /v1 registrations as
// "METHOD /path" strings, for the route coverage test. It is the live half of
// the auto-discovery contract documented in internal/apispec.
func (s *Server) RegisteredAPIRoutes() []string {
	out := make([]string, 0, len(v1Routes))
	for _, rt := range v1Routes {
		out = append(out, rt.pattern)
	}
	return out
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
	m.HandleFunc("GET /healthz", s.health)
	m.HandleFunc("GET /health", s.health)
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

// intQuery parses an integer query parameter. A value that is present but not
// an integer is rejected with 400 rather than silently defaulted; an absent
// value yields def with ok=true.
func intQuery(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, 400, "invalid "+name+": must be an integer")
		return 0, false
	}
	return v, true
}

// limitQuery parses the limit query parameter. It defaults to the standard page
// size and rejects anything that is not an integer of at least 1, so a negative
// or zero limit cannot be silently rewritten to the default.
func limitQuery(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return limits.PageSizeDefault, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, 400, "invalid limit: must be an integer")
		return 0, false
	}
	if v < 1 {
		writeError(w, 400, "invalid limit: must be at least 1")
		return 0, false
	}
	return v, true
}

// boolQuery parses an optional boolean query parameter. A value that is present
// but not a boolean is rejected with 400; an absent value yields nil.
func boolQuery(w http.ResponseWriter, r *http.Request, name string) (*bool, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		writeError(w, 400, "invalid "+name+": must be a boolean")
		return nil, false
	}
	return &b, true
}

// forwardedProto returns the first X-Forwarded-Proto value, lowercased.
func forwardedProto(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
}

// trustForwarded reports whether proxy headers may be believed for this request:
// either trusted globally or sent by a configured trusted proxy.
func (s *Server) trustForwarded(r *http.Request) bool {
	return s.Service.Config.TrustProxyHeaders || s.Service.Config.IsTrustedProxy(r.RemoteAddr)
}

// requestBaseURL derives the public origin a discovery document was fetched
// from, so /openapi.json advertises the host the caller actually reached
// instead of trusting BASE_URL config. It honours X-Forwarded-Proto when proxy
// headers are trusted, forces https when FORCE_HTTPS is set, and falls back to
// the configured BaseURL when the request carries no Host.
func (s *Server) requestBaseURL(r *http.Request) string {
	scheme := "http"
	switch {
	case s.Service.Config.ForceHTTPS:
		scheme = "https"
	case r.TLS != nil:
		scheme = "https"
	case s.trustForwarded(r):
		if p := forwardedProto(r); p != "" {
			scheme = p
		}
	}
	if r.Host == "" {
		return s.Service.Config.BaseURL
	}
	return scheme + "://" + r.Host
}

// httpsRedirect sends plaintext requests to HTTPS when the deployment expects
// TLS (FORCE_HTTPS). It leaves the inbound webhook connector and health checks
// untouched so providers and orchestrators are never redirected.
func (s *Server) httpsRedirect(next http.Handler) http.Handler {
	if !s.Service.Config.ForceHTTPS {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || (s.trustForwarded(r) && forwardedProto(r) == "https") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/health", "/healthz", "/v1/health":
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if host == "" {
			host = strings.TrimPrefix(strings.TrimPrefix(s.Service.Config.BaseURL, "https://"), "http://")
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// health is the liveness response shared by /healthz, /health and /v1/health.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok"})
}

// docsRedirect points the conventional /docs path at the served agent guide.
func (s *Server) docsRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/agent", http.StatusFound)
}

// apiLimits returns the same limits block advertised by discovery, so a client
// that only knows the /v1 surface can still discover the caps.
func (s *Server) apiLimits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.discoveryLimits())
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
