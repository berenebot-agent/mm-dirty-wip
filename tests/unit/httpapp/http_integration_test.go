package httpapp_test

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

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

func httpFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Domain, model.Inbox) {
	t.Helper()
	return httpFixtureWithActionWorker(t, true)
}

// httpFixtureWithoutActionWorker leaves queued message actions pending so tests
// can exercise in-flight behavior without the live worker settling them first.
func httpFixtureWithoutActionWorker(t *testing.T) (*app.Service, http.Handler, model.User, model.Domain, model.Inbox) {
	t.Helper()
	return httpFixtureWithActionWorker(t, false)
}

func httpFixtureWithActionWorker(t *testing.T, runWorker bool) (*app.Service, http.Handler, model.User, model.Domain, model.Inbox) {
	t.Helper()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
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
	seedInboundCredential(t, svc, u.AccountID, d.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	h := httpapp.New(svc, nil).Handler()
	if runWorker {
		h = settledActionFixture(t, svc, app.NewRemoteMailboxService(svc), h, u.AccountID)
	}
	return svc, h, u, d, b
}

const (
	testMailgunKey = "signing-secret"
	testCFSecret   = "cf-secret"
)

// seedInboundCredential configures the domain's receive path, mirroring the
// domain-first setup flow.
func seedInboundCredential(t *testing.T, svc *app.Service, accountID, domainID, provider string, cfg map[string]any) {
	t.Helper()
	if _, _, err := svc.SaveDomainReceivingConfig(context.Background(), accountID, domainID, provider, cfg, false); err != nil {
		t.Fatal(err)
	}
}

func TestPreAuthCSRFErrorGuidance(t *testing.T) {
	t.Parallel()
	st, dir := testdb.OpenDir(t)
	cfg := config.Config{DataDir: dir, BaseURL: "https://mail.example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	h := httpapp.New(svc, nil).Handler()
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "http://mail.example.test/setup", nil))
	var cookie *http.Cookie
	for _, c := range get.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure {
		t.Fatal("HTTPS BASE_URL must still set a Secure CSRF cookie")
	}
	for _, route := range []string{"/setup", "/register", "/login", "/login/key", "/invite/example"} {
		for _, failure := range []string{"missing-cookie", "missing-form-token", "mismatch", "unsigned"} {
			t.Run(route+"/"+failure, func(t *testing.T) {
				form := url.Values{"_csrf": {cookie.Value}}
				c := *cookie
				switch failure {
				case "missing-form-token":
					form.Del("_csrf")
				case "mismatch":
					form.Set("_csrf", "wrong")
				case "unsigned":
					c.Value = "unsigned"
					form.Set("_csrf", c.Value)
				}
				req := httptest.NewRequest("POST", "http://mail.example.test"+route, strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if failure != "missing-cookie" {
					req.AddCookie(&c)
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
				}
				for _, text := range []string{"Unable to verify your sign-in request", "Refresh the page", "cookies are enabled", "BASE_URL", "http://", "https://", "Return to sign in"} {
					if !strings.Contains(rr.Body.String(), text) {
						t.Errorf("error page missing %q", text)
					}
				}
				if rr.Header().Get("Content-Type") != "text/html; charset=utf-8" || rr.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("unexpected error page headers: %v", rr.Header())
				}
			})
		}
	}
}

func TestPreAuthAndAuthenticatedCSRF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := testdb.OpenAt(t, dir)
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, _ := app.New(cfg, st, events.NewHub())
	h := httpapp.New(svc, nil).Handler()
	// The system administrator now comes from ADMIN_* at startup, so seed an
	// ordinary account Admin directly for this HTTP-level test.
	if _, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != 200 {
		t.Fatalf("login get %d", rr.Code)
	}
	var csrfCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_csrf" {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("missing preauth csrf cookie")
	}
	form := url.Values{"email": {"admin@example.com"}, "password": {"correct horse battery staple"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("login without csrf = %d", rr.Code)
	}
	form.Set("_csrf", csrfCookie.Value)
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(csrfCookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("login with csrf=%d body=%s", rr.Code, rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "mmm_session" {
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

func TestSecureCookieLoginFlow(t *testing.T) {
	t.Parallel()
	newHandler := func(t *testing.T, baseURL string, trustProxy bool) http.Handler {
		t.Helper()
		dir := t.TempDir()
		st := testdb.OpenAt(t, dir)
		t.Cleanup(func() { st.Close() })
		if _, err := st.CreateInitialAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20); err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{DataDir: dir, BaseURL: baseURL, Mode: "selfhosted", AllowPrivateOutbound: true, TrustProxyHeaders: trustProxy, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
		svc, err := app.New(cfg, st, events.NewHub())
		if err != nil {
			t.Fatal(err)
		}
		return httpapp.New(svc, nil).Handler()
	}
	csrfCookie := func(t *testing.T, h http.Handler, req *http.Request) *http.Cookie {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("login get %d", rr.Code)
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name == "mmm_csrf" {
				return c
			}
		}
		t.Fatal("missing preauth csrf cookie")
		return nil
	}
	newLogin := func(value string, cookie *http.Cookie) *http.Request {
		form := url.Values{"email": {"admin@example.com"}, "password": {"correct horse battery staple"}, "_csrf": {value}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		return req
	}
	// An https BaseURL always marks cookies Secure, even for a request the app
	// sees over plain HTTP (behind a TLS-terminating proxy with no trusted
	// proxy). The browser stores and returns them over the https origin.
	h := newHandler(t, "https://mail.example.test", false)
	c := csrfCookie(t, h, httptest.NewRequest("GET", "/login", nil))
	if !c.Secure {
		t.Fatal("csrf cookie must be Secure when BaseURL is https")
	}
	// A Secure cookie is still set on the response; a real browser would return
	// it over TLS. The login flow itself must succeed with the presented value.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newLogin(c.Value, c))
	if rr.Code != 303 {
		t.Fatalf("login with csrf=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == "mmm_session" && !sc.Secure {
			t.Fatal("session cookie must be Secure when BaseURL is https")
		}
	}
	// A plain-HTTP self-hosted deployment keeps non-Secure cookies so the
	// browser does not drop them and break login with "invalid CSRF token".
	h = newHandler(t, "http://example.test", false)
	c = csrfCookie(t, h, httptest.NewRequest("GET", "/login", nil))
	if c.Secure {
		t.Fatal("csrf cookie must not be Secure when BaseURL is http")
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
	r := httptest.NewRequest("POST", "/internal/ingest/mailgun/raw-mime", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestAgentAPIAndAttachmentDownload(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	raw := strings.Join([]string{"From: sender@outside.test", "To: hermes@example.com", "Subject: attachment", "Message-ID: <a@test>", "MIME-Version: 1.0", "Content-Type: multipart/mixed; boundary=x", "", "--x", "Content-Type: text/plain", "", "hello searchable", "--x", "Content-Type: text/html; name=attack.html", "Content-Disposition: attachment; filename=attack.html", "Content-Transfer-Encoding: base64", "", "PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==", "--x--", ""}, "\r\n")
	m, _, err := svc.IngestInbound(ctx, "mailgun", signedMGRequest(t, testMailgunKey, "d1", box.Address, raw))
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
	for _, path := range []string{
		fmt.Sprintf("/v1/messages/%s/attachments/%d", m.ID, atts[0].PartIndex),
		fmt.Sprintf("/v1/inboxes/%s/messages/%s/attachments/%d", box.ID, m.ID, atts[0].PartIndex),
	} {
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 || rr.Body.String() != "<script>alert(1)</script>" {
			t.Fatalf("%s: %d %q", path, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment;") {
			t.Fatalf("%s unsafe headers: %v", path, rr.Header())
		}
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
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	raw := "From: x@y.test\r\nTo: missing@example.com\r\n\r\nhi"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, signedMGRequest(t, testMailgunKey, "unknown1", "missing@example.com", raw))
	if rr.Code != http.StatusNotAcceptable {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOpenAgentCompatibilityCommonFlow(t *testing.T) {
	t.Parallel()
	svc, h, u, dom, _ := httpFixture(t)
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
	m, _, err := svc.IngestInbound(ctx, "mailgun", signedMGRequest(t, testMailgunKey, "compat-d1", ident.Address, raw))
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
	if _, err = svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "mailgun", map[string]any{"api_key": "key", "domain": "mg.example.com", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "/v1/send", strings.NewReader(`{"from":"fox@example.com","to":"friend@example.net","subject":"hello","text":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+ident.Token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"queued":true`) {
		t.Fatalf("compat send %d %s", rr.Code, rr.Body.String())
	}
	// Deliver the queued message via the worker path.
	var sendResp struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &sendResp)
	if sendResp.Message.ID == "" {
		t.Fatalf("no message id in %s", rr.Body.String())
	}
	if err = svc.Deliver(ctx, u.AccountID, sendResp.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

// TestMalformedJSONIsRejected guards the handlers that used to ignore a JSON
// decode error. apiSeen defaulted to seen=true, so a malformed body silently
// marked a message read; it must now be a 400 and must not touch the message.
func TestMalformedJSONIsRejected(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: sender@example.net\r\nTo: hermes@example.com\r\nSubject: Malformed\r\nMessage-ID: <malformed@test>\r\n\r\nbody"
	m, _, err := svc.IngestInbound(ctx, "mailgun", signedMGRequest(t, testMailgunKey, "malformed-d1", "hermes@example.com", raw))
	if err != nil {
		t.Fatal(err)
	}
	// A malformed body must be rejected, not interpreted as the default.
	req := httptest.NewRequest("POST", "/v1/messages/"+m.ID+"/seen", strings.NewReader(`{"seen":`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("malformed seen body accepted: %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetMessage(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Read {
		t.Fatal("malformed body marked the message read")
	}
	// A malformed wait body is likewise rejected rather than defaulted.
	req = httptest.NewRequest("POST", "/v1/messages/wait", strings.NewReader(`{"timeout":`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("malformed wait body accepted: %d %s", rr.Code, rr.Body.String())
	}
	// An absent body keeps its all-defaults behaviour: a plain POST marks read.
	req = httptest.NewRequest("POST", "/v1/messages/"+m.ID+"/seen", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("empty seen body rejected: %d %s", rr.Code, rr.Body.String())
	}
	_ = box
}
