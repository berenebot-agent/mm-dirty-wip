package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

func TestAPIDraftSendAndOutbox(t *testing.T) {
	t.Parallel()
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<draft-http>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Create a draft via the API.
	req := httptest.NewRequest("POST", "/v1/drafts", strings.NewReader(`{"inbox_id":"`+box.ID+`","to":["friend@example.net"],"subject":"Draft","text":"body"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("create draft %d %s", rr.Code, rr.Body.String())
	}
	var d model.Draft
	if err = json.Unmarshal(rr.Body.Bytes(), &d); err != nil || d.ID == "" {
		t.Fatalf("draft %v %s", err, rr.Body.String())
	}
	// Send the draft.
	req = httptest.NewRequest("POST", "/v1/drafts/"+d.ID+"/send", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("send draft %d %s", rr.Code, rr.Body.String())
	}
	// Draft is deleted.
	req = httptest.NewRequest("GET", "/v1/drafts/"+d.ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("draft after send %d %s", rr.Code, rr.Body.String())
	}
	// Outbox lists the pending message.
	req = httptest.NewRequest("GET", "/v1/outbox", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatalf("outbox %d %s", rr.Code, rr.Body.String())
	}
	// Deliver, then outbox is empty.
	var outbox []model.Message
	_ = json.Unmarshal(rr.Body.Bytes(), &outbox)
	if len(outbox) != 1 {
		t.Fatalf("outbox len %d", len(outbox))
	}
	if err = svc.Deliver(ctx, u.AccountID, outbox[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestAPISendWaitTrue(t *testing.T) {
	t.Parallel()
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<wait-out>"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Start a worker so the wait path can observe delivery.
	w := app.NewOutboxWorker(svc, nil)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	defer w.Stop()
	req := httptest.NewRequest("POST", "/v1/send?wait=true", strings.NewReader(`{"inbox_id":"`+box.ID+`","to":["friend@example.net"],"subject":"Wait","text":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("wait send %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"sent"`) || !strings.Contains(rr.Body.String(), `"provider_message_id":"\u003cwait-out\u003e"`) {
		t.Fatalf("wait send body %s", rr.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestBrevoAttachmentRejectionVisibleInSendStatusAndLogs(t *testing.T) {
	t.Parallel()
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":"invalid_parameter","message":"Unsupported file format: md report.md api-key=xkeysib-secret private-body"}`)
	}))
	defer api.Close()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, dom.ID, "brevo", map[string]any{"api_key": "xkeysib-secret", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	worker := app.NewOutboxWorker(svc, slog.New(slog.NewTextHandler(&logs, nil)))
	worker.SetPeriod(10 * time.Millisecond)
	worker.Start()
	req := httptest.NewRequest("POST", "/v1/send?wait=true", strings.NewReader(`{"inbox_id":"`+box.ID+`","to":["friend@example.net"],"subject":"Report","text":"See attached","attachments":[{"filename":"report.md","content_type":"text/markdown","content":"IyBSZXBvcnQ="}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	func() {
		defer worker.Stop() // Join the logger's writers before inspecting the buffer.
		h.ServeHTTP(rr, req)
	}()
	if rr.Code != http.StatusOK {
		t.Fatalf("send status %d: %s", rr.Code, rr.Body.String())
	}
	var result struct{ Message model.Message }
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	const diagnostic = "brevo returned HTTP 400: provider rejected an attachment file extension; check Brevo's supported attachment types"
	if result.Message.Status != "failed" || result.Message.LastError != diagnostic || result.Message.Attempts != 1 || result.Message.NextRetry != "" {
		t.Fatalf("send status: %+v", result.Message)
	}
	attempts, err := svc.Store.ListDomainDeliveryAttempts(ctx, u.AccountID, dom.ID, 10, 0)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("delivery attempts: %+v, err %v", attempts, err)
	}
	if attempts[0].Status != "failed" || attempts[0].ErrorText != diagnostic {
		t.Fatalf("failed attempt: %+v", attempts[0])
	}
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/ui/domains/"+dom.ID+"/sending/deliveries")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "provider rejected an attachment file extension") {
		t.Fatalf("domain log %d: %s", page.Code, page.Body.String())
	}
	if !strings.Contains(logs.String(), "attachment file extension") {
		t.Fatalf("missing rejection in worker logs: %s", logs.String())
	}
	for _, output := range []string{rr.Body.String(), attempts[0].ErrorText, page.Body.String(), logs.String()} {
		for _, secret := range []string{"xkeysib-secret", "private-body", "report.md"} {
			if strings.Contains(output, secret) {
				t.Fatalf("provider detail %q leaked", secret)
			}
		}
	}
}
