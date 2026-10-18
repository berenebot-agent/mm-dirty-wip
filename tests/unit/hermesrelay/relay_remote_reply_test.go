package hermesrelay_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/cryptox"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/hermesrelay"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// TestRelayRepliesToRemoteTriggerMessage proves a relay reply to a connected
// mailbox targets the exact arrival that triggered the agent, not whichever
// remote message is newest, and keeps the remote RFC threading headers.
func TestRelayRepliesToRemoteTriggerMessage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20}
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	inbox, err := st.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "agent@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, u.AccountID, inbox.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// Two arrivals in the same mailbox: the reply must target the first one even
	// though the second is newer.
	first, _, err := st.RecordRemoteArrival(ctx, u.AccountID, inbox.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 10,
		RFCMessageID: "<remote-one@test>", FromName: "Alice", FromAddress: "alice@outside.test",
		Subject: "First remote", SizeBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordRemoteArrival(ctx, u.AccountID, inbox.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 11,
		RFCMessageID: "<remote-two@test>", FromName: "Bob", FromAddress: "bob@outside.test",
		Subject: "Second remote", SizeBytes: 11,
	}); err != nil {
		t.Fatal(err)
	}
	// The shared remote-message resolver is what a reply goes through.
	app.NewRemoteMailboxService(svc).InstallRemoteBridges()

	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: inbox.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD("gateway-remote-reply")))
	de, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte("delivery-secret"), []byte(app.RelaySecretAAD("gateway-remote-reply")))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-remote-reply", se, de)
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
	if err = client.writeJSON(map[string]any{
		"type":      "outbound",
		"requestId": "remote-reply-1",
		"action": map[string]any{
			"op":                  "send",
			"chat_id":             first.RFCMessageID,
			"reply_to_message_id": first.ID,
			"content":             "reply to the first message",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = client.readJSON(&result); err != nil {
		t.Fatal(err)
	}
	res, _ := result["result"].(map[string]any)
	if res == nil || res["success"] != true {
		t.Fatalf("remote reply failed: %#v", result)
	}
	msgID, _ := res["message_id"].(string)
	sent, err := st.GetMessageByID(ctx, u.AccountID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.To) != 1 || sent.To[0] != "alice@outside.test" {
		t.Fatalf("reply recipients = %v, want the first remote sender", sent.To)
	}
	if sent.InReplyTo != "<remote-one@test>" {
		t.Fatalf("in_reply_to = %q, want the trigger message", sent.InReplyTo)
	}
	foundRef := false
	for _, ref := range sent.References {
		if ref == "<remote-one@test>" {
			foundRef = true
		}
		if ref == "<remote-two@test>" {
			t.Fatalf("reply references the newer message: %v", sent.References)
		}
	}
	if !foundRef {
		t.Fatalf("reply references = %v, want the trigger message", sent.References)
	}
	// Give the queued send a moment to be visible if a provider is configured;
	// this fixture has none, so it must remain queued rather than fail.
	if sent.Status != "pending" {
		t.Fatalf("queued remote reply status = %q, want pending", sent.Status)
	}

	// The assistant boundary reuses the same resolved target: the draft freezes
	// the remote metadata id, not the relay's RFC conversation id.
	if err := st.SetHermesOutboundRole(ctx, u.AccountID, conn.ID, "assistant"); err != nil {
		t.Fatal(err)
	}
	if err = client.writeJSON(map[string]any{
		"type":      "outbound",
		"requestId": "remote-reply-assistant",
		"action": map[string]any{
			"op":                  "send",
			"chat_id":             first.RFCMessageID,
			"reply_to_message_id": first.ID,
			"content":             "please review this reply",
		},
	}); err != nil {
		t.Fatal(err)
	}
	result = nil
	if err = client.readJSON(&result); err != nil {
		t.Fatal(err)
	}
	res, _ = result["result"].(map[string]any)
	if res == nil || res["success"] != true {
		t.Fatalf("assistant remote reply failed: %#v", result)
	}
	draftID, _ := res["message_id"].(string)
	draft, err := st.GetDraftInternal(ctx, u.AccountID, draftID)
	if err != nil {
		t.Fatal(err)
	}
	remoteRec, err := st.GetRemoteMessageByUID(ctx, u.AccountID, inbox.ID, "INBOX", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if draft.ReplyToMessageID != remoteRec.ID {
		t.Fatalf("assistant draft reply target = %q, want remote message %q", draft.ReplyToMessageID, remoteRec.ID)
	}
}
