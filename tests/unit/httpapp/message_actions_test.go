package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestMessageActionsAsyncDefaultAndPolling(t *testing.T) {
	t.Parallel()
	svc, _, u, _, box := httpFixture(t)
	h := httpapp.New(svc, nil).Handler()
	m := seedInbound(t, svc, box, "action-api", "<action-api@test>", "Action", "body")
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/v1/messages/"+m.ID, strings.NewReader(`{"read":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 202 {
		t.Fatalf("async submission: %d %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Action store.MessageAction `json:"action"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	a, err := svc.WaitForInboxAction(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, result.Action.ID, 3*time.Second)
	if err != nil || a.State != "done" {
		t.Fatalf("execution: %+v %v", a, err)
	}
	req = httptest.NewRequest("GET", rr.Header().Get("Location"), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"state":"done"`) {
		t.Fatalf("poll: %d %s", rr.Code, rr.Body.String())
	}
	updated, err := svc.Store.GetMessage(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, m.ID)
	if err != nil || !updated.Read {
		t.Fatal("domain action was not applied")
	}
}

// The queue lists the account's whole mailbox set, so each row names the
// mailbox it came from.
func TestActionQueueNamesEachActionInbox(t *testing.T) {
	t.Parallel()
	svc, _, u, _, box := httpFixture(t)
	h := httpapp.New(svc, nil).Handler()
	m := seedInbound(t, svc, box, "queue-name", "<queue-name@test>", "Queue", "body")
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/v1/messages/"+m.ID, strings.NewReader(`{"read":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 202 {
		t.Fatalf("async submission: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("GET", "/v1/actions", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("queue list: %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Items []struct {
			ID        string `json:"id"`
			MessageID string `json:"message_id"`
			InboxName string `json:"inbox_name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range payload.Items {
		if item.MessageID != m.ID {
			continue
		}
		found = true
		if item.InboxName != "Hermes" {
			t.Fatalf("queue row names the mailbox %q, want %q", item.InboxName, "Hermes")
		}
	}
	if !found {
		t.Fatalf("the queued action is missing from the queue: %s", rr.Body.String())
	}
	// The single-action read names the mailbox the same way.
	req = httptest.NewRequest("GET", "/v1/actions/"+payload.Items[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("queue read: %d %s", rr.Code, rr.Body.String())
	}
	var single struct {
		Action struct {
			InboxName string `json:"inbox_name"`
		} `json:"action"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if single.Action.InboxName != "Hermes" {
		t.Fatalf("queue read names the mailbox %q, want %q", single.Action.InboxName, "Hermes")
	}
}
