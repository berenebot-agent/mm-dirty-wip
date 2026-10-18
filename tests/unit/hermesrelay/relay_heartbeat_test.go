package hermesrelay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

type relayHeartbeatFixture struct {
	st     *store.Store
	conn   store.HermesConnection
	client *rawWS
	event  model.Event
}

func newRelayHeartbeatFixture(t *testing.T, gateway string, ackTimeout, lease time.Duration) *relayHeartbeatFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	d, _ := st.CreateDomain(ctx, u.AccountID, "example.com")
	box, _ := st.CreateInbox(ctx, u.AccountID, d.ID, "hermes", "Hermes")
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD(gateway)))
	de, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte("delivery-secret"), []byte(app.RelaySecretAAD(gateway)))
	conn, err := st.CreateHermesConnection(ctx, rec, gateway, se, de)
	if err != nil {
		t.Fatal(err)
	}
	_, ev, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "hb-1", RFCMessageID: "<hb@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Heartbeat", Text: "body", RawPath: "messages/hb.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	rs.SetAckTimeoutsForTest(ackTimeout, lease)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	t.Cleanup(ts.Close)
	client := dialRawWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	t.Cleanup(client.close)
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
	if inbound["bufferId"] != ev.Cursor {
		t.Fatalf("inbound buffer = %v, want %s", inbound["bufferId"], ev.Cursor)
	}
	return &relayHeartbeatFixture{st: st, conn: conn, client: client, event: ev}
}

func (f *relayHeartbeatFixture) sendHeartbeat(t *testing.T) {
	t.Helper()
	if err := f.client.writeJSON(map[string]any{"type": "inbound_processing", "bufferId": f.event.Cursor}); err != nil {
		t.Fatal(err)
	}
}

func (f *relayHeartbeatFixture) lastAck(t *testing.T) int64 {
	t.Helper()
	conn, err := f.st.GetHermesConnectionByGateway(context.Background(), f.conn.GatewayID)
	if err != nil {
		t.Fatal(err)
	}
	return conn.LastAckEventID
}

// TestRelayHeartbeatExtendsAckDeadline proves a handler that sends
// inbound_processing is not disconnected at the short ack timeout, and that a
// heartbeat never advances the durable cursor by itself.
func TestRelayHeartbeatExtendsAckDeadline(t *testing.T) {
	f := newRelayHeartbeatFixture(t, "gateway-heartbeat", 300*time.Millisecond, 2*time.Second)
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		f.sendHeartbeat(t)
	}
	if got := f.lastAck(t); got != 0 {
		t.Fatalf("heartbeat advanced the cursor to %d", got)
	}
	if err := f.client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": f.event.Cursor}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.lastAck(t) == f.event.ID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ack did not advance the cursor after heartbeats: %d", f.lastAck(t))
}

// TestRelayProcessingLeaseCapsHeartbeats proves heartbeats cannot extend an
// event past the fixed processing lease.
func TestRelayProcessingLeaseCapsHeartbeats(t *testing.T) {
	f := newRelayHeartbeatFixture(t, "gateway-heartbeat-lease", 80*time.Millisecond, 300*time.Millisecond)
	started := time.Now()
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = f.client.writeJSON(map[string]any{"type": "inbound_processing", "bufferId": f.event.Cursor})
			}
		}
	}()
	defer close(stop)
	var entries []store.ClientDeliveryEntry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		entries, err = f.st.ClientDeliveryLog(context.Background(), f.conn.AccountID, f.conn.ID, 10, f.event.ID+1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(entries) != 1 || entries[0].Status != "failed" {
		t.Fatalf("lease expiry not recorded as a failed attempt: %#v", entries)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("event failed after %s, before the processing lease; heartbeats did not extend it", elapsed)
	}
	if got := f.lastAck(t); got != 0 {
		t.Fatalf("lease-expired event was acknowledged: %d", got)
	}
}
