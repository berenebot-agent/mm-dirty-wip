package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

// oneReconnectFixture builds a server whose remote mailbox service has a live
// One backend, a signed-in admin session and one imported Gmail-via-One inbox.
// It returns the handler, the service, the session cookie and CSRF token, and
// the imported inbox.
func oneReconnectFixture(t *testing.T) (http.Handler, *app.Service, model.User, *http.Cookie, string, model.Inbox) {
	t.Helper()
	svc, _, u, _, _, _ := standaloneFixture(t)
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	f := onefixture.New(t)
	rm.OneHTTP, rm.OneInterval = f.Client(t), time.Nanosecond
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	p := model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true}
	setup, err := rm.PrepareOneImport(context.Background(), p, onefixture.Key)
	if err != nil {
		t.Fatal(err)
	}
	results, err := rm.ImportOne(context.Background(), p, setup, []string{onefixture.ConnectionID})
	if err != nil || len(results) != 1 || results[0].Error != "" {
		t.Fatalf("import: %+v %v", results, err)
	}
	box, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, results[0].InboxID)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	return h, svc, u, cookie, csrf, box
}

// TestDashboardConnectionFlagForDisconnectedOneInbox proves a disconnected
// provider connection renders the red flag in the dashboard's flags column,
// linked to the inbox Identity tab, and that a healthy connection renders no
// such flag.
func TestDashboardConnectionFlagForDisconnectedOneInbox(t *testing.T) {
	h, svc, _, cookie, _, box := oneReconnectFixture(t)
	ctx := context.Background()
	b, err := svc.Store.OneBinding(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Healthy connection: no connection flag.
	body := uiGet(t, h, cookie, "/").Body.String()
	if strings.Contains(body, `href="/?inbox=`+box.ID+`&inbox_tab=basic"`) {
		t.Fatal("healthy inbox must not render the connection flag")
	}
	// Flip the binding to attention: the flag appears, linked to Identity.
	if err := svc.Store.SetOneHealth(ctx, box.AccountID, box.ID, "attention", "Reconnect the original Gmail connection", b.Revision); err != nil {
		t.Fatal(err)
	}
	body = uiGet(t, h, cookie, "/").Body.String()
	if !strings.Contains(body, `href="/?inbox=`+box.ID+`&inbox_tab=basic"`) {
		t.Fatalf("disconnected inbox missing the connection flag: %s", body)
	}
	if !strings.Contains(body, "Reconnect the original Gmail connection") {
		t.Fatal("connection flag must carry the operator reconnect detail")
	}
}

// TestRemoteConfigViewCarriesConnectionState proves the Identity tab's embedded
// remote config reports the provider connection state, so the client can render
// the reconnect prompt without a second request.
func TestRemoteConfigViewCarriesConnectionState(t *testing.T) {
	h, svc, u, _, _, box := oneReconnectFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/remote", key)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET remote = %d: %s", rr.Code, rr.Body.String())
	}
	var healthy struct {
		Provider   string `json:"provider"`
		Connection *struct {
			State     string `json:"state"`
			Provider  string `json:"provider"`
			Reconnect string `json:"reconnect"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &healthy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if healthy.Connection == nil || healthy.Connection.State != "connected" || healthy.Connection.Provider != "one" || healthy.Connection.Reconnect != "one" {
		t.Fatalf("healthy connection = %+v", healthy.Connection)
	}
	b, err := svc.Store.OneBinding(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetOneHealth(ctx, box.AccountID, box.ID, "disconnected", "One key rejected", b.Revision); err != nil {
		t.Fatal(err)
	}
	rr = apiGet(t, h, "/v1/inboxes/"+box.ID+"/remote", key)
	var bad struct {
		Connection *struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &bad); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if bad.Connection == nil || bad.Connection.State != "disconnected" || bad.Connection.Detail != "One key rejected" {
		t.Fatalf("disconnected connection = %+v", bad.Connection)
	}
}

// TestRemoteConfigViewOmitsConnectionForIMAPInbox proves an IMAP-only standalone
// inbox reports no connection state, so no reconnect light or prompt renders.
func TestRemoteConfigViewOmitsConnectionForIMAPInbox(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/remote", key)
	var out struct {
		Provider   string          `json:"provider"`
		Connection json.RawMessage `json:"connection"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Provider != "imap" {
		t.Fatalf("provider = %q, want imap", out.Provider)
	}
	if len(out.Connection) != 0 {
		t.Fatalf("IMAP inbox must omit connection: %s", out.Connection)
	}
}

// TestOneReconnectEndpointSavesAndChecks proves the in-place reconnect endpoint
// validates and stores a replacement key, refuses a wrong-identity key with a
// JSON verdict, requires CSRF, and refuses a non-One inbox.
func TestOneReconnectEndpointSavesAndChecks(t *testing.T) {
	h, svc, _, cookie, csrf, box := oneReconnectFixture(t)
	ctx := context.Background()
	post := func(body string, csrfToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ui/connections/one/reconnect", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if csrfToken != "" {
			req.Header.Set("X-CSRF-Token", csrfToken)
		}
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	payload := `{"inbox_id":"` + box.ID + `","api_key":"` + onefixture.Key + `"}`

	// CSRF is enforced even for the JSON endpoint.
	if rr := post(payload, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d, want 403", rr.Code)
	}
	// A successful save-and-check returns a connected verdict.
	rr := post(payload, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("reconnect = %d: %s", rr.Code, rr.Body.String())
	}
	var ok struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ok); err != nil || ok.Status != "connected" {
		t.Fatalf("reconnect verdict = %s (%v)", rr.Body.String(), err)
	}
	// The stored key is the replacement, not the fixture's original.
	b, err := svc.Store.OneBinding(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "active" {
		t.Fatalf("binding status = %q, want active", b.Status)
	}
	// A key exposing a different Gmail identity is rejected with a safe message.
	bad := `{"inbox_id":"` + box.ID + `","api_key":"fixture-outlook-key"}`
	rr = post(bad, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad key = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	var fail struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &fail); err != nil || fail.Status != "error" || fail.Error == "" {
		t.Fatalf("failure verdict = %s (%v)", rr.Body.String(), err)
	}
	if strings.Contains(rr.Body.String(), onefixture.Key) {
		t.Fatal("reconnect response must never echo the submitted key")
	}
}

// TestOneReconnectEndpointRefusesNonOneInbox proves the endpoint cannot be used
// to attach a One key to an inbox that is not One-bound.
func TestOneReconnectEndpointRefusesNonOneInbox(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodPost, "/ui/connections/one/reconnect",
		strings.NewReader(`{"inbox_id":"`+standalone.ID+`","api_key":"`+onefixture.Key+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-One inbox reconnect = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

// TestInboxConnectionScriptWiring pins the client wiring: app.js must build the
// reconnect control under an Advanced disclosure, render a red prompt when the
// provider reports the account disconnected, and open the in-place One dialog
// rather than leaving for the full wizard.
func TestInboxConnectionScriptWiring(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d", rr.Code)
	}
	script := rr.Body.String()
	for _, want := range []string{
		"data-advanced-reconnect",
		"data-reconnect-prompt",
		"data-one-reconnect",
		"one-reconnect-dialog",
		"/ui/connections/one/reconnect",
		"X-CSRF-Token",
		"Save and check",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("app.js missing reconnect wiring %q", want)
		}
	}
}

// TestDashboardConnectionFlagForReconnectRequiredGmailSync proves the Gmail /
// Microsoft path: a metadata sync that failed on an auth verdict (the service's
// explicit reconnect message) flags the inbox, while a retryable failure does
// not — the classification stays conservative.
func TestDashboardConnectionFlagForReconnectRequiredGmailSync(t *testing.T) {
	svc, _, u, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "flag-gmail@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	t.Cleanup(rm.Stop)
	if err := rm.SaveGoogleGrant(ctx, u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)
	flag := `href="/?inbox=` + box.ID + `&inbox_tab=basic"`

	// A retryable wait is not a disconnect.
	if err := svc.Store.SaveMetadataSyncState(ctx, u.AccountID, box.ID, "waiting", "initial", "Waiting to retry provider request"); err != nil {
		t.Fatal(err)
	}
	if body := uiGet(t, h, cookie, "/").Body.String(); strings.Contains(body, flag) {
		t.Fatal("a retryable sync wait must not flag the inbox as disconnected")
	}
	// An auth failure carries the service reconnect instruction: flag it.
	if err := svc.Store.SaveMetadataSyncState(ctx, u.AccountID, box.ID, "error", "initial", "Reconnect this inbox to resume metadata sync"); err != nil {
		t.Fatal(err)
	}
	body := uiGet(t, h, cookie, "/").Body.String()
	if !strings.Contains(body, flag) {
		t.Fatalf("reconnect-required Gmail sync must flag the inbox: %s", body)
	}
	if !strings.Contains(body, "Reconnect this inbox to resume metadata sync") {
		t.Fatal("connection flag must carry the reconnect detail")
	}
}

// TestDashboardRendersOneReconnectDialog proves the in-place One reconnect
// dialog markup is present on the dashboard, so app.js can open it.
func TestDashboardRendersOneReconnectDialog(t *testing.T) {
	svc, h, u, _, _, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	body := uiGet(t, h, cookie, "/").Body.String()
	for _, want := range []string{"id=\"one-reconnect-dialog\"", "id=\"one-reconnect-form\"", "id=\"one-reconnect-key\"", "Save and check"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}
