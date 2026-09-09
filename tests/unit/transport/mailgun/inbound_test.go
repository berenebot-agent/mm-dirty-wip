package mailgun

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/transport"
)

type fakeResolver struct {
	key string
	err error
}

func (f fakeResolver) ResolveInboundBinding(_ context.Context, provider, recipient string) (transport.InboundBinding, error) {
	if f.err != nil {
		return transport.InboundBinding{}, f.err
	}
	return transport.InboundBinding{
		AccountID: "acc", DomainID: "dom", CredentialID: "cred",
		Provider: provider, Recipient: recipient,
		Config: map[string]any{"signing_key": f.key},
	}, nil
}

func signature(key, ts, tok string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(ts + tok))
	return hex.EncodeToString(m.Sum(nil))
}
func TestSignatureFreshness(t *testing.T) {
	key := "secret"
	tok := "abc"
	tsi := fmtInt(time.Now().Unix())
	if !VerifySignature(key, tsi, tok, signature(key, tsi, tok)) {
		t.Fatal("valid signature rejected")
	}
	old := fmtInt(time.Now().Add(-48 * time.Hour).Unix())
	if VerifySignature(key, old, tok, signature(key, old, tok)) {
		t.Fatal("stale signature accepted")
	}
}
func fmtInt(v int64) string { return fmt.Sprintf("%d", v) }

func multipartRequest(t *testing.T, key, token, recipient, raw string, extra map[string]string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	ts := fmtInt(time.Now().Unix())
	fields := map[string]string{"timestamp": ts, "token": token, "signature": signature(key, ts, token), "sender": "b@test", "recipient": recipient, "Message-Id": "<x@test>"}
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for k, v := range extra {
		_ = w.WriteField(k, v)
	}
	p, _ := w.CreateFormField("body-mime")
	p.Write([]byte(raw))
	w.Close()
	r := httptest.NewRequest("POST", "/", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func TestReceiveMultipartRawMIME(t *testing.T) {
	var tr Transport
	path := t.TempDir() + "/m.eml"
	msg, binding, err := tr.Receive(context.Background(), multipartRequest(t, "key", "tok", "a@example.com", "From: b@test\r\nTo: a@example.com\r\n\r\nhello", nil), fakeResolver{key: "key"}, path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipient != "a@example.com" || msg.DeliveryID != "tok" || msg.ProviderMessageID != "<x@test>" {
		t.Fatalf("%+v", msg)
	}
	if binding.Recipient != "a@example.com" {
		t.Fatalf("binding %+v", binding)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte("hello")) {
		t.Fatal("raw mime missing")
	}
}

func TestReceiveURLEncoded(t *testing.T) {
	var tr Transport
	ts := fmtInt(time.Now().Unix())
	raw := "From: b@test\r\nTo: a@example.com\r\n\r\nhello"
	form := url.Values{
		"timestamp": {ts}, "token": {"tok"}, "signature": {signature("key", ts, "tok")},
		"sender": {"b@test"}, "recipient": {"a@example.com"}, "body-mime": {raw},
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	path := t.TempDir() + "/m.eml"
	msg, _, err := tr.Receive(context.Background(), r, fakeResolver{key: "key"}, path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Recipient != "a@example.com" || msg.DeliveryID != "tok" {
		t.Fatalf("%+v", msg)
	}
}

func TestReceiveRejectsBadSignature(t *testing.T) {
	var tr Transport
	req := multipartRequest(t, "wrong", "tok", "a@example.com", "body", nil)
	if _, _, err := tr.Receive(context.Background(), req, fakeResolver{key: "key"}, t.TempDir()+"/m.eml", 1024); err != transport.ErrInboundUnauthorized {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveRejectsDuplicateSingleton(t *testing.T) {
	var tr Transport
	req := multipartRequest(t, "key", "tok", "a@example.com", "body", map[string]string{"recipient": "other@example.com"})
	if _, _, err := tr.Receive(context.Background(), req, fakeResolver{key: "key"}, t.TempDir()+"/m.eml", 1024); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("err=%v", err)
	}
}

func TestReceiveRejectsTooManyMultipartParts(t *testing.T) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for i := 0; i < maxMultipartParts+1; i++ {
		_ = w.WriteField("field", fmt.Sprintf("v%d", i))
	}
	p, _ := w.CreateFormField("body-mime")
	p.Write([]byte("From: b@test\r\n\r\nhello"))
	w.Close()
	r := httptest.NewRequest("POST", "/", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	var tr Transport
	if _, _, err := tr.Receive(context.Background(), r, fakeResolver{key: "key"}, t.TempDir()+"/m.eml", 1024); err == nil || !strings.Contains(err.Error(), "too many multipart parts") {
		t.Fatalf("expected part-count error, got %v", err)
	}
}
