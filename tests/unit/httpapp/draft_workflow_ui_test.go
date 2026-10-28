package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
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
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	// A plain editable draft must not affect the pending-send count.
	if _, err = svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"other@example.net"}, Subject: "Notes", Text: "wip"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// Dashboard shows the pending-send count only, styled like unread.
	rr := uiGet(t, h, cookie, "/")
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, `title="Drafts awaiting approval to send">1<`) || !strings.Contains(body, `class="pill pending-pill"`) || !strings.Contains(body, ">Unread</th>") || !strings.Contains(body, ">Pending send</th>") || !strings.Contains(body, ">Size</th>") {
		t.Fatalf("dashboard pending-send column %d", rr.Code)
	}
	if strings.Contains(body, ">Mailbox size</th>") {
		t.Fatal("dashboard should use the shorter Size column label")
	}
	// Inbox detail shows the send-request section with a quick Send action.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	body = rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Draft send requests") || !strings.Contains(body, "Awaiting approval") {
		t.Fatalf("inbox send requests %d %s", rr.Code, body)
	}
	if !strings.Contains(body, "/drafts/"+draft.ID+"/approve") {
		t.Fatalf("inbox missing quick approve action")
	}
	// The quick approve action is a green tick, not the mark-read envelope.
	approveAt := strings.Index(body, `class="secondary icon-btn approve" title="Approve and send"`)
	if approveAt < 0 {
		t.Fatalf("inbox approve button is not the green tick")
	}
	btn := body[approveAt : approveAt+strings.Index(body[approveAt:], "</button>")]
	// The mark-read envelope is two paths; the tick is one.
	if paths := strings.Count(btn, "<path"); paths != 1 {
		t.Fatalf("inbox approve button glyph has %d paths, want a single tick: %s", paths, btn)
	}
	// The request card also carries the dash that cancels the approval.
	cancelAt := strings.Index(body, `title="Cancel approval and edit"`)
	if cancelAt < 0 || !strings.Contains(body, "/drafts/"+draft.ID+"/cancel-send-request") {
		t.Fatalf("inbox request card missing the cancel-approval action")
	}
	cancelBtn := body[cancelAt : cancelAt+strings.Index(body[cancelAt:], "</button>")]
	if paths := strings.Count(cancelBtn, "<path"); paths != 1 {
		t.Fatalf("inbox cancel glyph has %d paths, want a single dash: %s", paths, cancelBtn)
	}
	// Send-request row uses inbox-like columns: To, subject + body start,
	// request date and size.
	for _, want := range []string{"friend@example.net", "Proposal", "body", ">Size</span>", ">To</span>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("send-request row missing %q", want)
		}
	}
	// Drafts list flags it pending.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts")
	drafts := rr.Body.String()
	if !strings.Contains(drafts, "Pending Send") || !strings.Contains(drafts, `class="draft-status pending"`) {
		t.Fatalf("drafts list missing pending badge")
	}
	// The drafts list uses the same green tick to approve, and a dash to
	// cancel the approval request and keep the draft.
	if !strings.Contains(drafts, `class="secondary icon-btn approve" title="Approve and send"`) {
		t.Fatalf("drafts list approve action is not the green tick")
	}
	draftsCancelAt := strings.Index(drafts, `title="Cancel approval and edit"`)
	if draftsCancelAt < 0 || !strings.Contains(drafts, "/drafts/"+draft.ID+"/cancel-send-request") {
		t.Fatalf("drafts list missing the cancel-approval action")
	}
	draftsCancelBtn := drafts[draftsCancelAt : draftsCancelAt+strings.Index(drafts[draftsCancelAt:], "</button>")]
	if paths := strings.Count(draftsCancelBtn, "<path"); paths != 1 {
		t.Fatalf("drafts cancel glyph has %d paths, want a single dash: %s", paths, draftsCancelBtn)
	}
	if strings.Contains(rr.Body.String(), "Request send") {
		t.Fatalf("drafts list should not offer request-send to the UI user")
	}
	// Review page shows the exact draft and decision actions.
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/edit")
	body = rr.Body.String()
	for _, want := range []string{"Review draft", "Approve", "Reject", "Cancel approval"} {
		if !strings.Contains(body, want) {
			t.Fatalf("review page missing %q", want)
		}
	}
	// A stale compose form must not bypass approval or change the frozen content.
	form, contentType := multipartBody(t, map[string]string{"action": "send", "to": "attacker@example.net", "subject": "Changed", "text": "pay 10000"}, "", "")
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/save?_csrf="+csrf, form)
	req.Header.Set("Content-Type", contentType)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("pending form send = %d %s", rr.Code, rr.Body.String())
	}
	pending, err := svc.Store.GetSendRequestByDraft(ctx, admin, draft.ID)
	if err != nil || pending.Status != model.SendRequestPending || pending.MessageID != "" {
		t.Fatalf("form send changed approval: %+v, %v", pending, err)
	}
	outbox, err := svc.Store.ListOutbox(ctx, admin, box.ID, 10)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("form send queued mail: %+v, %v", outbox, err)
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
	sent, err := svc.Store.GetMessageByID(ctx, u.AccountID, sr.MessageID)
	if err != nil || sent.Text != draft.Text || sent.Subject != draft.Subject || len(sent.To) != 1 || sent.To[0] != draft.To[0] {
		t.Fatalf("approval used changed content: %+v, %v", sent, err)
	}
}

func TestUIDraftRejectAndCancel(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	cookie, csrf := uiSession(t, svc, u.ID)

	// Reject with feedback.
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "One", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft.ID, false); err != nil {
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
	if _, err = svc.RequestSend(ctx, admin, draft2.ID, false); err != nil {
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

// TestUIPendingRequestShowsRejectedReply proves a consumed reply that failed
// the sender check is visible on the pending request and review page, not only
// in the domain receiving log.
func TestUIPendingRequestShowsRejectedReply(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(ctx, admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(ctx, admin, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, admin, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.RecordControlMessage(ctx, store.ControlMessageRecord{
		AccountID: u.AccountID, InboxID: box.ID, Provider: "mailgun", ProviderDeliveryID: "ctl-rejected-1",
		FromAddress: "someone@outside.test", RequestID: sr.ID, Action: "approve", Outcome: "invalid",
		Reason: "sender does not match approver", Subject: "Proposal", Source: "Mailgun",
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "approval reply rejected: sender does not match approver") {
		t.Fatalf("inbox send-request card missing rejected reply %d", rr.Code)
	}
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+draft.ID+"/edit")
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "Reply rejected") || !strings.Contains(body, "sender does not match approver") {
		t.Fatalf("review page missing rejected reply %d", rr.Code)
	}
}
