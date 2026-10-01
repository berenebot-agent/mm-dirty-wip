package httpapp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
)

// testTLSPair generates a self-signed certificate and its matching private key
// as PEM, so the STARTTLS pair validation and the write-only key handling can be
// exercised without shipping a fixture.
func testTLSPair(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mx.test"},
		DNSNames:     []string{"mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// sessionReq builds a request carrying the system administrator's session
// cookie and, for writes, the CSRF token. An empty csrf omits it so a test can
// assert the CSRF gate.
func sessionReq(t *testing.T, method, path, csrf, body string, cookie *http.Cookie) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	req.AddCookie(cookie)
	return req
}

// TestMXSettingsAPIRoundTrip exercises the installation MX settings API through
// the system administrator's cookie session: an empty read, an included save
// (generated credentials and advanced limits, no echo of the secret), a remote
// save and blank-key retention, CAS conflicts, and clear.
func TestMXSettingsAPIRoundTrip(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, method, path, csrf, body, cookie))
		return rr
	}

	// Unconfigured read: empty mode, no secret, status disabled, no-store.
	rr := do(http.MethodGet, "/v1/admin/mx", "")
	if rr.Code != 200 {
		t.Fatalf("empty get: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("MX settings response must be no-store: %q", rr.Header().Get("Cache-Control"))
	}
	var empty mxResp
	if err := json.Unmarshal(rr.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Mode != "" || empty.KeyConfigured || empty.Revision != 0 || empty.Status.State != "disabled" {
		t.Fatalf("empty settings = %s", rr.Body.String())
	}

	// Save included with advanced limits; the bearer key is generated and never
	// echoed, and the live status must not claim ready without a runtime.
	rr = do(http.MethodPut, "/v1/admin/mx", `{"mode":"included","hostname":"mx.test","max_message_bytes":1048576,"max_recipients":25,"max_connections":50,"bearer_key":"ignored"}`)
	if rr.Code != 200 {
		t.Fatalf("included save: %d %s", rr.Code, rr.Body.String())
	}
	var inc mxResp
	if err := json.Unmarshal(rr.Body.Bytes(), &inc); err != nil {
		t.Fatal(err)
	}
	if inc.Mode != "included" || !inc.KeyConfigured || inc.Revision != 1 {
		t.Fatalf("included resp = %s", rr.Body.String())
	}
	if inc.BearerKey != "" {
		t.Fatal("read must not echo the bearer key")
	}
	if inc.MaxMessageBytes != 1048576 || inc.MaxRecipients != 25 || inc.MaxConnections != 50 {
		t.Fatalf("included limits not returned: %s", rr.Body.String())
	}
	if inc.Status.State == "active" {
		t.Fatalf("status must not claim ready without a live receiver: %s", rr.Body.String())
	}

	// A create against an existing row conflicts.
	if rr = do(http.MethodPut, "/v1/admin/mx", `{"mode":"included","revision":0}`); rr.Code != 409 {
		t.Fatalf("create on existing = %d %s", rr.Code, rr.Body.String())
	}

	// Switch to remote with a supplied key, then save again with a blank key:
	// the stored credential must be retained.
	rr = do(http.MethodPut, "/v1/admin/mx", `{"mode":"remote","url":"https://r.example/","bearer_key":"remote-secret","revision":1}`)
	if rr.Code != 200 {
		t.Fatalf("remote save: %d %s", rr.Code, rr.Body.String())
	}
	var remote mxResp
	if err := json.Unmarshal(rr.Body.Bytes(), &remote); err != nil {
		t.Fatal(err)
	}
	if remote.URL != "https://r.example" || !remote.KeyConfigured || remote.Revision != 2 {
		t.Fatalf("remote resp = %s", rr.Body.String())
	}
	rr = do(http.MethodPut, "/v1/admin/mx", `{"mode":"remote","url":"https://r2.example","revision":2}`)
	if rr.Code != 200 {
		t.Fatalf("blank-key save: %d %s", rr.Code, rr.Body.String())
	}
	rt, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rt.BearerKey != "remote-secret" || rt.URL != "https://r2.example" {
		t.Fatalf("blank remote key did not retain: %+v", rt)
	}

	// A remote receiver without a URL is a client error.
	if rr = do(http.MethodPut, "/v1/admin/mx", `{"mode":"remote","bearer_key":"x","revision":3}`); rr.Code != 400 {
		t.Fatalf("remote without URL = %d %s", rr.Code, rr.Body.String())
	}

	// Clear requires the revision and leaves an initialized, unconfigured row
	// (so the one-time import never re-fires).
	if rr = do(http.MethodDelete, "/v1/admin/mx", ""); rr.Code != 400 {
		t.Fatalf("clear without revision = %d %s", rr.Code, rr.Body.String())
	}
	if rr = do(http.MethodDelete, "/v1/admin/mx?revision=3", ""); rr.Code != 204 {
		t.Fatalf("clear = %d %s", rr.Code, rr.Body.String())
	}
	initialized, err := svc.Store.MXSettingsInitialized(ctx)
	if err != nil || !initialized {
		t.Fatalf("clear must leave an initialized row: %v %v", initialized, err)
	}
}

// TestMXSettingsAPICSRFDenied pins that a write without a CSRF token is refused
// even with a valid system-administrator session, matching the UI write path.
func TestMXSettingsAPICSRFDenied(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	for _, rt := range []struct {
		method, path, body string
	}{
		{http.MethodPut, "/v1/admin/mx", `{"mode":"included"}`},
		{http.MethodDelete, "/v1/admin/mx?revision=1", ""},
	} {
		// Missing token.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, rt.method, rt.path, "", rt.body, cookie))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s without CSRF = %d %s", rt.method, rt.path, rr.Code, rr.Body.String())
		}
		// Wrong token.
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, rt.method, rt.path, "not-the-token", rt.body, cookie))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s with bad CSRF = %d %s", rt.method, rt.path, rr.Code, rr.Body.String())
		}
	}
	// Nothing was written.
	settings, err := svc.GetMXReceiverSettings(context.Background())
	if err != nil || settings.Mode != "" {
		t.Fatalf("CSRF failure must not change settings: %+v %v", settings, err)
	}
	_ = csrf
}

// TestMXSettingsAPIBearerKeyForbidden pins that an account bearer API key (the
// /v1 default) cannot reach the installation MX settings, even an Admin key.
func TestMXSettingsAPIBearerKeyForbidden(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/admin/mx", ""},
		{http.MethodPut, "/v1/admin/mx", `{"mode":"included"}`},
		{http.MethodDelete, "/v1/admin/mx?revision=1", ""},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		// A bearer request carries no session cookie: the installation
		// middleware answers 401 and never routes to the handler.
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s as bearer key = %d %s", rt.method, rt.path, rr.Code, rr.Body.String())
		}
	}
	// No bearer-key request can have written a configuration.
	settings, err := svc.GetMXReceiverSettings(ctx)
	if err != nil || settings.Mode != "" {
		t.Fatalf("bearer key must not configure MX: %+v %v", settings, err)
	}
}

// TestMXSettingsAPINonSystemAdminSessionForbidden pins that a logged-in session
// without the system-administrator role is refused, so an account Admin cannot
// edit installation state.
func TestMXSettingsAPINonSystemAdminSessionForbidden(t *testing.T) {
	svc, h, _, _, _ := httpFixture(t)
	// httpFixture's bootstrap user is an account Admin but not a system admin.
	u, err := svc.Store.GetUserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.SystemAdmin {
		t.Fatal("fixture user unexpectedly system admin")
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sessionReq(t, http.MethodGet, "/v1/admin/mx", "", "", cookie))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-system-admin session read = %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, `{"mode":"included"}`, cookie))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-system-admin session write = %d %s", rr.Code, rr.Body.String())
	}
}

// TestAdminMXUISaveAndClear exercises the /admin MX form: it renders the three
// receiver choices (Auto disabled), saves a remote receiver without echoing the
// bearer key, and clears it, all with CSRF.
func TestAdminMXUISaveAndClear(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	page := uiGet(t, h, cookie, "/admin")
	body := page.Body.String()
	for _, want := range []string{"MX receiver", `value="auto" disabled`, `value="included"`, `value="remote"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin MX section missing %q", want)
		}
	}

	if rr := uiPost(t, h, cookie, "/ui/admin/mx", url.Values{"mode": {"remote"}}.Encode()); rr.Code != http.StatusForbidden {
		t.Fatalf("save without CSRF = %d", rr.Code)
	}

	form := url.Values{"_csrf": {csrf}, "revision": {"0"}, "mode": {"remote"}, "url": {"https://r.example"}, "bearer_key": {"sekret"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("remote save: %d %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.GetMXReceiverSettings(ctx)
	if err != nil || settings.Mode != "remote" || settings.URL != "https://r.example" || !settings.KeyConfigured {
		t.Fatalf("saved settings = %+v %v", settings, err)
	}
	page = uiGet(t, h, cookie, "/admin")
	if strings.Contains(page.Body.String(), "sekret") {
		t.Fatal("admin page echoed the bearer key")
	}

	// A validation error is surfaced on the redirect target.
	bad := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}, "mode": {"remote"}, "url": {""}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", bad.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid save = %d %s", rr.Code, rr.Body.String())
	}

	clear := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx/clear", clear.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	cleared, err := svc.GetMXReceiverSettings(ctx)
	if err != nil || cleared.Mode != "" {
		t.Fatalf("cleared settings = %+v %v", cleared, err)
	}
}

// TestMXSettingsAPIMatchesAppStruct keeps the HTTP body in step with the
// service contract: the included advanced fields round-trip through the JSON
// handler into the persisted runtime settings.
func TestMXSettingsAPIMatchesAppStruct(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	body := `{"mode":"included","hostname":"mx.test","max_message_bytes":2097152,"max_recipients":3,"max_connections":4}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, body, cookie))
	if rr.Code != 200 {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	rt, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Mode != app.MXModeIncluded || rt.Hostname != "mx.test" || rt.MaxMessageBytes != 2097152 || rt.MaxRecipients != 3 || rt.MaxConnections != 4 {
		t.Fatalf("fields did not round-trip: %+v", rt)
	}
	if !rt.VerifySPFEnabled() || !rt.VerifyDKIMEnabled() || !rt.VerifyDMARCEnabled() {
		t.Fatalf("verification toggles must default on: %+v", rt)
	}
}

// TestMXSettingsAPIVerifyTogglesStrict pins the API verification contract:
// omitting a toggle defaults on, an explicit false is persisted as off, and the
// stored tri-state is returned verbatim so a GET reflects the operator's
// choice.
func TestMXSettingsAPIVerifyTogglesStrict(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	do := func(body string) mxResp {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, body, cookie))
		if rr.Code != 200 {
			t.Fatalf("save %s: %d %s", body, rr.Code, rr.Body.String())
		}
		var out mxResp
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Omitted toggles: defaults on, and the response omits the unset pointers
	// so a GET->PUT round-trip cannot turn "default" into "explicit on".
	if out := do(`{"mode":"included"}`); out.VerifySPF != nil || out.VerifyDKIM != nil || out.VerifyDMARC != nil {
		t.Fatalf("unset toggles must be omitted: %+v", out)
	}
	rt, _ := svc.MXReceiverSettingsForRuntime(ctx)
	if !rt.VerifySPFEnabled() || !rt.VerifyDKIMEnabled() || !rt.VerifyDMARCEnabled() {
		t.Fatalf("unset toggles must resolve on: %+v", rt)
	}

	// Explicit false persists as off and is returned as false (present).
	out := do(`{"mode":"included","revision":1,"verify_dkim":false,"verify_spf":true}`)
	if out.VerifyDKIM == nil || *out.VerifyDKIM {
		t.Fatalf("verify_dkim must be returned as explicit false: %+v", out)
	}
	if out.VerifySPF == nil || !*out.VerifySPF {
		t.Fatalf("verify_spf must be returned as explicit true: %+v", out)
	}
	if out.VerifyDMARC != nil {
		t.Fatalf("omitted verify_dmarc must stay unset: %+v", out)
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if rt.VerifyDKIMEnabled() || !rt.VerifySPFEnabled() || !rt.VerifyDMARCEnabled() {
		t.Fatalf("resolved toggles wrong: spf=%v dkim=%v dmarc=%v", rt.VerifySPFEnabled(), rt.VerifyDKIMEnabled(), rt.VerifyDMARCEnabled())
	}

	// A remote save may not carry a verification toggle.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, `{"mode":"remote","url":"https://r.example","bearer_key":"k","revision":2,"verify_spf":false}`, cookie))
	if rr.Code != 400 {
		t.Fatalf("remote save with verify toggle = %d %s", rr.Code, rr.Body.String())
	}
}

// TestAdminMXUIVerifyCheckboxes pins that the /admin form renders the
// verification boxes checked by default and saves an unchecked box as an
// explicit off, and that switching to remote does not leak included-only fields
// (which the strict API would reject).
func TestAdminMXUIVerifyCheckboxes(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	// Fresh included save: boxes render checked (default on).
	inc := url.Values{"_csrf": {csrf}, "revision": {"0"}, "mode": {"included"}, "hostname": {"mx.test"},
		"max_message_bytes": {"2097152"}, "max_recipients": {"10"}, "max_connections": {"20"},
		"verify_spf": {"true"}, "verify_dkim": {"true"}, "verify_dmarc": {"true"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", inc.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("included save: %d %s", rr.Code, rr.Body.String())
	}
	page := uiGet(t, h, cookie, "/admin")
	// Each of the three boxes must be rendered checked on a fresh default.
	if got := strings.Count(page.Body.String(), `name="verify_spf" value="true" checked`); got != 1 {
		t.Fatalf("verify_spf box should be checked by default (found %d)", got)
	}
	settings, _ := svc.GetMXReceiverSettings(ctx)
	if !settings.VerifySPFEnabled() || !settings.VerifyDKIMEnabled() || !settings.VerifyDMARCEnabled() {
		t.Fatalf("fresh save did not default verification on: %+v", settings)
	}

	// Uncheck DMARC and SPF; only DKIM stays checked.
	off := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}, "mode": {"included"}, "hostname": {"mx.test"},
		"max_message_bytes": {"2097152"}, "max_recipients": {"10"}, "max_connections": {"20"},
		"verify_dkim": {"true"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", off.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("off save: %d %s", rr.Code, rr.Body.String())
	}
	settings, _ = svc.GetMXReceiverSettings(ctx)
	if settings.VerifySPFEnabled() || settings.VerifyDMARCEnabled() || !settings.VerifyDKIMEnabled() {
		t.Fatalf("unchecked boxes must persist off: spf=%v dkim=%v dmarc=%v", settings.VerifySPFEnabled(), settings.VerifyDKIMEnabled(), settings.VerifyDMARCEnabled())
	}
	// The page must now render SPF/DMARC unchecked and DKIM checked.
	page = uiGet(t, h, cookie, "/admin")
	if strings.Contains(page.Body.String(), `name="verify_spf" value="true" checked`) {
		t.Fatal("verify_spf should now be unchecked")
	}
	if !strings.Contains(page.Body.String(), `name="verify_dkim" value="true" checked`) {
		t.Fatal("verify_dkim should still be checked")
	}

	// Switching to remote must not send the stale included-only fields, which
	// the API rejects.
	toRemote := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}, "mode": {"remote"},
		"url": {"https://r.example"}, "bearer_key": {"k"},
		// These are present in the browser form because both blocks render.
		"hostname": {"mx.test"}, "max_recipients": {"10"}, "verify_spf": {"true"}}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", toRemote.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("remote switch: %d %s", rr.Code, rr.Body.String())
	}
	remote, _ := svc.GetMXReceiverSettings(ctx)
	if remote.Mode != app.MXModeRemote || remote.Hostname != "" || remote.MaxRecipients != 0 {
		t.Fatalf("remote switch leaked included-only fields: %+v", remote)
	}
}

// TestMXSettingsAPIRoundTripsGETBody verifies a client can take the GET
// response verbatim and PUT it back: the read-only fields the GET adds
// (key_configured, smtp_tls_key_configured, status, updated_at) must not be
// rejected as unknown, for both a remote and a full included configuration.
func TestMXSettingsAPIRoundTripsGETBody(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	certPEM, keyPEM := testTLSPair(t)
	cases := []struct {
		name, seed string
	}{
		{"remote", `{"mode":"remote","url":"https://r.example","bearer_key":"k"}`},
		{"included", `{"mode":"included","hostname":"mx.test","require_tls":true,"smtp_tls_cert":` + jsonString(certPEM) + `,"smtp_tls_key":` + jsonString(keyPEM) + `}`},
	}
	for _, tc := range cases {
		cur, err := svc.GetMXReceiverSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		seed := strings.Replace(tc.seed, "{", `{"revision":`+strconv.FormatInt(cur.Revision, 10)+`,`, 1)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, seed, cookie))
		if rr.Code != 200 {
			t.Fatalf("%s seed: %d %s", tc.name, rr.Code, rr.Body.String())
		}
		// GET, then PUT the body back unchanged.
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, http.MethodGet, "/v1/admin/mx", "", "", cookie))
		if rr.Code != 200 {
			t.Fatalf("%s get: %d %s", tc.name, rr.Code, rr.Body.String())
		}
		getBody := strings.TrimSpace(rr.Body.String())
		if tc.name == "included" && !strings.Contains(getBody, `"smtp_tls_key_configured":true`) {
			t.Fatalf("%s GET must report the key is configured: %s", tc.name, getBody)
		}
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, getBody, cookie))
		if rr.Code != 200 {
			t.Fatalf("%s round-trip put: %d %s", tc.name, rr.Code, rr.Body.String())
		}
	}
}

// TestMXSettingsAPIFullIncludedConfig saves the complete advanced Included
// configuration through the API and checks every field round-trips, that the
// private STARTTLS key is never returned (only smtp_tls_key_configured), and
// that clearing the certificate clears the retained key.
func TestMXSettingsAPIFullIncludedConfig(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	certPEM, keyPEM := testTLSPair(t)
	do := func(method, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, method, "/v1/admin/mx", csrf, body, cookie))
		return rr
	}

	body := `{"mode":"included","hostname":"mx.test","max_message_bytes":5242880,"max_staging_bytes":268435456,"max_recipients":100,"max_connections":256,` +
		`"require_tls":true,"dns_resolver":"9.9.9.9:53","dns_timeout_seconds":10,"read_timeout_seconds":60,"write_timeout_seconds":60,"data_timeout_seconds":300,` +
		`"smtp_tls_cert":` + jsonString(certPEM) + `,"smtp_tls_key":` + jsonString(keyPEM) + `}`
	rr := do(http.MethodPut, body)
	if rr.Code != 200 {
		t.Fatalf("full save: %d %s", rr.Code, rr.Body.String())
	}
	var out mxResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.MaxMessageBytes != 5242880 || out.MaxStagingBytes != 268435456 || out.MaxRecipients != 100 || out.MaxConnections != 256 {
		t.Fatalf("limits did not round-trip: %s", rr.Body.String())
	}
	if out.RequireTLS == nil || !*out.RequireTLS {
		t.Fatalf("require_tls must be returned true: %s", rr.Body.String())
	}
	if out.DNSResolver != "9.9.9.9:53" || out.DNSTimeoutSeconds != 10 || out.ReadTimeoutSeconds != 60 || out.WriteTimeoutSeconds != 60 || out.DataTimeoutSeconds != 300 {
		t.Fatalf("dns/timeouts did not round-trip: %s", rr.Body.String())
	}
	if out.SMTPTLSCert == "" {
		t.Fatalf("public certificate must be returned: %s", rr.Body.String())
	}
	if out.SMTPTLSKey != "" {
		t.Fatal("private STARTTLS key must never be returned by GET")
	}
	if !out.SMTPTLSKeyConfigured {
		t.Fatalf("smtp_tls_key_configured must be true: %s", rr.Body.String())
	}
	// The runtime accessor is the only place the key is materialised.
	rt, err := svc.MXReceiverSettingsForRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(rt.SMTPTLSKey) == "" || strings.TrimSpace(rt.SMTPTLSCert) == "" {
		t.Fatalf("runtime must see the TLS pair: %+v", rt)
	}

	// A same-form save with a blank key retains the stored one.
	rr = do(http.MethodPut, `{"mode":"included","revision":1,"smtp_tls_cert":`+jsonString(certPEM)+`}`)
	if rr.Code != 200 {
		t.Fatalf("blank-key save: %d %s", rr.Code, rr.Body.String())
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if strings.TrimSpace(rt.SMTPTLSKey) == "" {
		t.Fatal("blank key did not retain the stored private key")
	}

	// Clearing the certificate (blank key) removes the whole pair.
	rr = do(http.MethodPut, `{"mode":"included","revision":2}`)
	if rr.Code != 200 {
		t.Fatalf("clear TLS: %d %s", rr.Code, rr.Body.String())
	}
	var cleared mxResp
	if err := json.Unmarshal(rr.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.SMTPTLSCert != "" || cleared.SMTPTLSKeyConfigured {
		t.Fatalf("clearing the certificate must clear the pair: %s", rr.Body.String())
	}
	rt, _ = svc.MXReceiverSettingsForRuntime(ctx)
	if rt.SMTPTLSCert != "" || rt.SMTPTLSKey != "" {
		t.Fatalf("runtime still holds a cleared pair: %+v", rt)
	}
}

// TestMXSettingsAPIMalformedTLSRejected pins that a malformed STARTTLS pair and
// a cert-without-key are rejected with a field-level 400, and that RequireTLS
// without a certificate is refused.
func TestMXSettingsAPIMalformedTLSRejected(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	certPEM, _ := testTLSPair(t)
	cases := []struct {
		name, body string
	}{
		{"cert without key", `{"mode":"included","smtp_tls_cert":` + jsonString(certPEM) + `}`},
		{"malformed pair", `{"mode":"included","smtp_tls_cert":"not a cert","smtp_tls_key":"not a key"}`},
		{"require tls without cert", `{"mode":"included","require_tls":true}`},
		{"negative timeout", `{"mode":"included","dns_timeout_seconds":-1}`},
		{"staging below message", `{"mode":"included","max_message_bytes":2097152,"max_staging_bytes":1048576}`},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, sessionReq(t, http.MethodPut, "/v1/admin/mx", csrf, tc.body, cookie))
		if rr.Code != 400 {
			t.Fatalf("%s: got %d %s", tc.name, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "smtp_tls_key") && strings.Contains(rr.Body.String(), "-----") {
			t.Fatalf("%s: error echoed key material: %s", tc.name, rr.Body.String())
		}
	}
}

// TestAdminMXUIFullFormAndTLSKeyNeverEchoed saves the full advanced Included
// form (with a real STARTTLS pair) and then asserts the private key is never
// echoed on the success page or on a validation-error re-render, and that the
// form repopulates the submitted non-secret values after an error.
func TestAdminMXUIFullFormAndTLSKeyNeverEchoed(t *testing.T) {
	svc, h, u, _ := systemAdminFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	certPEM, keyPEM := testTLSPair(t)

	form := url.Values{
		"_csrf": {csrf}, "revision": {"0"}, "mode": {"included"}, "hostname": {"mx.test"},
		"max_message_bytes": {"5242880"}, "max_staging_bytes": {"268435456"},
		"max_recipients": {"100"}, "max_connections": {"256"},
		"require_tls": {"true"}, "verify_spf": {"true"}, "verify_dkim": {"true"}, "verify_dmarc": {"true"},
		"dns_resolver": {"9.9.9.9:53"}, "dns_timeout_seconds": {"10"},
		"read_timeout_seconds": {"60"}, "write_timeout_seconds": {"60"}, "data_timeout_seconds": {"300"},
		"smtp_tls_cert": {certPEM}, "smtp_tls_key": {keyPEM},
	}
	if rr := uiPost(t, h, cookie, "/ui/admin/mx", form.Encode()); rr.Code != http.StatusSeeOther {
		t.Fatalf("full form save: %d %s", rr.Code, rr.Body.String())
	}
	settings, err := svc.GetMXReceiverSettings(ctx)
	if err != nil || settings.Mode != app.MXModeIncluded || !settings.RequireTLSEnabled() || settings.MaxStagingBytes != 268435456 {
		t.Fatalf("saved settings = %+v %v", settings, err)
	}
	if !settings.SMTPTLSKeyConfigured || settings.SMTPTLSCert == "" {
		t.Fatalf("TLS pair not stored: %+v", settings)
	}
	// The success page must never contain the private key PEM.
	page := uiGet(t, h, cookie, "/admin")
	assertNoKeyEcho(t, page.Body.String(), keyPEM, certPEM)

	// A validation error (staging below message) repopulates the non-secret
	// fields and still never echoes the private key.
	bad := url.Values{
		"_csrf": {csrf}, "revision": {strconv.FormatInt(settings.Revision, 10)}, "mode": {"included"},
		"hostname": {"mx-renamed.test"}, "max_message_bytes": {"2097152"}, "max_staging_bytes": {"1048576"},
		"smtp_tls_cert": {certPEM}, "smtp_tls_key": {keyPEM},
	}
	rr := uiPost(t, h, cookie, "/ui/admin/mx", bad.Encode())
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("bad save: %d %s", rr.Code, rr.Body.String())
	}
	// Follow the redirect so the bound flash is consumed and the form renders
	// with the preserved non-secret values.
	loc := rr.Header().Get("Location")
	if loc == "" {
		t.Fatal("bad save did not redirect")
	}
	page = uiGet(t, h, cookie, loc)
	body := page.Body.String()
	if !strings.Contains(body, "max staging size must be at least the max message size") {
		t.Fatalf("validation error not surfaced: %s", body)
	}
	if !strings.Contains(body, `value="mx-renamed.test"`) || !strings.Contains(body, `value="2097152"`) || !strings.Contains(body, `value="1048576"`) {
		t.Fatalf("failed submission values not preserved:\n%s", body)
	}
	assertNoKeyEcho(t, body, keyPEM, certPEM)

	// The private key textarea is rendered empty even though a key is stored.
	if !strings.Contains(body, `name="smtp_tls_key"`) || !strings.Contains(body, "Leave blank to keep the stored private key") {
		t.Fatalf("private key field not rendered with retain hint:\n%s", body)
	}
}

// assertNoKeyEcho fails if the private key leaks into the rendered page. The
// public certificate is intentionally re-rendered into its textarea, so it is
// not checked here.
func assertNoKeyEcho(t *testing.T, body, keyPEM, certPEM string) {
	t.Helper()
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("page rendered a private key marker")
	}
	if strings.Contains(body, strings.TrimSpace(keyPEM)) {
		t.Fatal("page echoed the private STARTTLS key")
	}
	_ = certPEM
}

// jsonString encodes s as a JSON string literal for embedding in a request body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// mxResp mirrors the flat API response for decoding in tests.
type mxResp struct {
	Mode                 string `json:"mode"`
	URL                  string `json:"url"`
	BearerKey            string `json:"bearer_key"`
	KeyConfigured        bool   `json:"key_configured"`
	Hostname             string `json:"hostname"`
	MaxMessageBytes      int64  `json:"max_message_bytes"`
	MaxStagingBytes      int64  `json:"max_staging_bytes"`
	MaxRecipients        int    `json:"max_recipients"`
	MaxConnections       int    `json:"max_connections"`
	RequireTLS           *bool  `json:"require_tls"`
	VerifySPF            *bool  `json:"verify_spf"`
	VerifyDKIM           *bool  `json:"verify_dkim"`
	VerifyDMARC          *bool  `json:"verify_dmarc"`
	DNSResolver          string `json:"dns_resolver"`
	DNSTimeoutSeconds    int    `json:"dns_timeout_seconds"`
	ReadTimeoutSeconds   int    `json:"read_timeout_seconds"`
	WriteTimeoutSeconds  int    `json:"write_timeout_seconds"`
	DataTimeoutSeconds   int    `json:"data_timeout_seconds"`
	SMTPTLSCert          string `json:"smtp_tls_cert"`
	SMTPTLSKey           string `json:"smtp_tls_key"`
	SMTPTLSKeyConfigured bool   `json:"smtp_tls_key_configured"`
	Revision             int64  `json:"revision"`
	Status               struct {
		State             string `json:"state"`
		Configured        bool   `json:"configured"`
		IncludedSupported bool   `json:"included_supported"`
	} `json:"status"`
}
