package dbschema_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/config"
	"github.com/dellarb/mammothmail/internal/events"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestPublicDatabaseMailWorkflows(t *testing.T) {
	// Serial: app.New changes the process-global outbound network policy.
	ctx := context.Background()
	dir, db := fresh(t)
	basic(t, db)
	db.Close()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc, err := app.New(config.Config{
		DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted",
		AllowPrivateOutbound: true, AppEncryptionKey: strings.Repeat("ab", 32),
		MaxMessageBytes: 1 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour,
		SendLimitPerMinute: 60,
	}, s, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	principal := model.Principal{AccountID: "a", Admin: true, MailboxRoles: map[string]string{"i": "owner"}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, "a", "d", "mailgun", map[string]any{"signing_key": "fixture-signing-key"}, false); err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		timestamp, token := fmt.Sprint(time.Now().Unix()), "workflow-delivery"
		mac := hmac.New(sha256.New, []byte("fixture-signing-key"))
		mac.Write([]byte(timestamp + token))
		for name, value := range map[string]string{
			"timestamp": timestamp, "token": token, "signature": hex.EncodeToString(mac.Sum(nil)),
			"sender": "sender@outside.test", "recipient": "robot@example.test",
			"body-mime": "From: sender@outside.test\r\nTo: robot@example.test\r\nSubject: Public workflow\r\nMessage-ID: <workflow@outside.test>\r\n\r\nPlease reply",
		} {
			if err := writer.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
		writer.Close()
		r := httptest.NewRequest("POST", "/internal/ingest/mailgun/raw-mime", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		return r
	}
	inbound, duplicate, err := svc.IngestInbound(ctx, "mailgun", request())
	if err != nil || duplicate {
		t.Fatal("inbound delivery", duplicate, err)
	}
	replay, duplicate, err := svc.IngestInbound(ctx, "mailgun", request())
	if err != nil || !duplicate || replay.ID != inbound.ID {
		t.Fatal("duplicate delivery", duplicate, err)
	}
	if _, err := s.ReplaceMessageLabels(ctx, principal, inbound.ID, []string{"Review"}); err != nil {
		t.Fatal("label operation", err)
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<workflow-sent@test>"}`)
	}))
	defer provider.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, "a", "d", "mailgun", map[string]any{"api_key": "fixture-key", "domain": "example.test", "api_base": provider.URL}); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Send(ctx, principal, app.SendInput{InboxID: "i", ReplyToMessageID: inbound.ID, Text: "A reply"}, "workflow-idempotency")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Deliver(ctx, "a", sent.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Send(ctx, principal, app.SendInput{InboxID: "i", ReplyToMessageID: inbound.ID, Text: "A reply"}, "workflow-idempotency")
	if err != nil || again.Message.ID != sent.Message.ID || calls.Load() != 1 || sent.Message.ThreadID != inbound.ThreadID {
		t.Fatal("send/reply/idempotency", calls.Load(), err)
	}
	assistant := model.Principal{AccountID: "a", MailboxRoles: map[string]string{"i": "assistant"}}
	draft, err := s.CreateDraft(ctx, assistant, model.Draft{InboxID: "i", To: []string{"recipient@outside.test"}, Subject: "Approved", Text: "frozen draft"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, assistant, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	approved, err := svc.ApproveDraft(ctx, principal, draft.ID, "", model.DecisionMethodUI, "")
	if err != nil || approved.Message.Text != "frozen draft" {
		t.Fatal("approval", err)
	}
	if err := svc.Deliver(ctx, "a", approved.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	uncertain, err := svc.Send(ctx, principal, app.SendInput{InboxID: "i", To: []string{"recipient@outside.test"}, Subject: "Uncertain", Text: "uncertain"}, "workflow-uncertain")
	if err != nil {
		t.Fatal(err)
	}
	id, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "sender", time.Hour)
	if err != nil || id != uncertain.Message.ID || claim == "" {
		t.Fatal("send claim", id, err)
	}
	if err := s.BeginMessageSend(ctx, "a", id, claim); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.ReconcileUncertainSends(ctx, time.Now().UTC(), true); err != nil || n != 1 {
		t.Fatal("uncertain-delivery recovery", n, err)
	}
	if err := s.RequeueFailed(ctx, principal, id); !errors.Is(err, store.ErrUncertainDelivery) {
		t.Fatal("database allowed an unsafe retry", err)
	}
	box, err := s.CreateStandaloneInbox(ctx, "a", store.StandaloneCreate{Address: "connected@outside.test", DisplayName: "Connected"})
	if err != nil {
		t.Fatal("standalone mailbox creation", err)
	}
	sealed, err := svc.EncryptSecretAAD(app.RemoteCredentialAAD("a", box.ID), []byte(`{"password":"fixture-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRemoteCredentials(ctx, "a", box.ID, sealed, "", store.ConfigVersion{}); err != nil {
		t.Fatal("connector credentials", err)
	}
	if gateway, secret, _, err := svc.CreateHermesRelay(ctx, principal, "i", "Public relay"); err != nil || gateway == "" || secret == "" {
		t.Fatal("relay creation", err)
	}
	if events, err := s.ListEvents(ctx, principal, 0, "i", 100); err != nil || len(events) == 0 {
		t.Fatal("event replay", err)
	}
	if calls.Load() != 2 {
		t.Fatal("an uncertain send reached the provider", calls.Load())
	}
}
