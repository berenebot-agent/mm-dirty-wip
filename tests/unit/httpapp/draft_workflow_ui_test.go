package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func uiPost(t *testing.T, h http.Handler, cookie *http.Cookie, path, form string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func uiGet(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestUIDraftApprovalFlow(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft.ID); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// Dashboard shows the drafts count.
	rr := uiGet(t, h, cookie, "/dashboard")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), ">Drafts</th>") {
		t.Fatalf("dashboard drafts column %d", rr.Code)
	}
	// Inbox detail shows the send-request section with a quick Send action.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Draft send requests") || !strings.Contains(body, "Awaiting approval") {
		t.Fatalf("inbox send requests %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "/drafts/"+draft.ID+"/approve") {
		t.Fatalf("inbox missing quick approve action")
	}
	// Drafts list flags it pending.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts")
	if !strings.Contains(rr.Body.String(), "Pending") {
		t.Fatalf("drafts list missing pending badge")
	}
	// Review page shows the exact draft and decision actions.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/edit")
	body = rr.Body.String()
	for _, want := range []string{"Review draft", "Approve", "Reject", "Cancel approval"} {
		if !strings.Contains(body, want) {
			t.Fatalf("review page missing %q", want)
		}
	}
	// Approve via the UI.
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/approve", "_csrf="+csrf)
	if rr.Code != 303 {
		t.Fatalf("ui approve = %d %s", rr.Code, rr.Body.String())
	}
	if _, err = svc.Store.GetDraft(ctx, admin, draft.ID); err == nil {
		t.Fatal("draft not consumed by UI approve")
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, admin, draft.ID)
	if err != nil || sr.Status != model.SendRequestApproved || sr.DecisionMethod != model.DecisionMethodUI {
		t.Fatalf("send request after UI approve %+v err=%v", sr, err)
	}
}

func TestUIDraftRejectAndCancel(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	cookie, csrf := uiSession(t, svc, u.ID)

	// Reject with feedback.
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "One", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft.ID); err != nil {
		t.Fatal(err)
	}
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/reject", "_csrf="+csrf+"&feedback=fix+the+pricing")
	if rr.Code != 303 {
		t.Fatalf("ui reject = %d %s", rr.Code, rr.Body.String())
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, admin, draft.ID)
	if err != nil || sr.Status != model.SendRequestRejected || sr.Feedback != "fix the pricing" {
		t.Fatalf("rejected request %+v err=%v", sr, err)
	}
	// Rejected draft is editable again and shows the feedback.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/edit")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "fix the pricing") {
		t.Fatalf("rejected edit page %d", rr.Code)
	}

	// Cancel an outstanding request returns the draft to editable.
	draft2, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Two", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft2.ID); err != nil {
		t.Fatal(err)
	}
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft2.ID+"/cancel-send-request", "_csrf="+csrf)
	if rr.Code != 303 {
		t.Fatalf("ui cancel = %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetDraft(ctx, admin, draft2.ID)
	if err != nil || got.Status != model.DraftStatusDraft {
		t.Fatalf("draft after cancel %+v err=%v", got, err)
	}
}
