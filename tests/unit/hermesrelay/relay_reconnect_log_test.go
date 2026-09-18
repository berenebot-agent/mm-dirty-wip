package hermesrelay_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/hermesrelay"
	"github.com/dellarb/mailmoose/internal/store"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *lockedBuffer) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Reset()
}

func waitForLastConnected(t *testing.T, st *store.Store, gatewayID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h, err := st.GetHermesConnectionByGateway(ctx, gatewayID)
		if err == nil && h.LastConnectedAt != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for last_connected_at")
}

// A rapid reconnect within the quiet window must log at Debug
// ("relay reconnected"), not Info ("relay connected").
func TestRelayRapidReconnectLogsAtDebug(t *testing.T) {
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
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-logtest", se, de)
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	rs := hermesrelay.New(svc)
	rs.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"

	first := dialRawWS(t, wsURL, makeUpgradeTokenTest(conn.GatewayID, secret))
	waitForLastConnected(t, st, conn.GatewayID)
	time.Sleep(50 * time.Millisecond)
	first.close()
	if got := logs.String(); !strings.Contains(got, "relay connected") {
		t.Fatalf("first connect should log at Info, got %q", got)
	}

	logs.Reset()
	second := dialRawWS(t, wsURL, makeUpgradeTokenTest(conn.GatewayID, secret))
	defer second.close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "relay reconnected") || strings.Contains(logs.String(), "relay connected") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := logs.String()
	if !strings.Contains(got, "relay reconnected") {
		t.Fatalf("rapid reconnect should log relay reconnected, got %q", got)
	}
	if strings.Contains(got, "msg=\"relay connected\"") || strings.Contains(got, "msg=relay connected") {
		t.Fatalf("rapid reconnect must not log relay connected at Info, got %q", got)
	}
}
