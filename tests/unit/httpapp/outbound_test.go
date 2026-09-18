package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

func TestAPISendWithBase64Attachment(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<brevo-http>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"inbox_id": box.ID,
		"to":       []string{"friend@example.net"},
		"subject":  "Report",
		"text":     "See attached",
		"attachments": []map[string]any{{
			"filename":     "report.txt",
			"content_type": "text/plain",
			"content":      base64.StdEncoding.EncodeToString([]byte("http attachment")),
		}},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("send %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Message.ID == "" {
		t.Fatalf("no message id in %s", rr.Body.String())
	}
	// Deliver the queued message via the worker path.
	if err = svc.Deliver(ctx, u.AccountID, resp.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestCSPAllowsSelfScripts(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("csp must allow same-origin scripts: %q", csp)
	}
}

func TestAPIDeliveryLogAdminOnlyAndScoped(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<log-http>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Log", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Admin key can read the log by domain.
	req := httptest.NewRequest("GET", "/v1/admin/domains/"+dom.ID+"/sending/deliveries", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"sent"`) || !strings.Contains(rr.Body.String(), res.Message.ID) {
		t.Fatalf("deliveries %d %s", rr.Code, rr.Body.String())
	}
	// Unknown domain -> 404.
	req = httptest.NewRequest("GET", "/v1/admin/domains/dom_missing/sending/deliveries", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("foreign domain %d", rr.Code)
	}
}

func TestUIDomainDeliveriesShowsActivity(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<ui-log>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Log", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/ui/domains/"+dom.ID+"/sending/deliveries", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("domain deliveries %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{res.Message.ID, "/ui/messages/" + res.Message.ID} {
		if !strings.Contains(body, want) {
			t.Fatalf("domain deliveries missing %q", want)
		}
	}
}

func TestAppJSServed(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/assets/app.js", nil))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("asset %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	for _, want := range []string{"key-dialog", "key-result", "isSecureContext", "navigator.clipboard"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("asset missing %q", want)
		}
	}
}
