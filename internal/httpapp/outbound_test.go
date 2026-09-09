package httpapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"gatehouse-mail/internal/transport"
)

func TestAPISendWithBase64Attachment(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<brevo-http>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetActiveOutboundCredential(ctx, u.AccountID, cred.ID); err != nil {
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
	if err = svc.Deliver(ctx, u.AccountID, resp.Message.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestOutboundConfigFromForm(t *testing.T) {
	brevo, ok := transport.LookupOutbound("brevo")
	if !ok {
		t.Fatal("brevo not registered")
	}
	form := func(values url.Values) *http.Request {
		r := httptest.NewRequest("POST", "/ui/outbound", nil)
		r.Form = values
		return r
	}
	if _, err := outboundConfigFromForm(brevo, form(url.Values{}), true); err == nil {
		t.Fatal("missing api_key should error on create")
	}
	cfg, err := outboundConfigFromForm(brevo, form(url.Values{"cfg_brevo_api_key": {"k"}, "cfg_brevo_api_base": {"https://api.brevo.com"}}), true)
	if err != nil || cfg["api_key"] != "k" || cfg["api_base"] != "https://api.brevo.com" {
		t.Fatalf("cfg %#v err %v", cfg, err)
	}
	cfg, err = outboundConfigFromForm(brevo, form(url.Values{"cfg_brevo_api_base": {"https://api.brevo.com"}}), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := cfg["api_key"]; present {
		t.Fatalf("blank secret should be omitted on edit: %#v", cfg)
	}
	smtpProvider, ok := transport.LookupOutbound("smtp")
	if !ok {
		t.Fatal("smtp not registered")
	}
	cfg, err = outboundConfigFromForm(smtpProvider, form(url.Values{"cfg_smtp_host": {"smtp.example.com"}, "cfg_smtp_port": {"2525"}, "cfg_smtp_security": {"tls"}}), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["port"] != 2525 || cfg["security"] != "tls" || cfg["host"] != "smtp.example.com" {
		t.Fatalf("smtp cfg %#v", cfg)
	}
	if _, err = outboundConfigFromForm(smtpProvider, form(url.Values{"cfg_smtp_host": {"smtp.example.com"}, "cfg_smtp_port": {"nope"}}), true); err == nil {
		t.Fatal("bad port should error")
	}
}

func TestDashboardRendersOutboundProviderFields(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	srv := New(svc, nil)
	data := pageData{
		CSRF: "token",
		Outbound: []outboundView{
			{ID: "out_1", Name: "Primary", Provider: "brevo", Active: true, ConfigJSON: `{"api_base":"https://api.brevo.com"}`},
			{ID: "out_2", Name: "Backup", Provider: "smtp"},
		},
		OutboundProviders: outboundProviderViews(),
	}
	rr := httptest.NewRecorder()
	srv.render(rr, dashboardBody, data)
	body := rr.Body.String()
	for _, want := range []string{"Add Outbound Provider", `data-provider="brevo"`, `data-provider="smtp"`, "cfg_smtp_host", "Set Active", "data-config=", `src="/assets/app.js?v=`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline event handlers are blocked by CSP and must not be used")
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

func TestAppJSServed(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/assets/app.js", nil))
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("asset %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	for _, want := range []string{"provider-dialog", "key-dialog", "key-result", "isSecureContext", "navigator.clipboard"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("asset missing %q", want)
		}
	}
}
