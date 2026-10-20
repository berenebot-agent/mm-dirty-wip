package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestAPIUncertainOutboxRetryRequiresAcknowledgement(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, box := httpFixture(t)
	ctx := context.Background()
	_, token, err := svc.Store.CreateAPIKey(ctx, user.AccountID, "uncertain-retry", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: box, Provider: "brevo", RFCMessageID: "<uncertain-api@test>",
		From: model.Address{Address: box.Address}, To: []string{"friend@example.net"},
		Subject: "subject", Text: "text", RawPath: "messages/uncertain.eml", SizeBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, claim, err := svc.Store.ClaimNextPending(ctx, time.Now().UTC(), "sender", time.Hour)
	if err != nil || claim == "" {
		t.Fatalf("claim: %q %v", claim, err)
	}
	if err := svc.Store.BeginMessageSend(ctx, user.AccountID, m.ID, claim); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.MarkMessageUncertain(ctx, user.AccountID, m.ID, "outcome write failed", claim); err != nil {
		t.Fatal(err)
	}
	request := func(url string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, url, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	r := request("/v1/outbox/" + m.ID + "/retry")
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "delivery uncertain") {
		t.Fatalf("unconfirmed retry: %d %s", r.Code, r.Body.String())
	}
	got, err := svc.Store.GetMessageByID(ctx, user.AccountID, m.ID)
	if err != nil || got.Status != "failed" {
		t.Fatalf("unconfirmed retry changed message: %+v %v", got, err)
	}
	r = request("/v1/outbox/" + m.ID + "/retry?confirm_duplicate_risk=true")
	if r.Code != http.StatusNoContent {
		t.Fatalf("confirmed retry: %d %s", r.Code, r.Body.String())
	}
	got, err = svc.Store.GetMessageByID(ctx, user.AccountID, m.ID)
	if err != nil || got.Status != "pending" {
		t.Fatalf("confirmed retry did not queue: %+v %v", got, err)
	}
}
