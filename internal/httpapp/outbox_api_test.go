package httpapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/model"
)

func TestAPIDraftSendAndOutbox(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<draft-http>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
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
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"messageId":"<wait-out>"}`)
	}))
	defer api.Close()
	cred, err := svc.SaveOutboundCredential(ctx, u.AccountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetDomainOutboundCredential(ctx, u.AccountID, dom.ID, cred.ID); err != nil {
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
