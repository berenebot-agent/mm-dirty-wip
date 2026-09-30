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
)

// TestRelayDeliveryLogAcknowledged proves an acknowledged relay event records an
// "acknowledged" row in the client delivery log with the message snapshot, and
// that the acknowledged outcome is account-scoped.
func TestRelayDeliveryLogAcknowledged(t *testing.T) {
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
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-delivery-log", se, de)
	if err != nil {
		t.Fatal(err)
	}
	msg, ev, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: "log-d1", RFCMessageID: "<log@test>", From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: "Logged relay", Text: "hi", RawPath: "messages/log.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	hub.Publish(ev)
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
	if err = client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": ev.Cursor}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var entry store.ClientDeliveryEntry
	for time.Now().Before(deadline) {
		entries, lerr := st.ClientDeliveryLog(ctx, u.AccountID, conn.ID, 10, ev.ID+1)
		if lerr == nil {
			for _, e := range entries {
				if e.EventID == ev.ID {
					entry = e
				}
			}
		}
		if entry.Status == "acknowledged" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if entry.Status != "acknowledged" || entry.EventType != "message.received" {
		t.Fatalf("relay log entry %#v", entry)
	}
	if entry.Detail != "Logged relay" || entry.MessageID != msg.ID {
		t.Fatalf("relay log snapshot %#v", entry)
	}
	if entry.Attempts < 1 || entry.ClientKind != "hermes" {
		t.Fatalf("relay log attempts/kind %#v", entry)
	}
	ub, _ := st.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 1<<20)
	if _, err := st.ClientDeliveryLog(ctx, ub.AccountID, conn.ID, 10, 0); err == nil {
		t.Fatal("cross-account relay log was readable")
	}
}
