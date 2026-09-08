package httpapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func httpFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Domain, model.Inbox) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AppEncryptionKey: "01234567890123456789012345678901", MailgunSigningKey: "signing-secret", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
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
	return svc, New(svc, nil).Handler(), u, d, b
}

func TestPreAuthAndAuthenticatedCSRF(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, _ := app.New(cfg, st, events.NewHub())
	h := New(svc, nil).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/setup", nil))
	if rr.Code != 200 {
		t.Fatalf("setup get %d", rr.Code)
	}
	var csrfCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ghm_csrf" {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("missing preauth csrf cookie")
	}
	form := url.Values{"account": {"A"}, "email": {"admin@example.com"}, "password": {"correct horse battery staple"}}
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("setup without csrf = %d", rr.Code)
	}
	form.Set("_csrf", csrfCookie.Value)
	req = httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("setup with csrf=%d body=%s", rr.Code, rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ghm_session" {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad session cookie %#v", session)
	}
	req = httptest.NewRequest("POST", "/ui/domains", strings.NewReader("name=other.example"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("authenticated mutation without csrf=%d", rr.Code)
	}
}

func TestSecureCookieFollowsActualConnection(t *testing.T) {
	newHandler := func(t *testing.T, baseURL string, trustProxy bool) http.Handler {
		t.Helper()
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		cfg := config.Config{DataDir: dir, BaseURL: baseURL, Mode: "selfhosted", TrustProxyHeaders: trustProxy, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
		svc, err := app.New(cfg, st, events.NewHub())
		if err != nil {
			t.Fatal(err)
		}
		return New(svc, nil).Handler()
	}
	csrfCookie := func(t *testing.T, h http.Handler, req *http.Request) *http.Cookie {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("setup get %d", rr.Code)
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name == "ghm_csrf" {
				return c
			}
		}
		t.Fatal("missing preauth csrf cookie")
		return nil
	}
	newSetup := func(value string, cookie *http.Cookie) *http.Request {
		form := url.Values{"account": {"A"}, "email": {"admin@example.com"}, "password": {"correct horse battery staple"}, "_csrf": {value}}
		req := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		return req
	}
	// Direct plain HTTP with an https BaseURL: cookies must not be Secure,
	// otherwise browsers drop them and setup fails with "invalid CSRF token".
	h := newHandler(t, "https://mail.example.test", false)
	c := csrfCookie(t, h, httptest.NewRequest("GET", "/setup", nil))
	if c.Secure {
		t.Fatal("csrf cookie must not be Secure over direct plain HTTP")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newSetup(c.Value, c))
	if rr.Code != 303 {
		t.Fatalf("plain http setup with csrf=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == "ghm_session" && sc.Secure {
			t.Fatal("session cookie must not be Secure over direct plain HTTP")
		}
	}
	// Trusted proxy reporting https: cookies keep the Secure flag.
	h = newHandler(t, "https://mail.example.test", true)
	req := httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	c = csrfCookie(t, h, req)
	if !c.Secure {
		t.Fatal("csrf cookie must be Secure behind trusted https proxy")
	}
	// An untrusted X-Forwarded-Proto header must not enable Secure.
	h = newHandler(t, "https://mail.example.test", false)
	req = httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	c = csrfCookie(t, h, req)
	if c.Secure {
		t.Fatal("untrusted proxy header must not set Secure cookies")
	}
}

func signedMGRequest(t *testing.T, key, token, recipient, raw string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(ts + token))
	fields := map[string]string{"timestamp": ts, "token": token, "signature": hex.EncodeToString(mac.Sum(nil)), "sender": "sender@outside.test", "recipient": recipient}
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

func TestAgentAPIAndAttachmentDownload(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	raw := strings.Join([]string{"From: sender@outside.test", "To: hermes@example.com", "Subject: attachment", "Message-ID: <a@test>", "MIME-Version: 1.0", "Content-Type: multipart/mixed; boundary=x", "", "--x", "Content-Type: text/plain", "", "hello searchable", "--x", "Content-Type: text/html; name=attack.html", "Content-Disposition: attachment; filename=attack.html", "Content-Transfer-Encoding: base64", "", "PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", "--x--", ""}, "\r\n")
	m, _, err := svc.IngestMailgun(ctx, signedMGRequest(t, svc.Config.MailgunSigningKey, "d1", box.Address, raw))
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("bootstrap %d %s", rr.Code, rr.Body.String())
	}
	var boot map[string]any
	if err = json.Unmarshal(rr.Body.Bytes(), &boot); err != nil {
		t.Fatal(err)
	}
	atts, err := svc.Store.ListAttachments(ctx, model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}, m.ID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("atts %v %#v", err, atts)
	}
	req = httptest.NewRequest("GET", "/v1/attachments/"+atts[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("attachment %d", rr.Code)
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment;") || rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("unsafe headers %#v", rr.Header())
	}
	if rr.Body.String() != "<script>alert(1)</script>" {
		t.Fatalf("attachment bytes %q", rr.Body.String())
	}
	req = httptest.NewRequest("DELETE", "/v1/messages/"+m.ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("read delete=%d", rr.Code)
	}
}

func TestUnknownRecipientIs406(t *testing.T) {
	svc, h, _, _, _ := httpFixture(t)
	raw := "From: x@y.test\r\nTo: missing@example.com\r\n\r\nhi"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, signedMGRequest(t, svc.Config.MailgunSigningKey, "unknown1", "missing@example.com", raw))
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOpenAgentCompatibilityCommonFlow(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, adminKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/identities", strings.NewReader(`{"name":"Signup","localpart":"fox"}`))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("identity create %d %s", rr.Code, rr.Body.String())
	}
	var ident struct{ Address, Token string }
	if err = json.Unmarshal(rr.Body.Bytes(), &ident); err != nil {
		t.Fatal(err)
	}
	if ident.Address != "fox@example.com" || ident.Token == "" {
		t.Fatalf("identity %#v", ident)
	}
	raw := "From: noreply@service.test\r\nTo: fox@example.com\r\nSubject: Verify account\r\nMessage-ID: <verify@test>\r\n\r\nverification body"
	m, _, err := svc.IngestMailgun(ctx, signedMGRequest(t, svc.Config.MailgunSigningKey, "compat-d1", ident.Address, raw))
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "/v1/messages?address="+url.QueryEscape(ident.Address), nil)
	req.Header.Set("Authorization", "Bearer "+ident.Token)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"messages"`) {
		t.Fatalf("compat messages %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("POST", "/v1/messages/"+m.ID+"/seen", strings.NewReader(`{"address":"fox@example.com","seen":true}`))
	req.Header.Set("Authorization", "Bearer "+ident.Token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"seen":true`) {
		t.Fatalf("seen %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("POST", "/v1/messages/wait", strings.NewReader(`{"address":"fox@example.com","subjectContains":"verify","timeoutSec":1}`))
	req.Header.Set("Authorization", "Bearer "+ident.Token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(strings.ToLower(rr.Body.String()), "verify account") {
		t.Fatalf("wait %d %s", rr.Code, rr.Body.String())
	}
	// The compatible send shape accepts a string `to` and `from` address.
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"<compat-out>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "MG", "mailgun", map[string]any{"api_key": "key", "domain": "mg.example.com", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetActiveOutboundCredential(ctx, u.AccountID, cred.ID); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "/v1/send", strings.NewReader(`{"from":"fox@example.com","to":"friend@example.net","subject":"hello","text":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+ident.Token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || calls.Load() != 1 || !strings.Contains(rr.Body.String(), `"queued":true`) {
		t.Fatalf("compat send %d %s calls=%d", rr.Code, rr.Body.String(), calls.Load())
	}
}
