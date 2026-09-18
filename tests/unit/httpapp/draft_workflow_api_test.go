package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

func apiDo(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func createDraft(t *testing.T, h http.Handler, key, boxID string) model.Draft {
	t.Helper()
	rr := apiDo(h, "POST", "/v1/drafts", key, `{"inbox_id":"`+boxID+`","to":["friend@example.net"],"subject":"Draft","text":"body"}`)
	if rr.Code != 201 {
		t.Fatalf("create draft %d %s", rr.Code, rr.Body.String())
	}
	var d model.Draft
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || d.ID == "" {
		t.Fatalf("draft %v %s", err, rr.Body.String())
	}
	return d
}

func TestAPIDraftApprovalWorkflow(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
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

	draft := createDraft(t, h, asstKey, box.ID)

	// Read cannot request.
	if rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", readKey, `{}`); rr.Code != 403 {
		t.Fatalf("read request-send = %d %s", rr.Code, rr.Body.String())
	}
	// Assistant requests.
	rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"pending_approval"`) {
		t.Fatalf("request-send %d %s", rr.Code, rr.Body.String())
	}
	// Frozen edit is a conflict.
	if rr = apiDo(h, "PATCH", "/v1/drafts/"+draft.ID, asstKey, `{"subject":"changed"}`); rr.Code != 409 {
		t.Fatalf("frozen edit = %d %s", rr.Code, rr.Body.String())
	}
	// Assistant cannot approve.
	if rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/approve", asstKey, `{}`); rr.Code != 403 {
		t.Fatalf("assistant approve = %d %s", rr.Code, rr.Body.String())
	}
	// Owner rejects with feedback.
	rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/reject", ownerKey, `{"feedback":"fix pricing"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"rejected"`) || !strings.Contains(rr.Body.String(), `"feedback":"fix pricing"`) {
		t.Fatalf("reject %d %s", rr.Code, rr.Body.String())
	}
	// Editing the rejected draft returns it to draft, then resubmit and approve.
	if rr = apiDo(h, "PATCH", "/v1/drafts/"+draft.ID, asstKey, `{"inbox_id":"`+box.ID+`","to":["friend@example.net"],"subject":"Revised","text":"body2"}`); rr.Code != 200 {
		t.Fatalf("revise rejected = %d %s", rr.Code, rr.Body.String())
	}
	if rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{}`); rr.Code != 200 {
		t.Fatalf("resubmit = %d %s", rr.Code, rr.Body.String())
	}
	rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/approve", ownerKey, `{"feedback":"ok"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"queued":true`) {
		t.Fatalf("approve %d %s", rr.Code, rr.Body.String())
	}
	// Draft consumed; request survives and is approved.
	if rr = apiDo(h, "GET", "/v1/drafts/"+draft.ID, ownerKey, ""); rr.Code != 404 {
		t.Fatalf("draft after approve = %d %s", rr.Code, rr.Body.String())
	}
	rr = apiDo(h, "GET", "/v1/drafts/"+draft.ID+"/send-request", asstKey, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"approved"`) || !strings.Contains(rr.Body.String(), `"decision_method":"api"`) {
		t.Fatalf("send-request %d %s", rr.Code, rr.Body.String())
	}
	// The list endpoint reports it too.
	if rr = apiDo(h, "GET", "/v1/send-requests?inbox="+box.ID, asstKey, ""); rr.Code != 200 {
		t.Fatalf("send-requests %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIDraftAttachmentsAndFreeze(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "hermes", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	draft := createDraft(t, h, asstKey, box.ID)

	// Upload an attachment.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("attachments", "note.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/v1/drafts/"+draft.ID+"/attachments", &buf)
	req.Header.Set("Authorization", "Bearer "+asstKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 || !strings.Contains(rr.Body.String(), `"filename":"note.txt"`) {
		t.Fatalf("upload %d %s", rr.Code, rr.Body.String())
	}
	var atts []model.DraftAttachment
	if err := json.Unmarshal(rr.Body.Bytes(), &atts); err != nil || len(atts) != 1 {
		t.Fatalf("attachments %v %s", err, rr.Body.String())
	}
	// List.
	if rr = apiDo(h, "GET", "/v1/drafts/"+draft.ID+"/attachments", asstKey, ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"filename":"note.txt"`) {
		t.Fatalf("list attachments %d %s", rr.Code, rr.Body.String())
	}
	// Request send freezes attachment changes.
	if rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{}`); rr.Code != 200 {
		t.Fatalf("request-send %d %s", rr.Code, rr.Body.String())
	}
	if rr = apiDo(h, "DELETE", "/v1/drafts/"+draft.ID+"/attachments/"+atts[0].ID, asstKey, ""); rr.Code != 409 {
		t.Fatalf("delete frozen attachment = %d %s", rr.Code, rr.Body.String())
	}
	// Cancel then delete.
	if rr = apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/cancel-send-request", asstKey, ""); rr.Code != 200 {
		t.Fatalf("cancel %d %s", rr.Code, rr.Body.String())
	}
	if rr = apiDo(h, "DELETE", "/v1/drafts/"+draft.ID+"/attachments/"+atts[0].ID, asstKey, ""); rr.Code != 204 {
		t.Fatalf("delete attachment = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIDirectSendResolvesRequest(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "hermes", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "Ben", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := createDraft(t, h, asstKey, box.ID)
	if rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/request-send", asstKey, `{}`); rr.Code != 200 {
		t.Fatalf("request-send %d %s", rr.Code, rr.Body.String())
	}
	// A direct owner send authorizes the outstanding request.
	if rr := apiDo(h, "POST", "/v1/drafts/"+draft.ID+"/send", ownerKey, ""); rr.Code != 200 {
		t.Fatalf("direct send %d %s", rr.Code, rr.Body.String())
	}
	rr := apiDo(h, "GET", "/v1/drafts/"+draft.ID+"/send-request", asstKey, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"approved"`) {
		t.Fatalf("send-request after direct send %d %s", rr.Code, rr.Body.String())
	}
}
