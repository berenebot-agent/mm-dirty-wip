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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/auth"
	"gatehouse-mail/internal/cryptox"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
	"gatehouse-mail/internal/ws"
)

type Server struct {
	Service             *app.Service
	Store               *store.Store
	Hub                 *events.Hub
	RequireCallerBearer bool
	upgrader            ws.Upgrader
}

func New(svc *app.Service) *Server {
	return &Server{
		Service:             svc,
		Store:               svc.Store,
		Hub:                 svc.Hub,
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
	rec, err := s.Store.ConsumeHermesEnrollToken(r.Context(), req.EnrollmentToken)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired enrollment token"})
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
	secEnc, err := cryptox.Encrypt(s.Service.EncryptionKey, []byte(secret))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	delEnc, err := cryptox.Encrypt(s.Service.EncryptionKey, []byte(delivery))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	if _, err = s.Store.CreateHermesConnection(r.Context(), rec, req.GatewayID, secEnc, delEnc); err != nil {
		writeJSON(w, 500, map[string]string{"error": "enrollment failed"})
		return
	}
	writeJSON(w, http.StatusOK, EnrollResponse{Secret: secret, DeliveryKey: delivery, Tenant: rec.AccountID, GatewayID: req.GatewayID})
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
	secret, err := cryptox.Decrypt(s.Service.EncryptionKey, h.SecretEncrypted)
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
			"contract_version": 1, "platform": "email", "label": "Gatehouse Email", "emoji": "✉️",
			"platform_hint": "Email", "max_message_length": 100000, "supports_draft_streaming": false,
			"supports_edit": false, "supports_threads": true, "markdown_dialect": "plain", "len_unit": "chars",
			"supported_ops": []string{"send", "typing"},
		},
	}
}

func (s *Server) run(ctx context.Context, c *ws.Conn, h store.HermesConnection) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
			if err := wr.JSON(map[string]any{"type": "inbound", "event": messageEvent(m), "bufferId": ev.Cursor}); err != nil {
				return err
			}
			for {
				select {
				case id := <-ackCh:
					if id >= ev.ID {
						after = id
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
		"metadata":   map[string]any{"email_message_id": m.ID, "inbox_id": m.InboxID, "thread_id": m.ThreadID, "from": m.From.Address, "subject": m.Subject},
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
		p := model.Principal{AccountID: h.AccountID, MailboxRoles: map[string]string{h.InboxID: "owner"}}
		res, err := s.Service.Send(ctx, p, app.SendInput{InboxID: h.InboxID, ReplyToMessageID: target.ID, Text: content}, requestID)
		if err != nil {
			_ = wr.JSON(outboundResult(requestID, false, err.Error(), ""))
			return
		}
		_ = wr.JSON(outboundResult(requestID, true, "", res.Message.ID))
	default:
		_ = wr.JSON(outboundResult(requestID, false, "unsupported email operation", ""))
	}
}
