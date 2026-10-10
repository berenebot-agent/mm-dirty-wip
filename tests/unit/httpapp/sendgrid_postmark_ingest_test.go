package httpapp_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sgKeypair returns a fresh ECDSA key and its base64 SubjectPublicKeyInfo.
func sgKeypair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(der)
}

// sgRequest builds a signed SendGrid Inbound Parse webhook.
func sgRequest(t *testing.T, priv *ecdsa.PrivateKey, recipient, raw string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("envelope", fmt.Sprintf(`{"to":[%q],"from":"b@outside.test"}`, recipient))
	p, _ := w.CreateFormField("email")
	p.Write([]byte(raw))
	w.Close()
	body := b.Bytes()
	r := httptest.NewRequest("POST", "/internal/ingest/sendgrid", bytes.NewReader(body))
	r.Header.Set("Content-Type", w.FormDataContentType())
	ts := fmt.Sprintf("%d", time.Now().Unix())
	r.Header.Set("X-Twilio-Email-Event-Webhook-Timestamp", ts)
	h := sha256.New()
	h.Write([]byte(ts))
	h.Write(body)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, h.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Twilio-Email-Event-Webhook-Signature", base64.StdEncoding.EncodeToString(sig))
	return r
}

func TestSendGridInboundEndToEnd(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	priv, pub := sgKeypair(t)
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "sendgrid", map[string]any{"public_key": pub}, false); err != nil {
		t.Fatal(err)
	}
	raw := "From: Sender <b@outside.test>\r\nTo: " + box.Address + "\r\nMessage-ID: <sg-e2e@test>\r\nSubject: hello\r\n\r\nsynthetic body"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sgRequest(t, priv, box.Address, raw))
	if rr.Code != http.StatusOK {
		t.Fatalf("sendgrid ingest = %d %s", rr.Code, rr.Body.String())
	}
	// A bad signature is rejected before any delivery.
	rr = httptest.NewRecorder()
	_, otherPub := sgKeypair(t)
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "sendgrid", map[string]any{"public_key": otherPub}, false); err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(rr, sgRequest(t, priv, box.Address, raw))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d %s", rr.Code, rr.Body.String())
	}
}

func postmarkRequest(t *testing.T, recipient, rawEmail, messageID string) *http.Request {
	t.Helper()
	payload := map[string]any{
		"From":              "Sender <b@outside.test>",
		"OriginalRecipient": recipient,
		"MessageID":         messageID,
		"RawEmail":          rawEmail,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/internal/ingest/postmark", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestPostmarkInboundEndToEnd(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	saved, generated, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dom.ID, "postmark", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provider != "postmark" || generated["username"] == "" || generated["password"] == "" {
		t.Fatalf("generated credentials missing: %+v %+v", saved, generated)
	}
	creds := base64.StdEncoding.EncodeToString([]byte(generated["username"] + ":" + generated["password"]))
	raw := "From: b@outside.test\r\nTo: " + box.Address + "\r\nSubject: hi\r\n\r\nsynthetic body"

	do := func(auth string) *httptest.ResponseRecorder {
		r := postmarkRequest(t, box.Address, raw, "pm-e2e-1")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}
	if rr := do("Basic " + creds); rr.Code != http.StatusOK {
		t.Fatalf("postmark ingest = %d %s", rr.Code, rr.Body.String())
	}
	// A duplicate MessageID is accepted as a duplicate, not a second delivery.
	if rr := do("Basic " + creds); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate = %d %s", rr.Code, rr.Body.String())
	}
	// Wrong credentials are unauthorized.
	if rr := do("Basic " + base64.StdEncoding.EncodeToString([]byte("x:y"))); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad creds = %d %s", rr.Code, rr.Body.String())
	}
}

// TestPostmarkGeneratedCredentialFlash proves the generated Basic-auth URL is
// shown exactly once on the domain page.
func TestPostmarkGeneratedCredentialFlash(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{"_csrf": {csrf}, "provider": {"postmark"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save postmark %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "_flash=") {
		t.Fatalf("credential redirect missing flash: %q", loc)
	}
	page := domainGet(t, h, cookie, loc)
	if page.Code != http.StatusOK {
		t.Fatalf("credential page %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "domain-credential-url") || !strings.Contains(body, "/internal/ingest/postmark") {
		t.Fatalf("credential dialog missing webhook URL")
	}
	// A refresh no longer shows the credentials.
	if again := domainGet(t, h, cookie, loc).Body.String(); strings.Contains(again, "domain-credential-url") {
		t.Fatal("credentials shown after first view")
	}
}
