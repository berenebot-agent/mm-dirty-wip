package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// commitDashboardOutbound queues one outbound message, the way a send does.
func commitDashboardOutbound(t *testing.T, svc *app.Service, box model.Inbox, subject string) model.Message {
	t.Helper()
	m, _, err := svc.Store.CommitOutbound(context.Background(), store.OutboundRecord{
		Inbox: box, Provider: "brevo", RFCMessageID: "<out@dashboard.test>",
		From: model.Address{Address: box.Address}, To: []string{"someone@outside.test"},
		Subject: subject, Text: "body", RawPath: "messages/dashboard-dedupe.eml", SizeBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// dashboardBody returns the account dashboard as the admin sees it.
func dashboardBody(t *testing.T, h http.Handler, svc *app.Service, userID string) string {
	t.Helper()
	cookie, _ := uiSession(t, svc, userID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status=%d body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestDashboardCollapsesOutboundDeliveryRows proves the dashboard's Recent
// messages shows one row per outbound message, carrying that message's latest
// delivery status, instead of one row per delivery-log state transition. The
// delivery log records the in-flight marker and the outcome separately, so one
// send contributes two rows to the log.
func TestDashboardCollapsesOutboundDeliveryRows(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := commitDashboardOutbound(t, svc, box, "Dashboard dedupe subject")
	if err := svc.Store.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MarkSent(ctx, u.AccountID, m.ID, "<provider-1>", "brevo", ""); err != nil {
		t.Fatal(err)
	}
	body := dashboardBody(t, h, svc, u.ID)
	if n := strings.Count(body, "Dashboard dedupe subject"); n != 1 {
		t.Fatalf("dashboard rendered the outbound message %d times, want 1: %s", n, body)
	}
	if !strings.Contains(body, `<span class="pill">Sent</span>`) {
		t.Fatalf("dashboard missing the Sent pill: %s", body)
	}
	if n := strings.Count(body, "/ui/messages/"+m.ID); n != 1 {
		t.Fatalf("dashboard linked the outbound message %d times, want 1: %s", n, body)
	}
}

// TestDashboardShowsLatestFailedDelivery proves a failed delivery is shown as
// one Failed row: the failure is not labelled Sent, and the failed attempt
// does not also appear as a second row.
func TestDashboardShowsLatestFailedDelivery(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := commitDashboardOutbound(t, svc, box, "Dashboard failure subject")
	if err := svc.Store.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MarkFailed(ctx, u.AccountID, m.ID, "provider refused", time.Time{}, 1, "brevo", ""); err != nil {
		t.Fatal(err)
	}
	body := dashboardBody(t, h, svc, u.ID)
	if n := strings.Count(body, "/ui/messages/"+m.ID); n != 1 {
		t.Fatalf("dashboard linked the failed message %d times, want 1: %s", n, body)
	}
	if !strings.Contains(body, `<span class="pill danger">Failed</span>`) {
		t.Fatalf("dashboard missing the Failed pill: %s", body)
	}
}

// TestDashboardShowsOutboundAttemptCount proves the collapsed row says how
// many attempts the delivery took, so a retried message is distinguishable
// from one that went out first time.
func TestDashboardShowsOutboundAttemptCount(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := commitDashboardOutbound(t, svc, box, "Dashboard retry subject")
	if err := svc.Store.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	// A first attempt that fails but is retried, then a second attempt that
	// succeeds: three log rows for one message.
	if _, _, err := svc.Store.MarkFailed(ctx, u.AccountID, m.ID, "provider timeout", time.Now().UTC().Add(time.Minute), 2, "brevo", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.RecordDeliveryStarted(ctx, u.AccountID, m.ID, "brevo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MarkSent(ctx, u.AccountID, m.ID, "<provider-1>", "brevo", ""); err != nil {
		t.Fatal(err)
	}
	body := dashboardBody(t, h, svc, u.ID)
	if n := strings.Count(body, "Dashboard retry subject"); n != 1 {
		t.Fatalf("dashboard rendered the outbound message %d times, want 1: %s", n, body)
	}
	if !strings.Contains(body, "attempt 2") {
		t.Fatalf("dashboard missing the attempt count: %s", body)
	}
}
