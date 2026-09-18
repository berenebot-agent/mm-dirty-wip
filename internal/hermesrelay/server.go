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

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/ws"
)

type Server struct {
	Service             *app.Service
	Store               *store.Store
	Hub                 *events.Hub
	Log                 *slog.Logger
	RequireCallerBearer bool
	upgrader            ws.Upgrader
}

// relayQuietReconnectWindow suppresses Info-level spam from the idle-timeout
// reconnect loop: a gateway that connected recently logs at Debug instead.
const relayQuietReconnectWindow = 10 * time.Minute

func New(svc *app.Service) *Server {
	return &Server{
		Service:             svc,
		Store:               svc.Store,
		Hub:                 svc.Hub,
		Log:                 svc.Log,
		RequireCallerBearer: svc.Config.RelayRequireBearer,
		upgrader:            ws.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
}

type EnrollRequest struct {
	EnrollmentToken string `json:"enrollmentToken"`
	GatewayID       string `json:"gatewayId"`
}

type EnrollResponse struct {
	Secret      string `json:"secret"`
	DeliveryKey string `json:"deliveryKey"`
	Tenant      string `json:"tenant"`
	GatewayID   string `json:"gatewayId"`
}

// Enroll implements the connector side of `hermes gateway enroll`.
// The one-time token is the authority for enrollment. Hosted deployments can
// additionally require the caller Authorization header to be present.
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
	delivery, err := auth.RandomToken(32)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	secEnc, err := s.Service.EncryptSecret([]byte(secret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	delEnc, err := s.Service.EncryptSecret([]byte(delivery))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	// The token is consumed and the connection created/replaced in one
	// transaction; a gateway owned by another account is rejected without
	// burning the token.
	conn, err := s.Store.EnrollHermesConnection(r.Context(), req.EnrollmentToken, req.GatewayID, secEnc, delEnc)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired enrollment token"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	s.Log.Info("relay enrolled", "gateway_id", conn.GatewayID, "account_id", conn.AccountID, "inbox_id", conn.InboxID)
	writeJSON(w, http.StatusOK, EnrollResponse{Secret: secret, DeliveryKey: delivery, Tenant: conn.AccountID, GatewayID: req.GatewayID})
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
	secret, err := s.Service.DecryptSecret(h.SecretEncrypted)
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
	Content  string `json:"content,omitempty"`
	Text     string `json:"text,omitempty"`
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
	// Deleting the connection cancels this socket immediately.
	scopeCtx, unregister := s.Hub.RegisterScope("hrm:" + h.ID)
	defer unregister()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(scopeCtx, cancel)
	defer stop()
	wr := &socketWriter{c: c}
	ackCh := make(chan int64, 8)
	helloCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
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
				if id := store.ParseCursor(f.BufferID); id > 0 {
					_ = s.Store.AckHermesEvent(ctx, h.ID, id)
					select {
					case ackCh <- id:
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
				go s.handleOutbound(ctx, wr, h, f.RequestID, a)
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
				// The message was deleted between the event and delivery, so
				// there is nothing to replay. Advance the durable cursor past
				// it; otherwise every reconnect re-reads the same stale event
				// and drops the socket in a tight loop.
				_ = s.Store.AckHermesEvent(ctx, h.ID, ev.ID)
				after = ev.ID
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
			if err := wr.JSON(map[string]any{"type": "inbound", "event": messageEvent(m), "bufferId": ev.Cursor}); err != nil {
				return err
			}
			s.Log.Info("relay inbound", "gateway_id", h.GatewayID, "cursor", ev.Cursor, "message_id", m.ID, "from", m.From.Address, "to", m.To)
			for {
				select {
				case id := <-ackCh:
					if id >= ev.ID {
						after = id
						s.Log.Info("relay acked", "gateway_id", h.GatewayID, "cursor", ev.Cursor)
						goto delivered
					}
				case err := <-errCh:
					return err
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(60 * time.Second):
					return errors.New("relay acknowledgement timeout")
				}
			}
		delivered:
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

func messageEvent(m model.Message) map[string]any {
	name := m.From.Name
	if name == "" {
		name = m.From.Address
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		text = "(HTML email; open the message to view content)"
	}
	if len(text) > 60000 {
		text = text[:60000] + "\n\n[message truncated in realtime delivery; full message remains available via API]"
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

func (s *Server) handleOutbound(ctx context.Context, wr *socketWriter, h store.HermesConnection, requestID string, a outboundAction) {
	// The connection may have been deleted since this socket was accepted;
	// revalidate before performing any outbound operation.
	if _, err := s.Store.GetHermesConnectionByGateway(ctx, h.GatewayID); err != nil {
		_ = wr.JSON(outboundResult(requestID, false, "relay connection is no longer active", ""))
		return
	}
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
		target, err := s.Store.LatestInboundMessageInThread(ctx, h.AccountID, h.InboxID, thread)
		if err != nil {
			_ = wr.JSON(outboundResult(requestID, false, "email thread not found", ""))
			return
		}
		inbox, err := s.Store.GetInboxInternal(ctx, h.AccountID, h.InboxID)
		if err != nil {
			_ = wr.JSON(outboundResult(requestID, false, "email inbox not found", ""))
			return
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
