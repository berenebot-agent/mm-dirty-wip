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
