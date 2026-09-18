package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

func draftPayload(inboxID string, extra map[string]any) string {
	m := map[string]any{"inbox_id": inboxID, "to": []string{"friend@example.net"}, "subject": "Hi", "text": "body"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestAPICreateDraftWithInlineAttachments(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "asst", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	payload := draftPayload(box.ID, map[string]any{
		"attachments": []map[string]any{{"filename": "a.txt", "content_type": "text/plain", "content": b64("hello")}},
	})
	rr := apiDo(h, "POST", "/v1/drafts", asstKey, payload)
	if rr.Code != 201 {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var d model.Draft
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode %v %s", err, rr.Body.String())
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Filename != "a.txt" {
		t.Fatalf("attachments not in draft response: %s", rr.Body.String())
	}
	// A GET returns the attachment metadata too.
	rr = apiDo(h, "GET", "/v1/drafts/"+d.ID, asstKey, "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"filename":"a.txt"`) {
		t.Fatalf("get draft %d %s", rr.Code, rr.Body.String())
	}
	// And the bytes are downloadable.
	rr = apiDo(h, "GET", "/v1/drafts/"+d.ID+"/attachments/"+d.Attachments[0].ID, asstKey, "")
	if rr.Code != 200 || rr.Body.String() != "hello" {
		t.Fatalf("download %d %q", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("disposition %q", cd)
	}
}

func TestAPIPatchDraftIsPartial(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "asst", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	d := createDraft(t, h, asstKey, box.ID)

	// Patch only the subject; to/text must be preserved.
	rr := apiDo(h, "PATCH", "/v1/drafts/"+d.ID, asstKey, `{"subject":"changed"}`)
	if rr.Code != 200 {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}
	var got model.Draft
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subject != "changed" || len(got.To) != 1 || got.To[0] != "friend@example.net" || got.Text != "body" {
		t.Fatalf("partial patch clobbered fields: %+v", got)
	}
}

func TestAPIOneShotRequestSendFromAssistant(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "asst", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Assistant creates, attaches and submits for approval in one request.
	payload := draftPayload(box.ID, map[string]any{
		"action":      "request-send",
		"attachments": []map[string]any{{"filename": "q.pdf", "content_type": "application/pdf", "content": b64("pdf-bytes")}},
	})
	rr := apiDo(h, "POST", "/v1/drafts", asstKey, payload)
	if rr.Code != 201 {
		t.Fatalf("one-shot request-send %d %s", rr.Code, rr.Body.String())
	}
	var d model.Draft
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != model.DraftStatusPendingApproval || len(d.Attachments) != 1 {
		t.Fatalf("not frozen with attachment: %s", rr.Body.String())
	}
	// The frozen draft is approved by the owner and queued.
	rr = apiDo(h, "POST", "/v1/drafts/"+d.ID+"/approve", ownerKey, `{}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"queued":true`) {
		t.Fatalf("approve %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIRequestSendEndpointWithAttachments(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, asstKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "asst", false, map[string]string{box.ID: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	d := createDraft(t, h, asstKey, box.ID)
	payload := `{"subject":"Reviewed","attachments":[{"filename":"r.txt","content_type":"text/plain","content":"` + b64("r") + `"}]}`
	rr := apiDo(h, "POST", "/v1/drafts/"+d.ID+"/request-send", asstKey, payload)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"pending_approval"`) || !strings.Contains(rr.Body.String(), `"filename":"r.txt"`) {
		t.Fatalf("request-send with attachment %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"subject":"Reviewed"`) {
		t.Fatalf("request-send override not applied: %s", rr.Body.String())
	}
}

func TestAPIDraftUnknownAction(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := apiDo(h, "POST", "/v1/drafts", ownerKey, draftPayload(box.ID, map[string]any{"action": "teleport"}))
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "unknown action") {
		t.Fatalf("unknown action %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIOneShotSendFromOwnerWithOverride(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := createDraft(t, h, ownerKey, box.ID)
	payload := `{"subject":"Edited","attachments":[{"filename":"x.txt","content_type":"text/plain","content":"` + b64("x") + `"}]}`
	rr := apiDo(h, "POST", "/v1/drafts/"+d.ID+"/send", ownerKey, payload)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"queued":true`) {
		t.Fatalf("one-shot send %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"provider_message_id"`) {
		t.Fatalf("send response missing provider_message_id: %s", rr.Body.String())
	}
}

func TestAPIDraftSenderAliasAndListPaging(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, ownerKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "owner", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// sender is accepted as an alias for from_address.
	rr := apiDo(h, "POST", "/v1/drafts", ownerKey, `{"inbox_id":"`+box.ID+`","sender":"`+box.Address+`","to":["a@b.c"],"text":"x"}`)
	if rr.Code != 201 {
		t.Fatalf("sender alias %d %s", rr.Code, rr.Body.String())
	}
	// limit caps the list.
	_ = createDraft(t, h, ownerKey, box.ID)
	rr = apiDo(h, "GET", "/v1/drafts?inbox="+box.ID+"&limit=1", ownerKey, "")
	if rr.Code != 200 {
		t.Fatalf("list %d %s", rr.Code, rr.Body.String())
	}
	var list []model.Draft
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("limit not applied: %v %s", err, rr.Body.String())
	}
}

func TestAPIInboxCreateAcceptsLocalpartSpelling(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	_, adminKey, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := apiDo(h, "POST", "/v1/inboxes", adminKey, `{"domain_id":"`+dom.ID+`","localpart":"agentx","display_name":"Agent X"}`)
	if rr.Code != 201 || !strings.Contains(rr.Body.String(), "agentx@") {
		t.Fatalf("localpart alias %d %s", rr.Code, rr.Body.String())
	}
}
