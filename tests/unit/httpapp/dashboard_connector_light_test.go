package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestDashboardConnectorLightRendersForDialMXOnly proves the dashboard's
// Domains table draws the aggregate red/green connector light for a Dial MX
// domain and leaves a non-Dial MX domain's Receiving cell untouched. With no
// live manager the configured Antler receiver is only "connecting", so the
// light is red — the honest state when no receiver can be confirmed ready.
func TestDashboardConnectorLightRendersForDialMXOnly(t *testing.T) {
	svc, h, u, mailgunDomain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	dialDomain, err := svc.Store.CreateDomain(ctx, u.AccountID, "dial.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, dialDomain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}

	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rr.Code)
	}
	body := rr.Body.String()
	// The fixture domain is a non-Dial MX provider and must never carry the light.
	_ = mailgunDomain
	if !strings.Contains(body, `<span class="domain-receiving"><span class="dns-light danger"`) {
		t.Fatalf("dashboard missing the Dial MX connector light:\n%s", body)
	}
	if strings.Count(body, `<span class="dns-light danger"`) != 1 {
		t.Fatalf("connector light must render once, for the Dial MX domain only")
	}
	if !strings.Contains(body, `<span class="domain-receiving"><button`) {
		t.Fatalf("non-Dial MX receiving cell must not carry a light")
	}
}
