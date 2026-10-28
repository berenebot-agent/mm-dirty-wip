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
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
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
	remoteSvc := app.NewRemoteMailboxService(svc)
	remoteSvc.InstallRemoteBridges()

	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: inbox.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD("gateway-remote-reply")))
	de, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte("delivery-secret"), []byte(app.RelaySecretAAD("gateway-remote-reply")))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-remote-reply", se, de)
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	rs.SetRemoteBody(remoteSvc.ArrivalBodyPreview)
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

// TestRelayHermesStyleRemoteReply proves the server recovers a connected-mailbox
// reply from the only identifier an unmodified Hermes gateway sends: chat_id,
// which is the event's RFC Message-ID. The reply must use the resolved remote
// message, not a local thread lookup, and must fail closed when that message is
// not resolvable.
func TestRelayHermesStyleRemoteReply(t *testing.T) {
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
	if _, err := st.ReconcileRemoteFolders(ctx, u.AccountID, inbox.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordRemoteArrival(ctx, u.AccountID, inbox.ID, store.RemoteArrivalInput{
		FolderPath: "INBOX", UIDValidity: 1, UID: 20,
		RFCMessageID: "<hermes-one@test>", FromName: "Alice", FromAddress: "alice@outside.test",
		Subject: "Hermes remote", SizeBytes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, u.AccountID, inbox.ID, "INBOX", 1, []store.RemoteMessageInput{
		{UID: 20, RFCMessageID: "<hermes-one@test>", FromName: "Alice", FromAddress: "alice@outside.test", Subject: "Hermes remote"},
	}, false); err != nil {
		t.Fatal(err)
	}
	// A detected arrival also has a remote preview queued; the relay uses that
	// cached snippet without a live fetch.
	if err := st.CompleteRemotePreview(ctx, u.AccountID, inbox.ID, mustRemoteMessageID(t, st, u.AccountID, inbox.ID, 20), "Please review the attached invoice today."); err != nil {
		t.Fatal(err)
	}
	remoteSvc := app.NewRemoteMailboxService(svc)
	remoteSvc.InstallRemoteBridges()

	rec := store.EnrollRecord{AccountID: u.AccountID, InboxID: inbox.ID, Name: "Hermes"}
	secret := "relay-secret-abcdefghijklmnopqrstuvwxyz"
	se, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte(secret), []byte(app.RelaySecretAAD("gateway-hermes-style")))
	de, _ := cryptox.EncryptWithAAD(svc.EncryptionKey, []byte("delivery-secret"), []byte(app.RelaySecretAAD("gateway-hermes-style")))
	conn, err := st.CreateHermesConnection(ctx, rec, "gateway-hermes-style", se, de)
	if err != nil {
		t.Fatal(err)
	}
	rs := hermesrelay.New(svc)
	rs.SetRemoteBody(remoteSvc.ArrivalBodyPreview)
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

	// Exactly what an unmodified Hermes gateway sends: no reply_to_message_id.
	if err = client.writeJSON(map[string]any{
		"type":      "outbound",
		"requestId": "hermes-reply-1",
		"action": map[string]any{
			"op":      "send",
			"chat_id": "<hermes-one@test>",
			"content": "reply with only chat_id",
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
		t.Fatalf("Hermes-style remote reply failed: %#v", result)
	}
	msgID, _ := res["message_id"].(string)
	sent, err := st.GetMessageByID(ctx, u.AccountID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.To) != 1 || sent.To[0] != "alice@outside.test" {
		t.Fatalf("reply recipients = %v, want the remote sender", sent.To)
	}
	if sent.InReplyTo != "<hermes-one@test>" {
		t.Fatalf("in_reply_to = %q, want the trigger's RFC Message-ID", sent.InReplyTo)
	}

	// An unresolvable id (a notification for mail not yet indexed) must fail
	// closed rather than send to a guessed recipient.
	if err = client.writeJSON(map[string]any{
		"type":      "outbound",
		"requestId": "hermes-reply-unknown",
		"action": map[string]any{
			"op":      "send",
			"chat_id": "<not-indexed@test>",
			"content": "reply to unknown",
		},
	}); err != nil {
		t.Fatal(err)
	}
	result = nil
	if err = client.readJSON(&result); err != nil {
		t.Fatal(err)
	}
	res, _ = result["result"].(map[string]any)
	if res == nil || res["success"] != false {
		t.Fatalf("unresolvable reply was not refused: %#v", result)
	}
}

// mustRemoteMessageID resolves a cached remote metadata id by its locator.
func mustRemoteMessageID(t *testing.T, st *store.Store, accountID, inboxID string, uid uint32) string {
	t.Helper()
	rec, err := st.GetRemoteMessageByUID(context.Background(), accountID, inboxID, "INBOX", 1, uid)
	if err != nil {
		t.Fatal(err)
	}
	return rec.ID
}
