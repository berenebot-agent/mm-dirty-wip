package mailgun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func signature(key, ts, tok string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(ts + tok))
	return hex.EncodeToString(m.Sum(nil))
}
func TestSignatureFreshness(t *testing.T) {
	key := "secret"
	tok := "abc"
	ts := time.Now().Unix()
	s := time.Unix(ts, 0).Format("150405")
	_ = s
	now := time.Now().Format("20060102150405")
	_ = now
	tsi := fmtInt(ts)
	if !VerifySignature(key, tsi, tok, signature(key, tsi, tok)) {
		t.Fatal("valid signature rejected")
	}
	old := fmtInt(time.Now().Add(-time.Hour).Unix())
	if VerifySignature(key, old, tok, signature(key, old, tok)) {
		t.Fatal("stale signature accepted")
	}
}
func fmtInt(v int64) string { return fmt.Sprintf("%d", v) }
func TestParseMultipartRawMIME(t *testing.T) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range map[string]string{"timestamp": fmtInt(time.Now().Unix()), "token": "tok", "signature": "sig", "recipient": "a@example.com", "sender": "b@test"} {
		_ = w.WriteField(k, v)
	}
	p, _ := w.CreateFormField("body-mime")
	p.Write([]byte("From: b@test\r\nTo: a@example.com\r\n\r\nhello"))
	w.Close()
	r := httptest.NewRequest("POST", "/", &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	path := t.TempDir() + "/m.eml"
	f, err := ParseInboundRequest(r, path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if f.Token != "tok" || f.Recipient != "a@example.com" {
		t.Fatalf("%+v", f)
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte("hello")) {
		t.Fatal("raw mime missing")
	}
}

func TestParseRejectsTooManyMultipartParts(t *testing.T) {
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
	if _, err := ParseInboundRequest(r, t.TempDir()+"/m.eml", 1024); err == nil || !strings.Contains(err.Error(), "too many multipart parts") {
		t.Fatalf("expected part-count error, got %v", err)
	}
}
