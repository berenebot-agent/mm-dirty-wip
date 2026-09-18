package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/store"
)

// TestApprovalControlShowsReceivedWithControlClient locks in that a consumed
// approval control message is rendered as a normal Received row in both the
// dashboard Recent messages and the domain log, distinguished from genuine
// mail by the grey Control pill in the Client column, and never as Blocked.
func TestApprovalControlShowsReceivedWithControlClient(t *testing.T) {
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
	if !strings.Contains(dashboard, `<span class="pill">Received</span>`) {
		t.Fatalf("dashboard missing Received direction for approval control mail: %s", dashboard)
	}
	if !strings.Contains(dashboard, `<span class="pill">Control</span>`) {
		t.Fatalf("dashboard missing Control client pill for approval control mail: %s", dashboard)
	}
	if strings.Contains(dashboard, `<span class="pill amber">Blocked</span>`) {
		t.Fatalf("dashboard mislabelled approval control mail as Blocked: %s", dashboard)
	}
	if strings.Contains(dashboard, `<span class="pill amber">Approval</span>`) {
		t.Fatalf("dashboard shows approval control mail as Approval direction instead of Received: %s", dashboard)
	}
	if !strings.Contains(dashboard, "approver@outside.test") {
		t.Fatalf("dashboard missing the consumed control message")
	}

	log := get("/ui/domains/" + dom.ID + "/sending/deliveries")
	if !strings.Contains(log, `<span class="pill">Received</span>`) {
		t.Fatalf("domain log missing Received direction for approval control mail: %s", log)
	}
	if !strings.Contains(log, `<span class="pill">Control</span>`) {
		t.Fatalf("domain log missing Control client pill for approval control mail: %s", log)
	}
	if strings.Contains(log, `<span class="pill amber">Blocked</span>`) {
		t.Fatalf("domain log mislabelled approval control mail as Blocked: %s", log)
	}
	if strings.Contains(log, `<span class="pill amber">Approval</span>`) {
		t.Fatalf("domain log shows approval control mail as Approval direction instead of Received: %s", log)
	}
}

// TestApprovalControlSubjectLabelReflectsOutcome locks in that a consumed
// approval control message shows its reviewed draft subject prefixed
// "Approval:" for an approval and "Rejected:" for a rejection, in both the
// dashboard Recent messages and the domain log.
func TestApprovalControlSubjectLabelReflectsOutcome(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	for _, rec := range []store.ControlMessageRecord{
		{
			AccountID:          u.AccountID,
			InboxID:            box.ID,
			Provider:           "mailgun",
			ProviderDeliveryID: "ctl-approved",
			EnvelopeRecipient:  box.Address,
			FromName:           "Ben",
			FromAddress:        "approver@outside.test",
			RequestID:          "dsr_approved",
			Action:             "approve",
			Outcome:            "approved",
			Subject:            "Revised proposal",
		},
		{
			AccountID:          u.AccountID,
			InboxID:            box.ID,
			Provider:           "mailgun",
			ProviderDeliveryID: "ctl-rejected",
			EnvelopeRecipient:  box.Address,
			FromName:           "Ben",
			FromAddress:        "approver@outside.test",
			RequestID:          "dsr_rejected",
			Action:             "reject",
			Outcome:            "rejected",
			Subject:            "Revised proposal",
		},
	} {
		if _, err := svc.Store.RecordControlMessage(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	cookie, _ := uiSession(t, svc, u.ID)

	for _, path := range []string{"/", "/ui/domains/" + dom.ID + "/sending/deliveries"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Approval: Revised proposal") {
			t.Fatalf("%s missing approved subject label: %s", path, body)
		}
		if !strings.Contains(body, "Rejected: Revised proposal") {
			t.Fatalf("%s missing rejected subject label: %s", path, body)
		}
	}
}
