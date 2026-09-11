package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/store"
)

// TestApprovalControlNotShownAsBlocked locks in that a consumed approval
// control message is rendered as an Approval row in both the dashboard Recent
// messages and the domain log, and never as Blocked.
func TestApprovalControlNotShownAsBlocked(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.Store.RecordControlMessage(ctx, store.ControlMessageRecord{
		AccountID:          u.AccountID,
		InboxID:            box.ID,
		Provider:           "mailgun",
		ProviderDeliveryID: "ctl-1",
		EnvelopeRecipient:  box.Address,
		FromName:           "Ben",
		FromAddress:        "approver@outside.test",
		RequestID:          "dsr_test",
		Action:             "approve",
		Outcome:            "approved",
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}

	dashboard := get("/")
	if !strings.Contains(dashboard, `<span class="pill amber">Approval</span>`) {
		t.Fatalf("dashboard missing Approval pill: %s", dashboard)
	}
	if strings.Contains(dashboard, `<span class="pill amber">Blocked</span>`) {
		t.Fatalf("dashboard mislabelled approval control mail as Blocked: %s", dashboard)
	}
	if !strings.Contains(dashboard, "approver@outside.test") {
		t.Fatalf("dashboard missing the consumed control message")
	}

	log := get("/ui/domains/" + dom.ID + "/sending/deliveries")
	if !strings.Contains(log, `<span class="pill amber">Approval</span>`) {
		t.Fatalf("domain log missing Approval pill: %s", log)
	}
	if strings.Contains(log, `<span class="pill amber">Blocked</span>`) {
		t.Fatalf("domain log mislabelled approval control mail as Blocked: %s", log)
	}
}
