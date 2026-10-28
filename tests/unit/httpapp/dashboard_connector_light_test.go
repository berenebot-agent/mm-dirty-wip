package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// TestDashboardConnectorLightRendersForDialMXOnly proves the dashboard's
// Domains table draws the aggregate red/green connector light for a Dial MX
// domain and leaves a non-Dial MX domain's Receiving cell untouched. With no
// live manager the configured Tusk receiver is only "connecting", and the
// light is red — the honest state when no receiver can be confirmed ready and
// routed. (The red/green MX-gating logic itself is pinned by
// TestDialMXHealthAggregatesReceiverStatus.)
func TestDashboardConnectorLightRendersForDialMXOnly(t *testing.T) {
	t.Parallel()
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
	if !strings.Contains(body, `<span class="dns-light danger" data-receiving-light title="No inbound connector is ready`) {
		t.Fatalf("dashboard missing the Dial MX connector light:\n%s", body)
	}
	// Count only the in-button lights, which carry the receiving data-kind
	// attribute; the receiving dialog's DNS list also uses dns-light and must
	// not be counted.
	if strings.Count(body, `data-kind="receiving" data-receiving-button><span class="dns-light`) != 1 {
		t.Fatalf("connector light must render once, for the Dial MX domain only")
	}
	// The light sits inside the receiving provider button, ahead of its label,
	// rather than outside it.
	if !strings.Contains(body, `data-kind="receiving" data-receiving-button><span class="dns-light danger"`) {
		t.Fatalf("connector light must be inside the receiving button")
	}
}

// TestDashboardConnectorLightAbsentWithoutReceivers proves the aggregate light is
// gated on the domain actually naming a receiver target. A Tusk MX setup whose
// receiver set is empty — the manifest advertised none, or the operator started
// setup and abandoned it after the config was saved — must render no dot at all,
// rather than a red "no inbound connector is ready / mail delivery is blocked"
// for a setup that was never finished. The phantom light is the bug: a red dot
// asserts mail is blocked, which is not a fact about a domain with nothing to
// deliver to.
func TestDashboardConnectorLightAbsentWithoutReceivers(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	// A manifest that advertises no receivers is a real (if useless) state: the
	// save path stores the empty snapshot, so the domain has a Dial MX receiving
	// configuration that names nothing.
	svc.AntlerEndpoints = fixedAntler{receivers: nil}
	emptyDomain, err := svc.Store.CreateDomain(ctx, u.AccountID, "empty.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, emptyDomain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save with no receivers: %v", err)
	}
	// The provider is recorded, so the dialmx credential exists; the point is
	// that no receiver target does.
	if _, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, emptyDomain.ID); err != nil {
		t.Fatalf("dialmx provider without a credential: %v", err)
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
	// The empty-receiver domain must contribute no light at all. The only other
	// domain in the fixture is the Mailgun one, which never carries a Dial MX
	// light, so no "No inbound connector is ready" title may appear anywhere.
	if strings.Contains(body, "No inbound connector is ready") {
		t.Fatalf("dashboard rendered a phantom connector light for a domain with no receivers:\n%s", body)
	}
	if n := strings.Count(body, `data-receiving-light`); n != 0 {
		t.Fatalf("dashboard rendered %d receiving light(s) for a domain set with no receiver targets", n)
	}
	// The live lights snapshot must agree: the empty-receiver domain reports no
	// receiving light, so the client tick cannot reintroduce the dot. The domain
	// is still named — a configured domain carries its provider label even with
	// no light — so only the light itself is asserted empty.
	stateReq := httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
	stateReq.AddCookie(cookie)
	stateRR := httptest.NewRecorder()
	h.ServeHTTP(stateRR, stateReq)
	if stateRR.Code != http.StatusOK {
		t.Fatalf("GET /ui/state = %d", stateRR.Code)
	}
	var state map[string]any
	if err := json.Unmarshal(stateRR.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode /ui/state: %v", err)
	}
	if domains, ok := state["domains"].(map[string]any); ok {
		if entry, present := domains[emptyDomain.ID]; present {
			if m, _ := entry.(map[string]any); m["light"] != nil && m["light"] != "" {
				t.Fatalf("live state reported a light for a domain with no receivers: %v", entry)
			}
		}
	}
}

// TestDashboardReceivingLabelNamesStoredProvider proves the live dashboard
// snapshot names a domain's receiving provider as soon as one is stored, with no
// receiver ready. The Tusk MX setup wizard stores the provider on its first
// step, so an operator who starts a setup and cancels it part-way leaves a
// configured domain: without the label the row keeps drawing its pre-open amber
// "Add" beside the live light until a manual reload, even though the domain
// already has a receiver.
func TestDashboardReceivingLabelNamesStoredProvider(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	domain, err := svc.Store.CreateDomain(ctx, u.AccountID, "partial.example.com")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	label := func() string {
		req := httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /ui/state = %d", rr.Code)
		}
		var snap struct {
			Domains map[string]struct {
				Label string `json:"label"`
			} `json:"domains"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatalf("decode /ui/state: %v body=%s", err, rr.Body.String())
		}
		return snap.Domains[domain.ID].Label
	}
	if got := label(); got != "" {
		t.Fatalf("unconfigured domain label = %q, want empty", got)
	}
	// The wizard's first step writes the provider and contact email but names no
	// receiver yet: the state a cancelled setup leaves behind.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	if got := label(); got != "Tusk MX" {
		t.Fatalf("partly configured domain label = %q, want Tusk MX", got)
	}
}
