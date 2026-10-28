package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/dkimsign"
	"github.com/dellarb/mammothmail/internal/transport/mx"
)

// TestDashboardSendingLightRendersForSigningDomain proves the Domains table's
// Sending column draws the published-DKIM-record light for a Direct MX domain
// that signs: grey while the record is unconfirmed, green once it is published
// and matching, and never for a domain that does not sign.
func TestDashboardSendingLightRendersForSigningDomain(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, domain.ID, mx.Provider, map[string]any{
		"helo": "mail.example.test", mx.FieldDKIMEnabled: "on",
	}); err != nil {
		t.Fatalf("direct mx sending save: %v", err)
	}

	// Publish nothing yet: the check is pending, which renders grey.
	res := &switchableResolver{}
	res.set(nil, nil)
	watchDNS(svc, res)

	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-kind="sending" data-sending-button><span class="dns-light grey" data-sending-light`) {
		t.Fatalf("dashboard missing the grey DKIM sending light:\n%s", body)
	}
	// The light must render once, for the signing domain only.
	if got := strings.Count(body, "data-sending-light"); got != 1 {
		t.Fatalf("sending light rendered %d times, want 1", got)
	}
	if !strings.Contains(body, `data-dkim-record-text>DNS not present</b>`) {
		t.Fatalf("DKIM panel missing the DNS-not-present status text:\n%s", body)
	}

	// Publish the exact matching record: the light turns green, and the lights
	// snapshot carries the same value so the browser can patch it live.
	res.set(nil, []string{dkimPublishedValue(t, svc, u.AccountID, domain.ID)})
	svc.DNS.CheckOnce(ctx)

	req = httptest.NewRequest(http.MethodGet, "/ui/state?page=dashboard&lights=1", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("lights snapshot = %d", rr.Code)
	}
	var out struct {
		Sending map[string]struct {
			Light string `json:"light"`
			Title string `json:"title"`
		} `json:"sending"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got := out.Sending[domain.ID].Light; got != "ok" {
		t.Fatalf("sending light with the published record = %q, want ok", got)
	}

	// The sending dialog's DNS-record panel states the result in words beside the
	// dot, so the operator reads the verdict rather than decoding a colour.
	req = httptest.NewRequest(http.MethodGet, "/?domain="+domain.ID+"&kind=sending&provider=mx", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sending dialog = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `data-dkim-record-text>DNS valid</b>`) {
		t.Fatalf("DKIM panel missing the DNS-valid status text:\n%s", rr.Body.String())
	}
}

// TestDashboardSendingLightAbsentWhenSigningOff proves a non-signing Direct MX
// domain renders no sending light at all, matching "no traffic lights when DKIM
// is off".
func TestDashboardSendingLightAbsentWhenSigningOff(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, domain.ID, mx.Provider, map[string]any{
		"helo": "mail.example.test", mx.FieldDKIMEnabled: "off",
	}); err != nil {
		t.Fatalf("direct mx sending save: %v", err)
	}
	res := &switchableResolver{}
	watchDNS(svc, res)

	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "data-sending-light") {
		t.Fatalf("a non-signing domain must render no sending light:\n%s", rr.Body.String())
	}
}

// dkimPublishedValue returns the TXT value an operator would publish for the
// domain's stored signing key, derived the same way the watcher does.
func dkimPublishedValue(t *testing.T, svc *app.Service, accountID, domainID string) string {
	t.Helper()
	key, err := svc.ServiceDomainDKIM(context.Background(), accountID, domainID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := dkimsign.ParsePrivateKey(key.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, err := dkimsign.PublicKeyPEM(parsed)
	if err != nil {
		t.Fatal(err)
	}
	pubB64, err := dkimsign.PublicKeyBase64(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	return dkimsign.TXTRecordValue(pubB64)
}
