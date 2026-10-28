package hermesrelay_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/cryptox"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/hermesrelay"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

func TestRelayHTMLBody(t *testing.T) {
	// Serial: app.New changes the process-wide outbound network policy.
	ctx := context.Background()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	defer st.Close()
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.CreateInbox(ctx, u.AccountID, d.ID, "agent", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	const gateway = "gateway-html"
	const secret = "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, err := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD(gateway)))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateHermesConnection(ctx, store.EnrollRecord{AccountID: u.AccountID, InboxID: box.ID, Name: "Agent"}, gateway, se, se)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, text, html, want string }{
		{"html only", "", `<head><title>hidden</title></head><style>hidden</style><script>hidden</script><p>Please reply &amp; confirm</p><p>HTML BODY OK</p>`, "Please reply & confirm HTML BODY OK"},
		{"plain preferred", " plain body ", "<p>other body</p>", "plain body"},
		{"blank plain", " \n\t", "<p>HTML fallback</p>", "HTML fallback"},
		{"no readable body", "", "<style>hidden</style><p>&nbsp;</p>", "(No readable text body; full message remains available via the message-detail API)"},
		{"beyond preview", "", "<p>" + strings.Repeat("x", 300) + " END</p>", strings.Repeat("x", 300) + " END"},
		{"html truncated", "", "<p>" + strings.Repeat("x", 60002) + "</p>", strings.Repeat("x", 60000) + "\n\n[message truncated in realtime delivery; full message remains available via API]"},
		{"unicode truncated", strings.Repeat("界", 19999) + "🙂tail", "", strings.Repeat("界", 19999) + "\n\n[message truncated in realtime delivery; full message remains available via API]"},
	}
	for i, tc := range cases {
		_, _, _, err := st.CommitInbound(ctx, store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: fmt.Sprintf("html-%d", i), RFCMessageID: fmt.Sprintf("<html-%d@test>", i), From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address}, Subject: tc.name, Text: tc.text, HTML: tc.html, RawPath: "messages/html.eml", SizeBytes: 10, ReceivedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
	}
	rs := hermesrelay.New(svc)
	ts := httptest.NewServer(http.HandlerFunc(rs.ServeWebSocket))
	defer ts.Close()
	client := dialRawWS(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/relay", makeUpgradeTokenTest(conn.GatewayID, secret))
	defer client.close()
	if err := client.writeJSON(map[string]any{"type": "hello", "platform": "email", "botId": "default"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.readFrame(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		var inbound struct {
			Type     string `json:"type"`
			BufferID string `json:"bufferId"`
			Event    struct {
				Text string `json:"text"`
			} `json:"event"`
		}
		if err := client.readJSON(&inbound); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("From: alice@outside.test\nTo: %s\nSubject: %s\n\n%s", box.Address, tc.name, tc.want)
		if inbound.Type != "inbound" || inbound.Event.Text != want || !utf8.ValidString(inbound.Event.Text) {
			t.Fatalf("%s: relay body differs (got %d bytes, want %d)", tc.name, len(inbound.Event.Text), len(want))
		}
		if err := client.writeJSON(map[string]any{"type": "inbound_ack", "bufferId": inbound.BufferID}); err != nil {
			t.Fatal(err)
		}
	}
}
