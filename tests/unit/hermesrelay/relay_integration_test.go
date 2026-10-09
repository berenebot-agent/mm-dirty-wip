package hermesrelay_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/hermesrelay"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

type rawWS struct {
	c net.Conn
	r *bufio.Reader
}

func dialRawWS(t *testing.T, rawURL, bearer string) *rawWS {
	t.Helper()
	u, _ := url.Parse(rawURL)
	c, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	keyb := make([]byte, 16)
	rand.Read(keyb)
	key := base64.StdEncoding.EncodeToString(keyb)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nAuthorization: Bearer %s\r\n\r\n", u.RequestURI(), u.Host, key, bearer)
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || !strings.Contains(line, "101") {
		b, _ := io.ReadAll(r)
		t.Fatalf("upgrade %q err=%v body=%s", line, err, b)
	}
	for {
		line, _ = r.ReadString('\n')
		if line == "\r\n" {
			break
		}
	}
	return &rawWS{c: c, r: r}
}
func (w *rawWS) close() { w.c.Close() }
func (w *rawWS) writeJSON(v any) error {
	b, _ := json.Marshal(v)
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr := []byte{0x81}
	n := len(b)
	if n < 126 {
		hdr = append(hdr, byte(n)|0x80)
	} else {
		hdr = append(hdr, 126|0x80, byte(n>>8), byte(n))
	}
	payload := make([]byte, n)
	for i := range b {
		payload[i] = b[i] ^ mask[i%4]
	}
	_, err := w.c.Write(append(append(hdr, mask...), payload...))
	return err
}
func (w *rawWS) readFrame() ([]byte, error) {
	_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	h := make([]byte, 2)
	if _, err := io.ReadFull(w.r, h); err != nil {
		return nil, err
	}
	n := uint64(h[1] & 0x7f)
	if n == 126 {
		b := make([]byte, 2)
		io.ReadFull(w.r, b)
		n = uint64(binary.BigEndian.Uint16(b))
	} else if n == 127 {
		b := make([]byte, 8)
		io.ReadFull(w.r, b)
		n = binary.BigEndian.Uint64(b)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(w.r, data); err != nil {
		return nil, err
	}
	return data, nil
}
func (w *rawWS) readJSON(v any) error {
	data, err := w.readFrame()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
func makeUpgradeTokenTest(gateway, secret string) string {
	exp := time.Now().Add(5 * time.Minute).Unix()
	signed := fmt.Sprintf("%s:%d", gateway, exp)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(signed))
	raw := fmt.Sprintf("%s:%s", signed, hex.EncodeToString(m.Sum(nil)))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func TestRelayHandshakeAndBufferedInbound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	delivery := "delivery-secret"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(delivery))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-test", se, de)
	if err != nil {
		t.Fatal(err)
	}
	msg, ev, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "relay-d1", RFCMessageID: "<r1@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Relay test", Text: "hello hermes", RawPath: "messages/r.eml", SizeBytes: 10, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish(ev)
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
	client := dialRawWS(t, wsURL, makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	rawDescriptor, err := client.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(rawDescriptor, []byte("\n")) {
		t.Fatalf("descriptor frame must be newline-terminated: %q", rawDescriptor)
	}
	var descriptor map[string]any
	if err = json.Unmarshal(rawDescriptor, &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor["type"] != "descriptor" {
		t.Fatalf("descriptor %#v", descriptor)
	}
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if inbound["type"] != "inbound" || inbound["bufferId"] != ev.Cursor {
		t.Fatalf("inbound %#v", inbound)
	}
	e := inbound["event"].(map[string]any)
	if e["message_id"] != msg.ID {
		t.Fatalf("event %#v", e)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev.Cursor}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	updated, err := st.GetHermesConnectionByGateway(ctx, conn.GatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastAckEventID < ev.ID {
		t.Fatalf("ack not persisted: %d", updated.LastAckEventID)
	}
}

// TestRelayFutureAckDoesNotSkipEvents proves a gateway that acknowledges a
// buffer id ahead of the event actually delivered cannot advance the durable
// cursor over the events in between: the cursor moves only to the delivered
// event, and the skipped event is still replayed.
func TestRelayFutureAckDoesNotSkipEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte("delivery-secret"))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-future-ack", se, de)
	if err != nil {
		t.Fatal(err)
	}
	_, ev1, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "future-1", RFCMessageID: "<f1@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Future one", Text: "one", RawPath: "messages/f1.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	_, ev2, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "future-2", RFCMessageID: "<f2@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Future two", Text: "two", RawPath: "messages/f2.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
	client := dialRawWS(t, wsURL, makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readFrame(); err != nil {
		t.Fatal(err)
	}
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if inbound["bufferId"] != ev1.Cursor {
		t.Fatalf("first event %#v", inbound)
	}
	// Ack a future buffer id (ev2) while ev1 is the event actually delivered.
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev2.Cursor}); err != nil {
		t.Fatal(err)
	}
	// The second event must still be delivered: the future ack must not have
	// skipped it.
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if inbound["bufferId"] != ev2.Cursor {
		t.Fatalf("second event after a future ack %#v", inbound)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev2.Cursor}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	updated, err := st.GetHermesConnectionByGateway(ctx, conn.GatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastAckEventID != ev2.ID {
		t.Fatalf("final cursor = %d, want %d", updated.LastAckEventID, ev2.ID)
	}
}

// A disconnected relay must replay buffered events in order on reconnect.
func TestRelayDisconnectReconnectReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte("delivery-secret"))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-replay", se, de)
	if err != nil {
		t.Fatal(err)
	}
	// Commit two events while disconnected.
	_, ev1, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "replay-1", RFCMessageID: "<r1@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Replay one", Text: "one", RawPath: "messages/r1.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	_, ev2, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "replay-2", RFCMessageID: "<r2@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Replay two", Text: "two", RawPath: "messages/r2.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
	client := dialRawWS(t, wsURL, makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readFrame(); err != nil {
		t.Fatal(err)
	}
	// First event.
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if inbound["bufferId"] != ev1.Cursor {
		t.Fatalf("first replay %#v", inbound)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev1.Cursor}); err != nil {
		t.Fatal(err)
	}
	// Second event.
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if inbound["bufferId"] != ev2.Cursor {
		t.Fatalf("second replay %#v", inbound)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev2.Cursor}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	updated, err := st.GetHermesConnectionByGateway(ctx, conn.GatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastAckEventID < ev2.ID {
		t.Fatalf("replay ack not persisted: %d", updated.LastAckEventID)
	}
}

// TestRelayOutboundUsesDefaultSender proves a relay send uses the inbox's
// configured default sender, including an external sending alias, rather than
// always the primary address.
func TestRelayOutboundUsesDefaultSender(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")

	// The inbox default sender is an external alias.
	alias, err := st.CreateExternalAlias(ctx, u.AccountID, box.ID, "agent@gmail.com", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetInboxDefaultSender(ctx, u.AccountID, box.ID, alias.Address); err != nil {
		t.Fatal(err)
	}

	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte("delivery-secret"))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-sender", se, de)
	if err != nil {
		t.Fatal(err)
	}
	thread, _, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "sender-1", RFCMessageID: "<s1@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Relay sender", Text: "hi", RawPath: "messages/s.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	client := dialRawWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readFrame(); err != nil {
		t.Fatal(err)
	}
	// Drain the buffered inbound event so the socket is ready for the reply.
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": inbound["bufferId"]}); err != nil {
		t.Fatal(err)
	}
	if err = client.writeJSON(map[string]any{"type": "outbound", "requestId": "req-1", "action": map[string]any{"op": "send", "chat_id": thread.ThreadID, "content": "reply"}}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = client.readJSON(&result); err != nil {
		t.Fatal(err)
	}
	if result["type"] != "outbound_result" {
		t.Fatalf("outbound result %#v", result)
	}
	res, _ := result["result"].(map[string]any)
	if res == nil || res["success"] != true {
		t.Fatalf("outbound failed: %#v", result)
	}
	msgID, _ := res["message_id"].(string)
	sent, err := st.GetMessageByID(ctx, u.AccountID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if sent.From.Address != "agent@gmail.com" || sent.From.Name != "Agent" {
		t.Fatalf("relay did not use the default external alias: %+v", sent.From)
	}
}

// TestRelayOutboundBurstIsBounded proves a burst of outbound frames does not
// spawn unbounded work: every request receives exactly one result (success or a
// bounded "too many in-flight" rejection) and the socket stays healthy.
func TestRelayOutboundBurstIsBounded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SendLimitPerMinute: 1 << 30}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte("delivery-secret"))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-burst", se, de)
	if err != nil {
		t.Fatal(err)
	}
	thread, _, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "burst-1", RFCMessageID: "<b1@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Burst", Text: "hi", RawPath: "messages/b.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	client := dialRawWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readFrame(); err != nil {
		t.Fatal(err)
	}
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatal(err)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": inbound["bufferId"]}); err != nil {
		t.Fatal(err)
	}
	const burst = 64
	for i := 0; i < burst; i++ {
		req := fmt.Sprintf("burst-%d", i)
		if err = client.writeJSON(map[string]any{"type": "outbound", "requestId": req, "action": map[string]any{"op": "send", "chat_id": thread.ThreadID, "content": "reply"}}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	for len(seen) < burst {
		var result map[string]any
		if err = client.readJSON(&result); err != nil {
			t.Fatalf("after %d results: %v", len(seen), err)
		}
		if result["type"] != "outbound_result" {
			continue
		}
		rid, _ := result["requestId"].(string)
		if rid == "" {
			t.Fatalf("result without requestId: %#v", result)
		}
		seen[rid]++
	}
	for rid, n := range seen {
		if n != 1 {
			t.Fatalf("request %s received %d results, want 1", rid, n)
		}
	}
}

// A message.received event whose message was later deleted must be skipped
// rather than tearing the socket down, which would reconnect-loop forever on
// the same stale event.
func TestRelaySkipsDeletedMessageEvent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.Encrypt(svc.EncryptionKey, []byte(secret))
	de, _ := cryptox.Encrypt(svc.EncryptionKey, []byte("delivery-secret"))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-test", se, de)
	if err != nil {
		t.Fatal(err)
	}
	gone, _, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "relay-gone", RFCMessageID: "<gone@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Deleted", Text: "gone", RawPath: "messages/g.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = st.DeleteMessage(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, gone.ID); err != nil {
		t.Fatal(err)
	}
	_, ev, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "relay-live", RFCMessageID: "<live@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Live", Text: "live", RawPath: "messages/l.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	client := dialRawWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err = client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.readFrame(); err != nil {
		t.Fatal(err)
	}
	var inbound map[string]any
	if err = client.readJSON(&inbound); err != nil {
		t.Fatalf("connection dropped instead of skipping deleted message: %v", err)
	}
	if inbound["type"] != "inbound" || inbound["bufferId"] != ev.Cursor {
		t.Fatalf("expected live event %v, got %#v", ev.Cursor, inbound)
	}
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev.Cursor}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	updated, err := st.GetHermesConnectionByGateway(ctx, conn.GatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastAckEventID < ev.ID {
		t.Fatalf("ack not persisted: %d", updated.LastAckEventID)
	}
}
