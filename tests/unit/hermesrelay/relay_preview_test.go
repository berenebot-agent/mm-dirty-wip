package hermesrelay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/cryptox"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/hermesrelay"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// remoteStubBody is a deterministic stand-in for the remote preview fetcher (the
// unit suite cannot dial a live mailbox).
func remoteStubBody(_ context.Context, _ string, a store.RemoteArrival) (string, bool) {
	if strings.TrimSpace(a.RFCMessageID) == "" {
		return "", false
	}
	return "Preview body for " + a.Subject, true
}

// TestRelayRemoteReplyStoresBodyQuestion proves a detected remote-arrival
// notification reaches a relay gateway with a plain-text body, so the gateway
// records a non-empty message instead of an empty placeholder.
func TestRelayRemoteReplyStoresBodyQuestion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	inbox, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "agent@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	arrival, _, err := st.RecordRemoteArrival(ctx, u.AccountID, inbox.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 3, UID: 77, RFCMessageID: "<preview@test>",
		FromAddress: "alice@outside.test", FromName: "Alice", Subject: "Remote subject", SizeBytes: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordRemoteArrivalEvent(ctx, u.AccountID, inbox.ID, arrival); err != nil {
		t.Fatal(err)
	}
	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: inbox.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD("gateway-preview")))
	de, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte("delivery-secret"), []byte(app.RelaySecretAAD("gateway-preview")))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-preview", se, de)
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	rs.SetRemoteBody(remoteStubBody)
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
	event, _ := inbound["event"].(map[string]any)
	body, _ := event["text"].(string)
	if !strings.Contains(body, "alice@outside.test") || !strings.Contains(body, "Preview body for Remote subject") {
		t.Fatalf("remote event body lacks sender or preview: %q", body)
	}
	// The metadata-only placeholder must not be used when a preview was fetched.
	if strings.Contains(body, "fetch it via the API") {
		t.Fatalf("previewed event still carries the placeholder: %q", body)
	}
	// An arrival that predates the preview machinery has no snippet and no more
	// queued preview, so the relay must not fetch a live body on the hot path.
	rs2 := hermesrelay.New(svc)
	called := false
	rs2.SetRemoteBodyForTest(func(context.Context, string, store.RemoteArrival) (string, bool) {
		called = true
		return "", false
	})
	ts2 := httptest.NewServer(http.HandlerFunc(rs2.ServeWebSocket))
	defer ts2.Close()
	client2 := dialRawWS(t, "ws"+strings.TrimPrefix(ts2.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client2.close()
	if err = client2.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err = client2.readFrame(); err != nil {
		t.Fatal(err)
	}
	var fallback map[string]any
	if err = client2.readJSON(&fallback); err != nil {
		t.Fatal(err)
	}
	event, _ = fallback["event"].(map[string]any)
	if body, _ := event["text"].(string); !strings.Contains(body, "fetch it via the API") {
		t.Fatalf("failed preview did not fall back to the placeholder: %q", body)
	}
	if !called {
		t.Fatal("the remote preview source was never consulted")
	}
}
