package hermesrelay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/messagepreview"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/ws"
)

// RequireCallerBearer, when set, requires the enrolment request to carry an
// Authorization header with a "Bearer " prefix. It checks only that a bearer
// header is *present* — it does NOT validate the credential's value, so it is
// a minimal spray/scan deterrent and never a substitute for the one-time
// enrolment token, which remains the sole authority for enrolment. Do not
// present it to operators as caller authentication.
type Server struct {
	Service             *app.Service
	Store               *store.Store
	Hub                 *events.Hub
	Log                 *slog.Logger
	RequireCallerBearer bool
	upgrader            ws.Upgrader
	// ackTimeout bounds the wait for an inbound_ack. A matching
	// inbound_processing heartbeat re-arms it, but processingLease is absolute:
	// a slow handler can hold an event for at most that long.
	ackTimeout      time.Duration
	processingLease time.Duration
	// bodyFor fetches a short preview of a remote message the first time it is
	// relayed. It is installed by cmd/server over the remote mailbox service; nil
	// means remote events carry only their metadata summary.
	bodyFor func(ctx context.Context, accountID string, arrival store.RemoteArrival) (string, bool)
}

// relayQuietReconnectWindow suppresses Info-level spam from the idle-timeout
// reconnect loop: a gateway that connected recently logs at Debug instead.
const relayQuietReconnectWindow = 10 * time.Minute

const (
	defaultRelayAckTimeout   = 60 * time.Second
	defaultProcessingLease   = 30 * time.Minute
	relayHeartbeatBufferSize = 8
	// remoteBodyTimeout bounds the one-off preview fetch for a remote event, so a
	// slow mailbox cannot delay the relay.
	remoteBodyTimeout = 8 * time.Second
	// remoteBodyPlaceholder is the metadata-only body used when no preview could
	// be fetched.
	remoteBodyPlaceholder = "(remote message; fetch it via the API)"
)

// maxConcurrentOutbound bounds in-flight outbound operations per relay socket,
// so a gateway cannot force unbounded goroutine and SQLite pressure by bursting
// outbound frames.
const maxConcurrentOutbound = 16

func New(svc *app.Service) *Server {
	return &Server{
		Service:             svc,
		Store:               svc.Store,
		Hub:                 svc.Hub,
		Log:                 svc.Log,
		RequireCallerBearer: svc.Config.RelayRequireBearer,
		// Origin is deliberately not checked. Relay auth is the connection's
		// bearer secret plus an optional caller bearer, never an ambient
		// cookie, so a cross-origin WebSocket cannot ride a logged-in session.
		// The upgrader's same-origin default would instead reject legitimate
		// cross-origin gateways, which have no browser origin to present.
		upgrader:        ws.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		ackTimeout:      defaultRelayAckTimeout,
		processingLease: defaultProcessingLease,
	}
}

// SetAckTimeoutsForTest shortens the acknowledgement window and processing
// lease so a test can exercise heartbeat extension and its cap without waiting
// the production durations.
func (s *Server) SetAckTimeoutsForTest(ackTimeout, processingLease time.Duration) {
	if ackTimeout > 0 {
		s.ackTimeout = ackTimeout
	}
	if processingLease > 0 {
		s.processingLease = processingLease
	}
}

// SetRemoteBody installs the source of a short preview body for relayed remote
// arrivals. It is set once at startup.
func (s *Server) SetRemoteBody(fn func(ctx context.Context, accountID string, arrival store.RemoteArrival) (string, bool)) {
	s.bodyFor = fn
}

// SetRemoteBodyForTest installs a deterministic remote preview body source.
func (s *Server) SetRemoteBodyForTest(fn func(ctx context.Context, accountID string, arrival store.RemoteArrival) (string, bool)) {
	s.bodyFor = fn
}

// ackWait is one event's acknowledgement window. The timer is re-armed by a
// matching processing heartbeat, but never past lease, which is fixed when the
// event was sent.
type ackWait struct {
	timer      *time.Timer
	ackTimeout time.Duration
	lease      time.Time
}

func newAckWait(ackTimeout, lease time.Duration) *ackWait {
	return &ackWait{timer: time.NewTimer(ackTimeout), ackTimeout: ackTimeout, lease: time.Now().Add(lease)}
}

func (w *ackWait) stop() { w.timer.Stop() }

// extend re-arms the window for a live handler. It reports false once the fixed
// processing lease has elapsed, at which point the caller must stop waiting.
func (w *ackWait) extend() bool {
	remaining := time.Until(w.lease)
	if remaining <= 0 {
		return false
	}
	wait := w.ackTimeout
	if remaining < wait {
		wait = remaining
	}
	if !w.timer.Stop() {
		select {
		case <-w.timer.C:
		default:
		}
	}
	w.timer.Reset(wait)
	return true
}

type EnrollRequest struct {
	EnrollmentToken string `json:"enrollmentToken"`
	GatewayID       string `json:"gatewayId"`
}

type EnrollResponse struct {
	Secret    string `json:"secret"`
	Tenant    string `json:"tenant"`
	GatewayID string `json:"gatewayId"`
	// Kind echoes the connector kind the redeemed setup code created:
	// "hermes" or "openclaw". Older claimants may ignore it.
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
}

// Enroll implements the connector side of `hermes gateway enroll`.
// The one-time token is the authority for enrollment. The caller
// Authorization header can additionally be required to be present.
func (s *Server) Enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.RequireCallerBearer && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "authorization required", http.StatusUnauthorized)
		return
	}
	var req EnrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	req.EnrollmentToken = strings.TrimSpace(req.EnrollmentToken)
	req.GatewayID = strings.TrimSpace(req.GatewayID)
	if req.EnrollmentToken == "" || req.GatewayID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enrollmentToken and gatewayId are required"})
		return
	}
	secret, err := auth.RandomToken(32)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	secEnc, err := s.Service.EncryptSecretAAD(app.RelaySecretAAD(req.GatewayID), []byte(secret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	// The token is consumed and the connection created/replaced in one
	// transaction; a gateway owned by another account is rejected without
	// burning the token.
	conn, err := s.Store.EnrollHermesConnection(r.Context(), req.EnrollmentToken, req.GatewayID, secEnc, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired enrollment token"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	s.Log.Info("relay enrolled", "gateway_id", conn.GatewayID, "account_id", conn.AccountID, "inbox_id", conn.InboxID, "kind", conn.Kind)
	writeJSON(w, http.StatusOK, EnrollResponse{Secret: secret, Tenant: conn.AccountID, GatewayID: req.GatewayID, Kind: conn.Kind, Name: conn.Name})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type upgradeToken struct {
	GatewayID string
	Exp       int64
}

func parseUpgradeToken(token string, secret []byte) (upgradeToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return upgradeToken{}, err
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 {
		return upgradeToken{}, errors.New("invalid relay token")
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return upgradeToken{}, err
	}
	msg := parts[0] + ":" + parts[1]
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(msg))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(parts[2])), []byte(expected)) {
		return upgradeToken{}, errors.New("invalid relay signature")
	}
	if time.Now().Unix() > exp || exp > time.Now().Add(15*time.Minute).Unix() {
		return upgradeToken{}, errors.New("expired relay token")
	}
	return upgradeToken{GatewayID: parts[0], Exp: exp}, nil
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ServeWebSocket accepts the outbound WebSocket opened by Hermes Gateway.
func (s *Server) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	tok := bearerToken(r)
	if tok == "" {
		http.Error(w, "missing relay token", http.StatusUnauthorized)
		return
	}
	// Decode gateway id before signature verification so we can locate its secret.
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		http.Error(w, "invalid relay token", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 {
		http.Error(w, "invalid relay token", http.StatusUnauthorized)
		return
	}
	h, err := s.Store.GetHermesConnectionByGateway(r.Context(), parts[0])
	if err != nil {
		http.Error(w, "unknown gateway", http.StatusUnauthorized)
		return
	}
	secret, err := s.Service.DecryptSecretAAD(app.RelaySecretAAD(h.GatewayID), h.SecretEncrypted)
	if err != nil {
		http.Error(w, "relay credential error", http.StatusUnauthorized)
		return
	}
	parsed, err := parseUpgradeToken(tok, secret)
	if err != nil || parsed.GatewayID != h.GatewayID {
		http.Error(w, "invalid relay token", http.StatusUnauthorized)
		return
	}
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	s.Store.MarkHermesConnected(r.Context(), h.ID)
	if h.LastConnectedAt != nil && time.Since(*h.LastConnectedAt) < relayQuietReconnectWindow {
		s.Log.Debug("relay reconnected", "gateway_id", h.GatewayID, "inbox_id", h.InboxID)
	} else {
		s.Log.Info("relay connected", "gateway_id", h.GatewayID, "inbox_id", h.InboxID)
	}
	_ = s.run(r.Context(), c, h)
}

type socketWriter struct {
	mu sync.Mutex
	c  *ws.Conn
}

func (w *socketWriter) JSON(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.WriteJSON(v)
}

type wireFrame struct {
	Type      string          `json:"type"`
	Platform  string          `json:"platform,omitempty"`
	BotID     string          `json:"botId,omitempty"`
	BufferID  string          `json:"bufferId,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	Action    json.RawMessage `json:"action,omitempty"`
}

type outboundAction struct {
	Op       string `json:"op"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id,omitempty"`
	// ReplyToMessageID identifies the exact inbound mail the agent is answering
	// (a local message id or a remote arrival/metadata id). Older gateways omit
	// it; the server then keeps the existing local-thread resolution.
	ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	Content          string `json:"content,omitempty"`
	Text             string `json:"text,omitempty"`
}

func descriptor() map[string]any {
	return map[string]any{
		"type": "descriptor",
		"descriptor": map[string]any{
			"contract_version": 1, "platform": "email", "label": "MailMoose", "emoji": "✉️",
			"platform_hint": "Email", "max_message_length": 100000, "supports_draft_streaming": false,
			"supports_edit": false, "supports_threads": true, "markdown_dialect": "plain", "len_unit": "chars",
			"supported_ops": []string{"send", "typing"},
		},
	}
}

func (s *Server) run(ctx context.Context, c *ws.Conn, h store.HermesConnection) error {
	// The read loop spawns outbound goroutines that touch SQLite and the socket.
	// Wait for them to settle before returning, so a late outbound write never
	// races the connection teardown and an in-flight send is never silently
	// abandoned mid-unwind. Their context is cancelled by the loop's teardown,
	// so the join is bounded by individual op unwinding only.
	var outboundWG sync.WaitGroup
	err := s.runLoop(ctx, c, h, &outboundWG)
	outboundWG.Wait()
	return err
}

func (s *Server) runLoop(ctx context.Context, c *ws.Conn, h store.HermesConnection, outboundWG *sync.WaitGroup) error {
	// Deleting the connection cancels this socket immediately.
	scopeCtx, unregister := s.Hub.RegisterScope("hrm:" + h.ID)
	defer unregister()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(scopeCtx, cancel)
	defer stop()
	wr := &socketWriter{c: c}
	// ackCh buffers gateway acknowledgements for the delivery loop. 64 covers a
	// burst of acks (one per delivered event) without the read loop stalling or
	// dropping them silently; the durable cursor remains the safety net.
	ackCh := make(chan int64, 64)
	// beatCh carries inbound_processing heartbeats. They only re-arm the current
	// event's acknowledgement window; they never advance the durable cursor.
	beatCh := make(chan int64, relayHeartbeatBufferSize)
	helloCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	// Bound concurrent outbound operations per socket. Each outbound op spawns a
	// goroutine that touches SQLite and may run a full send pipeline; without a
	// cap a gateway could burst thousands of frames and exhaust the process.
	outboundSem := make(chan struct{}, maxConcurrentOutbound)
	go func() {
		defer cancel()
		for {
			_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
			var f wireFrame
			if err := c.ReadJSON(&f); err != nil {
				errCh <- err
				return
			}
			switch f.Type {
			case "hello":
				if f.Platform != "" && f.Platform != "email" {
					errCh <- fmt.Errorf("unexpected platform %q", f.Platform)
					return
				}
				if err := wr.JSON(descriptor()); err != nil {
					errCh <- err
					return
				}
				select {
				case helloCh <- struct{}{}:
				default:
				}
			case "inbound_ack":
				// Forward the acknowledgement to the delivery loop, which is the
				// single owner of the durable cursor advance. The reader must not
				// write the cursor itself: a gateway that acks a buffer id ahead
				// of the event actually delivered would otherwise skip every
				// event in between.
				if id := store.ParseCursor(f.BufferID); id > 0 {
					select {
					case ackCh <- id:
					default:
					}
				}
			case "inbound_processing":
				// A handler heartbeat. The delivery loop matches it against the
				// event it is waiting on; a stale or unknown id changes nothing.
				if id := store.ParseCursor(f.BufferID); id > 0 {
					select {
					case beatCh <- id:
					default:
					}
				}
			case "outbound":
				var a outboundAction
				if err := json.Unmarshal(f.Action, &a); err != nil {
					_ = wr.JSON(outboundResult(f.RequestID, false, "invalid outbound action", ""))
					continue
				}
				if a.Op != "typing" {
					s.Log.Info("relay outbound", "gateway_id", h.GatewayID, "request_id", f.RequestID, "op", a.Op, "chat_id", a.ChatID)
				}
				// Acquire a slot without blocking the read loop; if the socket
				// already has too many in-flight outbound ops, reject this one
				// instead of spawning another goroutine.
				select {
				case outboundSem <- struct{}{}:
					outboundWG.Add(1)
					go func() {
						defer func() {
							<-outboundSem
							outboundWG.Done()
						}()
						s.handleOutbound(ctx, wr, h, f.RequestID, a)
					}()
				default:
					s.Log.Warn("relay outbound rejected: too many in-flight", "gateway_id", h.GatewayID, "request_id", f.RequestID)
					_ = wr.JSON(outboundResult(f.RequestID, false, "too many in-flight outbound operations", ""))
				}
			case "interrupt":
				// Email sends are short, transactional operations. Interrupt is acknowledged implicitly by the next result.
			}
		}
	}()
	select {
	case <-helloCh:
	case err := <-errCh:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("relay hello timeout")
	case <-ctx.Done():
		return ctx.Err()
	}

	_, sub, cancelSub := s.Hub.Subscribe(32)
	defer cancelSub()
	after := h.LastAckEventID
	for {
		// Revalidate the connection so a direct database deletion also closes
		// the socket, not just the delete handler's scope cancellation.
		if _, err := s.Store.GetHermesConnectionByGateway(ctx, h.GatewayID); err != nil {
			return err
		}
		ev, err := s.Store.NextHermesEvent(ctx, h.ID, after)
		if err == nil {
			m, err := s.Store.GetMessageByID(ctx, h.AccountID, ev.EntityID)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				// The entity is not a locally-persisted message. A remote arrival
				// (a standalone inbox's detected new mail) has no local message
				// row, so resolve it as a remote arrival and deliver a remote-aware
				// event. A remote arrival that is a control message or was skipped
				// is not actionable: advance the cursor past it.
				arrival, aerr := s.Store.GetRemoteArrival(ctx, h.AccountID, ev.EntityID)
				if aerr != nil {
					if !errors.Is(aerr, store.ErrNotFound) {
						return aerr
					}
					// The message was deleted between the event and delivery, so
					// there is nothing to replay. Advance the durable cursor past
					// it; otherwise every reconnect re-reads the same stale event
					// and drops the socket in a tight loop.
					_ = s.Store.AckHermesEvent(ctx, h.ID, ev.ID)
					after = ev.ID
					continue
				}
				if arrival.Control || arrival.DeliveryState == store.RemoteArrivalSkipped || arrival.InboxID != h.InboxID {
					_ = s.Store.AckHermesEvent(ctx, h.ID, ev.ID)
					after = ev.ID
					continue
				}
				if derr := s.deliverRemote(ctx, h, wr, ev, arrival, &after, ackCh, beatCh, errCh); derr != nil {
					return derr
				}
				continue
			}
			// A message that is currently Spam is not delivered as an actionable
			// inbound event: the durable received event is flagged, but Relay
			// revalidates current state so a replayed or stale event cannot push
			// quarantine as fresh mail. Advance the cursor so a skipped Spam
			// message never stalls the relay.
			if m.Spam {
				_ = s.Store.AckHermesEvent(ctx, h.ID, ev.ID)
				after = ev.ID
				continue
			}
			// Record the attempt before writing to the socket. If the socket
			// drops before the gateway acknowledges, the durable cursor is
			// untouched so the event is redelivered, and the log shows the
			// outstanding attempt rather than silently losing it.
			_ = s.Store.RecordHermesDeliveryPending(ctx, h.ID, ev.ID)
			if err := wr.JSON(map[string]any{"type": "inbound", "event": messageEvent(m), "bufferId": ev.Cursor}); err != nil {
				_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, ev.ID, "relay socket closed before acknowledgement")
				return err
			}
			s.Log.Info("relay inbound", "gateway_id", h.GatewayID, "cursor", ev.Cursor, "message_id", m.ID, "from", m.From.Address, "to", m.To)
			if err := s.awaitAcknowledgement(ctx, h, ev.ID, ackCh, beatCh, errCh); err != nil {
				return err
			}
			// The durable cursor advances only to the event actually delivered:
			// a gateway that (buggily or maliciously) acks a future buffer id
			// must not cause the events in between to be skipped.
			after = ev.ID
			if m, mErr := s.Store.ClientDeliveryLog(ctx, h.AccountID, h.ID, 1, ev.ID+1); mErr == nil && len(m) == 1 {
				_ = s.Store.RecordHermesDeliveryAcknowledged(ctx, h.ID, ev.ID, m[0].Attempts)
			} else {
				_ = s.Store.RecordHermesDeliveryAcknowledged(ctx, h.ID, ev.ID, 1)
			}
			s.Log.Info("relay acked", "gateway_id", h.GatewayID, "cursor", ev.Cursor)
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		select {
		case <-sub: // Wake up and query SQLite; the DB remains authoritative.
		case err := <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			if err := wr.JSON(map[string]any{"type": "ping"}); err != nil {
				return err
			}
		}
	}
}

// deliverRemote delivers one remote-arrival event to the gateway with the same
// acknowledge/cursor protocol as a local message: the durable cursor only advances
// after the gateway acknowledges, and the delivery log records the attempt. The
// event is remote-aware and carries the remote locator (so the gateway can fetch
// the full message through the canonical API) plus a short preview of the body.
func (s *Server) deliverRemote(ctx context.Context, h store.HermesConnection, wr *socketWriter, ev model.Event, arrival store.RemoteArrival, after *int64, ackCh, beatCh <-chan int64, errCh <-chan error) error {
	_ = s.Store.RecordHermesDeliveryPending(ctx, h.ID, ev.ID)
	if err := wr.JSON(map[string]any{"type": "inbound", "event": remoteMessageEvent(arrival, s.remoteBody(ctx, h.AccountID, arrival)), "bufferId": ev.Cursor}); err != nil {
		_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, ev.ID, "relay socket closed before acknowledgement")
		return err
	}
	s.Log.Info("relay inbound remote", "gateway_id", h.GatewayID, "cursor", ev.Cursor, "message_id", arrival.ID, "from", arrival.FromAddress, "inbox_id", arrival.InboxID)
	if err := s.awaitAcknowledgement(ctx, h, ev.ID, ackCh, beatCh, errCh); err != nil {
		return err
	}
	*after = ev.ID
	_ = s.Store.RecordHermesDeliveryAcknowledged(ctx, h.ID, ev.ID, 1)
	s.Log.Info("relay acked remote", "gateway_id", h.GatewayID, "cursor", ev.Cursor)
	return nil
}

// awaitAcknowledgement waits for the gateway's acknowledgement of one delivered
// event. A matching inbound_processing heartbeat re-arms the ack timeout so a
// slow handler is not disconnected, but the fixed processing lease caps that
// extension. Heartbeats for any other event are ignored, and this function
// never advances the durable cursor itself.
func (s *Server) awaitAcknowledgement(ctx context.Context, h store.HermesConnection, evID int64, ackCh, beatCh <-chan int64, errCh <-chan error) error {
	wait := newAckWait(s.ackTimeout, s.processingLease)
	defer wait.stop()
	for {
		select {
		case id := <-ackCh:
			if id >= evID {
				return nil
			}
		case id := <-beatCh:
			if id != evID {
				continue
			}
			if !wait.extend() {
				_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, evID, "relay acknowledgement timeout")
				return errors.New("relay processing lease expired")
			}
		case err := <-errCh:
			_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, evID, "relay socket closed before acknowledgement")
			return err
		case <-ctx.Done():
			_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, evID, "relay connection closed before acknowledgement")
			return ctx.Err()
		case <-wait.timer.C:
			_ = s.Store.RecordHermesDeliveryFailed(ctx, h.ID, evID, "relay acknowledgement timeout")
			return errors.New("relay acknowledgement timeout")
		}
	}
}

// remoteBody returns a bounded preview of an arrival's body. Fetching is
// best-effort and bounded: a failure or timeout leaves the metadata-only body,
// so a slow mailbox never holds up the relay.
func (s *Server) remoteBody(ctx context.Context, accountID string, arrival store.RemoteArrival) string {
	if s.bodyFor == nil {
		return ""
	}
	fetchCtx, cancel := context.WithTimeout(ctx, remoteBodyTimeout)
	defer cancel()
	body, ok := s.bodyFor(fetchCtx, accountID, arrival)
	if !ok {
		return ""
	}
	return body
}

// remoteMessageEvent projects a detected remote arrival onto the gateway event
// shape. It mirrors messageEvent but carries the remote locator as well, so the
// gateway can fetch the full message live through the canonical API. A short
// preview (when one could be fetched) gives the agent something to read; the
// body is never archived.
func remoteMessageEvent(a store.RemoteArrival, preview string) map[string]any {
	name := a.FromName
	if name == "" {
		name = a.FromAddress
	}
	display := fmt.Sprintf("From: %s\nSubject: %s\n\n%s", a.FromAddress, a.Subject, remoteBodyPlaceholder)
	if preview != "" {
		display = fmt.Sprintf("From: %s\nSubject: %s\n\n%s", a.FromAddress, a.Subject, preview)
	}
	return map[string]any{
		"text": display, "message_type": "text", "user_id": a.FromAddress, "user_name": name, "message_id": a.ID,
		"source": map[string]any{"platform": "email", "chat_id": a.RFCMessageID, "chat_type": "thread", "chat_name": a.Subject, "user_id": a.FromAddress, "user_name": name, "thread_id": a.RFCMessageID, "chat_topic": nil, "message_id": a.ID},
		// reply_to_message_id carries the exact trigger (the durable arrival id),
		// so a reply targets this email and its remote threading metadata rather
		// than treating the RFC Message-ID as a local thread id. It resolves
		// through the same remote-message path as a REST reply.
		"metadata":              map[string]any{"email_message_id": a.ID, "reply_to_message_id": a.ID, "inbox_id": a.InboxID, "thread_id": a.RFCMessageID, "from": a.FromAddress, "subject": a.Subject, "remote": true, "folder_path": a.FolderPath, "rfc_message_id": a.RFCMessageID, "is_spam": false},
		"provenance":            map[string]any{"source": "email", "trust": "external_untrusted", "authenticated_sender": false},
		"allow_gateway_control": false,
	}
}

func messageEvent(m model.Message) map[string]any {
	name := m.From.Name
	if name == "" {
		name = m.From.Address
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = messagepreview.HTML(m.HTML, 60001)
	}
	if text == "" {
		text = "(No readable text body; full message remains available via the message-detail API)"
	}
	if len(text) > 60000 {
		end := 60000
		for !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end] + "\n\n[message truncated in realtime delivery; full message remains available via API]"
	}
	display := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", m.From.Address, strings.Join(m.To, ", "), m.Subject, text)
	ts := m.CreatedAt
	if m.ReceivedAt != nil {
		ts = *m.ReceivedAt
	}
	return map[string]any{
		"text": display, "message_type": "text", "user_id": m.From.Address, "user_name": name, "message_id": m.ID,
		"source":     map[string]any{"platform": "email", "chat_id": m.ThreadID, "chat_type": "thread", "chat_name": m.Subject, "user_id": m.From.Address, "user_name": name, "thread_id": m.ThreadID, "chat_topic": nil, "message_id": m.ID},
		"metadata":   map[string]any{"email_message_id": m.ID, "inbox_id": m.InboxID, "thread_id": m.ThreadID, "from": m.From.Address, "subject": m.Subject, "is_spam": m.Spam, "spam_reason": m.SpamReason},
		"provenance": map[string]any{"source": "email", "trust": "external_untrusted", "authenticated_sender": false},
		"timestamp":  ts.UTC().Format(time.RFC3339Nano), "allow_gateway_control": false,
	}
}

func outboundResult(requestID string, success bool, errText, messageID string) map[string]any {
	result := map[string]any{"success": success}
	if errText != "" {
		result["error"] = errText
	}
	if messageID != "" {
		result["message_id"] = messageID
	}
	return map[string]any{"type": "outbound_result", "requestId": requestID, "result": result}
}

// resolveRelayReplyTarget resolves the exact mail a relay reply answers. The
// local-thread lookup stays first, so local-mail replies keep their existing
// behaviour. Only when no local thread matches is a reply id resolved: the
// explicit reply_to_message_id when the gateway sends one, otherwise the
// conversation id itself. A Hermes gateway always sends an RFC Message-ID as
// chat_id, which for a detected remote arrival never names a local thread; the
// remote resolver still matches it to the cached remote message of that
// Message-ID, so connected-mailbox replies work without a gateway change. Both
// ids resolve first as a local message (an outbound target), then through the
// shared remote-message resolver, which understands metadata ids and arrival
// ids. The relay connection's own inbox is always enforced.
func (s *Server) resolveRelayReplyTarget(ctx context.Context, h store.HermesConnection, thread, replyTo string) (model.Message, error) {
	target, err := s.Store.LatestInboundMessageInThread(ctx, h.AccountID, h.InboxID, thread)
	if err == nil {
		return target, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Message{}, err
	}
	tried := map[string]bool{}
	for _, id := range []string{replyTo, thread} {
		id = strings.TrimSpace(id)
		if id == "" || tried[id] {
			continue
		}
		tried[id] = true
		if target, lerr := s.Store.GetMessageByID(ctx, h.AccountID, id); lerr == nil {
			if target.InboxID != h.InboxID || target.Internal {
				return model.Message{}, store.ErrNotFound
			}
			return target, nil
		} else if !errors.Is(lerr, store.ErrNotFound) {
			return model.Message{}, lerr
		}
		if s.Service.RemoteMessages == nil {
			continue
		}
		target, ok, rerr := s.Service.RemoteMessages.ResolveRemoteReply(ctx, h.AccountID, id)
		if rerr != nil {
			return model.Message{}, rerr
		}
		if !ok || target.InboxID != h.InboxID || target.Internal {
			continue
		}
		return target, nil
	}
	return model.Message{}, store.ErrNotFound
}

func (s *Server) handleOutbound(ctx context.Context, wr *socketWriter, h store.HermesConnection, requestID string, a outboundAction) {
	// The connection may have been deleted since this socket was accepted;
	// revalidate before performing any outbound operation.
	current, err := s.Store.GetHermesConnectionByGateway(ctx, h.GatewayID)
	if err != nil {
		_ = wr.JSON(outboundResult(requestID, false, "relay connection is no longer active", ""))
		return
	}
	h = current
	switch a.Op {
	case "typing":
		_ = wr.JSON(outboundResult(requestID, true, "", ""))
		return
	case "send":
		content := a.Content
		if content == "" {
			content = a.Text
		}
		thread := a.ChatID
		if thread == "" {
			thread = a.ThreadID
		}
		if strings.TrimSpace(thread) == "" || strings.TrimSpace(content) == "" {
			_ = wr.JSON(outboundResult(requestID, false, "chat_id and content are required", ""))
			return
		}
		target, err := s.resolveRelayReplyTarget(ctx, h, thread, a.ReplyToMessageID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.Log.Warn("relay reply target", "gateway_id", h.GatewayID, "request_id", requestID, "error", err)
			}
			_ = wr.JSON(outboundResult(requestID, false, "email thread not found", ""))
			return
		}
		inbox, err := s.Store.GetInboxInternal(ctx, h.AccountID, h.InboxID)
		if err != nil {
			_ = wr.JSON(outboundResult(requestID, false, "email inbox not found", ""))
			return
		}
		if inbox.Kind == model.InboxKindStandalone {
			thread = target.ID
		}
		if len(model.DefaultReplyRecipients(target)) == 0 {
			// The local path leaves an unclassifiable target to the send layer. A
			// relay reply has no local thread to consult, so treat an unclassified
			// remote message as incoming and answer its sender; an outbound (Sent)
			// message keeps its stored recipients and with none still needs an
			// explicit choice, which the relay has no surface for.
			if target.Direction == model.DirectionUnknown && strings.TrimSpace(target.From.Address) != "" {
				target.Direction = model.DirectionInbound
			}
			if len(model.DefaultReplyRecipients(target)) == 0 {
				_ = wr.JSON(outboundResult(requestID, false, "email thread not found", ""))
				return
			}
		}
		// A connection may be held to the Assistant boundary: instead of
		// sending with Owner authority, it creates a draft and requests
		// approval, so a human authorizes the send exactly as with an Assistant
		// API key. Existing connections default to owner.
		if strings.EqualFold(h.OutboundRole, "assistant") {
			p := model.Principal{AccountID: h.AccountID, MailboxRoles: map[string]string{h.InboxID: "assistant"}}
			draft, derr := s.Store.CreateDraft(ctx, p, model.Draft{InboxID: h.InboxID, FromAddress: inbox.DefaultSender, ReplyToMessageID: target.ID, To: []string{target.From.Address}, Subject: app.ReplySubject(target.Subject), Text: content})
			if derr != nil {
				_ = wr.JSON(outboundResult(requestID, false, derr.Error(), ""))
				return
			}
			if _, derr = s.Service.RequestSend(ctx, p, draft.ID, false); derr != nil {
				s.Log.Warn("relay assistant draft request failed", "gateway_id", h.GatewayID, "request_id", requestID, "error", derr)
				_ = wr.JSON(outboundResult(requestID, false, derr.Error(), ""))
				return
			}
			s.Log.Info("relay outbound requested approval", "gateway_id", h.GatewayID, "request_id", requestID, "draft_id", draft.ID)
			_ = wr.JSON(outboundResult(requestID, true, "", draft.ID))
			return
		}
		p := model.Principal{AccountID: h.AccountID, MailboxRoles: map[string]string{h.InboxID: "owner"}}
		res, err := s.Service.Send(ctx, p, app.SendInput{InboxID: h.InboxID, FromAddress: inbox.DefaultSender, ReplyToMessageID: target.ID, Text: content}, requestID)
		if err != nil {
			s.Log.Warn("relay outbound failed", "gateway_id", h.GatewayID, "request_id", requestID, "error", err)
			_ = wr.JSON(outboundResult(requestID, false, err.Error(), ""))
			return
		}
		s.Log.Info("relay outbound sent", "gateway_id", h.GatewayID, "request_id", requestID, "message_id", res.Message.ID)
		_ = wr.JSON(outboundResult(requestID, true, "", res.Message.ID))
	default:
		_ = wr.JSON(outboundResult(requestID, false, "unsupported email operation", ""))
	}
}
