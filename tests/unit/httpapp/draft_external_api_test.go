package httpapp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestAPIInboxApproverAndExternalRequest(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "hermes", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "Ben", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, readKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}

	// External request without an approver is rejected.
	draft := createDraft(t, h, asstKey, box.ID)
	if rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{"external":true}`); rr.Code != 400 {
		t.Fatalf("external without approver = %d %s", rr.Code, rr.Body.String())
	}

	// A read-only key may not set the inbox approver.
	if rr := apiDo(h, "PATCH", "/v1/inboxes/"+box.ID, readKey, `{"approver_email":"approver@outside.test"}`); rr.Code != 403 {
		t.Fatalf("read set approver = %d %s", rr.Code, rr.Body.String())
	}
	// Owner/Admin sets the approver.
	rr := apiDo(h, "PATCH", "/v1/inboxes/"+box.ID, ownerKey, `{"approver_email":"Approver@Outside.test"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"approver_email":"approver@outside.test"`) {
		t.Fatalf("set approver = %d %s", rr.Code, rr.Body.String())
	}

	// Assistant requests via the API with no external flag: the configured
	// approver makes it external automatically.
	rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"pending_approval"`) || !strings.Contains(rr.Body.String(), `"approver_email":"approver@outside.test"`) {
		t.Fatalf("external request-send = %d %s", rr.Code, rr.Body.String())
	}
	// The approval email is queued and the request records an expiry.
	var sr model.DraftSendRequest
	get := apiDo(h, "GET", "/v1/drafts/"+draft.ID+"/send-request", asstKey, "")
	if get.Code != 200 {
		t.Fatalf("get send-request = %d %s", get.Code, get.Body.String())
	}
	if err := json.Unmarshal(get.Body.Bytes(), &sr); err != nil || sr.ApproverEmail != "approver@outside.test" || sr.TokenExpiresAt == nil {
		t.Fatalf("send-request %+v err=%v", sr, err)
	}
	rr = apiDo(h, "GET", "/v1/outbox?inbox="+box.ID, ownerKey, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Approval required") {
		t.Fatalf("approval email not queued = %d %s", rr.Code, rr.Body.String())
	}

	// Changing the approver is allowed while a request is pending: the request
	// keeps the approver it was created with.
	if rr = apiDo(h, "PATCH", "/v1/inboxes/"+box.ID, ownerKey, `{"approver_email":"other@outside.test"}`); rr.Code != 200 {
		t.Fatalf("approver change while pending = %d %s", rr.Code, rr.Body.String())
	}
	get = apiDo(h, "GET", "/v1/drafts/"+draft.ID+"/send-request", asstKey, "")
	var again model.DraftSendRequest
	if err := json.Unmarshal(get.Body.Bytes(), &again); err != nil || again.ApproverEmail != "approver@outside.test" {
		t.Fatalf("outstanding request lost its approver: %+v err=%v", again, err)
	}
}
