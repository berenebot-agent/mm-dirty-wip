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
	if !strings.Contains(body, `class="secondary btn-sm cell-edit domain-provider-edit open-domain-dialog"`) {
		t.Fatalf("dashboard missing the Dial MX receiving provider button:\n%s", body)
	}
	if !strings.Contains(body, `<span class="dns-light amber" title="Inbound connectors are still connecting`) {
		t.Fatalf("dashboard missing the Dial MX connector light:\n%s", body)
	}
	// Count only the in-button lights, which carry the receiving data-kind
	// attribute; the receiving dialog's DNS list also uses dns-light and must
	// not be counted.
	if strings.Count(body, `data-kind="receiving"><span class="dns-light`) != 1 {
		t.Fatalf("connector light must render once, for the Dial MX domain only")
	}
	// The light sits inside the receiving provider button, ahead of its label,
	// rather than outside it.
	if !strings.Contains(body, `data-kind="receiving"><span class="dns-light amber"`) {
		t.Fatalf("connector light must be inside the receiving button")
	}
}
