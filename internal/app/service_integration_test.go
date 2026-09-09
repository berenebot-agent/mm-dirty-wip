package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func testService(t *testing.T) (*Service, model.User, model.Domain, model.Inbox) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AppEncryptionKey: "01234567890123456789012345678901", MailgunSigningKey: "signing-secret", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 10, SendLimitPerMinute: 60}
	svc, err := New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	return svc, u, d, b
}
func mgSig(key, ts, tok string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(ts + tok))
	return hex.EncodeToString(m.Sum(nil))
}
func mgRequest(t *testing.T, key, token, recipient, raw string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	fields := map[string]string{"timestamp": ts, "token": token, "signature": mgSig(key, ts, token), "sender": "sender@outside.test", "recipient": recipient, "Message-Id": "<provider@test>"}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	p, _ := mw.CreateFormField("body-mime")
	io.WriteString(p, raw)
	mw.Close()
	r := httptest.NewRequest("POST", "/internal/ingest/mailgun", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestMailgunIngestOutboundReplyAndIdempotency(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	raw := "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: Hello\r\nMessage-ID: <inbound@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nPlease reply"
	m, dup, err := svc.IngestMailgun(ctx, mgRequest(t, svc.Config.MailgunSigningKey, "delivery-1", box.Address, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	m2, dup, err := svc.IngestMailgun(ctx, mgRequest(t, svc.Config.MailgunSigningKey, "delivery-1", box.Address, raw))
	if err != nil || !dup || m2.ID != m.ID {
		t.Fatalf("dedup %v dup=%v", err, dup)
	}
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/v3/mg.example.com/messages") {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<mailgun-id>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "MG", "mailgun", map[string]any{"api_key": "key-test", "domain": "mg.example.com", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, ReplyToMessageID: m.ID, Text: "Done"}, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.ThreadID != m.ThreadID || res.Message.InReplyTo != "<inbound@test>" {
		t.Fatalf("reply thread/header: %+v", res.Message)
	}
	// Deliver so the idempotency reservation completes.
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Send(ctx, p, SendInput{InboxID: box.ID, ReplyToMessageID: m.ID, Text: "Done"}, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if again.Message.ID != res.Message.ID || calls.Load() != 1 {
		t.Fatalf("idempotency calls=%d ids=%s/%s", calls.Load(), again.Message.ID, res.Message.ID)
	}
}
