package httpapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// fixedAntler is a deterministic Antler endpoint resolver for HTTP tests.
type fixedAntler struct{ receivers []mxdial.AntlerReceiver }

func (f fixedAntler) Receivers(context.Context) ([]mxdial.AntlerReceiver, error) {
	return f.receivers, nil
}

// fakeResolver serves deterministic MX/TXT answers for the traffic-light tests.
type fakeResolver struct {
	mx   []*net.MX
	mxE  error
	txt  []string
	txtE error
}

func (f fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return f.mx, f.mxE }
func (f fakeResolver) LookupTXT(context.Context, string) ([]string, error) {
	return f.txt, f.txtE
}

// waitForDNS polls until the asynchronous DNS checks settle past the
// "checking" placeholder.
func waitForDNS(t *testing.T, do func() *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		rr := do()
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		dns, _ := body["dns"].([]any)
		checking := false
		for _, raw := range dns {
			v, _ := raw.(map[string]any)
			reason, _ := v["reason"].(string)
			if v["state"] == "pending" && strings.Contains(reason, "checking") {
				checking = true
			}
		}
		if len(dns) > 0 && !checking {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("DNS checks never settled")
	return nil
}

// TestDialMXAntlerReceivingAPITrafficLights proves the receiving API returns the
// Antler instructions, live receiver status and published-record checks, with a
// green light for matching MX and TXT records and a red one for a mismatch.
func TestDialMXAntlerReceivingAPITrafficLights(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://antler2.example.test", SMTPHostname: "antler2.example.test", MXPriority: 20},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	txtValue := mxwire.DomainTXT(cred.KeyID, pub)

	srv := httpapp.New(svc, nil)
	srv.SetDNSResolver(fakeResolver{
		mx:  []*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "antler2.example.test.", Pref: 20}},
		txt: []string{txtValue},
	})
	h := srv.Handler()

	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	body := waitForDNS(t, do)
	statuses, _ := body["status"].([]any)
	if len(statuses) != 2 {
		t.Fatalf("must show both receivers before their first connection: %v", body["status"])
	}
	for _, raw := range statuses {
		status := raw.(map[string]any)
		if status["state"] != "connecting" || status["smtp_hostname"] == nil {
			t.Fatalf("unexpected initial receiver status: %v", status)
		}
	}

	instr, _ := body["instructions"].(map[string]any)
	if instr == nil || instr["service"] != "antler" || instr["contact_email"] != "ops@example.test" {
		t.Fatalf("instructions missing Antler metadata: %v", body["instructions"])
	}
	mx, _ := instr["mx"].([]any)
	if len(mx) != 2 {
		t.Fatalf("instructions missing MX records: %v", instr["mx"])
	}
	if mx[0].(map[string]any)["hostname"] != "antler1.example.test" {
		t.Fatalf("unexpected MX instruction: %v", mx[0])
	}
	if instr["txt_name"] != "_mailmoose-mx."+domain.Name {
		t.Fatalf("unexpected TXT name: %v", instr["txt_name"])
	}

	dns, _ := body["dns"].([]any)
	lights := map[string]string{}
	for _, raw := range dns {
		v := raw.(map[string]any)
		lights[v["kind"].(string)] = v["state"].(string)
	}
	if lights["mx"] != "ok" || lights["txt"] != "ok" {
		t.Fatalf("expected green MX and TXT lights, got %v", lights)
	}

	// A wrong key turns the TXT light red; a foreign MX hostname turns MX red.
	if _, err := svc.RotateDialMXCredential(ctx, u.AccountID, domain.ID); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	srv.SetDNSResolver(fakeResolver{
		mx:  []*net.MX{{Host: "elsewhere.example.test.", Pref: 10}},
		txt: []string{"v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub)},
	})
	body = waitForDNS(t, do)
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		if v["state"] != "mismatch" {
			t.Fatalf("%s light = %v, want mismatch", v["kind"], v["state"])
		}
	}

	// The dashboard setup panel renders the MX instructions and the lights.
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	for _, want := range []string{"Antler MX", "10 antler1.example.test", "ops@example.test", "dns-light"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("setup panel missing %q", want)
		}
	}
}

// TestDialMXAntlerPartialMXSetIsHealthy proves the MX check treats the receiver
// set as redundancy: publishing only one of two advertised receivers is "ok",
// matched lists the receiver that is actually pointed at, and the other is left
// out of matched (so its connector row is not painted by the aggregate check).
func TestDialMXAntlerPartialMXSetIsHealthy(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
		{ID: "antler-2", SessionURL: "https://antler2.example.test", SMTPHostname: "antler2.example.test", MXPriority: 20},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	cred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	txtValue := mxwire.DomainTXT(cred.KeyID, pub)

	srv := httpapp.New(svc, nil)
	// Only the priority-10 receiver is published.
	srv.SetDNSResolver(fakeResolver{
		mx:  []*net.MX{{Host: "antler1.example.test.", Pref: 10}},
		txt: []string{txtValue},
	})
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	body := waitForDNS(t, do)
	var mx *map[string]any
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		if v["kind"] == "mx" {
			m := v
			mx = &m
		}
	}
	if mx == nil {
		t.Fatalf("no MX check returned: %v", body["dns"])
	}
	if (*mx)["state"] != "ok" {
		t.Fatalf("partial MX set = %v, want ok", (*mx)["state"])
	}
	matched, _ := (*mx)["matched"].([]any)
	if len(matched) != 1 || matched[0] != "antler1.example.test" {
		t.Fatalf("matched = %v, want only the published receiver", matched)
	}
}

// The browser uses session-scoped setup routes; saves require the existing
// CSRF guard, and reads require a browser session.
func TestAntlerWizardSessionSetup(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/domains/" + domain.ID + "/receiving/setup"
	do := func(method, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"provider":"dialmx","config":{"service":"antler","contact_email":"ops@example.test","enforcement":"moderate"}}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(http.MethodPut, ""); rr.Code != http.StatusForbidden {
		t.Fatalf("save without CSRF: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodPut, csrf); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"txt_value"`) {
		t.Fatalf("session setup save: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(http.MethodGet, ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"smtp_hostname":"antler1.example.test"`) {
		t.Fatalf("session status check: %d %s", rr.Code, rr.Body.String())
	}
	before, err := svc.Store.GetDomainReceivingConfig(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeValues, err := svc.DecryptDomainReceivingConfig(before)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := svc.Store.GetDialMXCredential(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A changed manifest must not change endpoints during an email-only save.
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "new", SessionURL: "https://new.example.test", SMTPHostname: "new.example.test", MXPriority: 10},
	}}
	update := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"provider":"dialmx","config":{"contact_email":"changed@example.test"}}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := update(""); rr.Code != http.StatusForbidden {
		t.Fatalf("email save without CSRF: %d", rr.Code)
	}
	if rr := update(csrf); rr.Code != http.StatusOK {
		t.Fatalf("email-only save: %d %s", rr.Code, rr.Body.String())
	}
	after, err := svc.Store.GetDomainReceivingConfig(context.Background(), u.AccountID, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterValues, err := svc.DecryptDomainReceivingConfig(after)
	if err != nil {
		t.Fatal(err)
	}
	if afterValues["contact_email"] != "changed@example.test" {
		t.Fatalf("email not saved: %v", afterValues)
	}
	for _, field := range []string{"receiver_urls", "antler_receivers", "setup_id", "enforcement"} {
		if afterValues[field] != beforeValues[field] {
			t.Fatalf("email save changed %s", field)
		}
	}
	afterCredential, err := svc.Store.GetDialMXCredential(context.Background(), u.AccountID, domain.ID)
	if err != nil || afterCredential.KeyID != credential.KeyID {
		t.Fatalf("email save changed domain key: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup must require a session: %d", rr.Code)
	}
}

// TestDialMXAntlerDialogHidesReceiverURLs pins that the editable Receiver URLs
// field is not present in the Antler MX dialog (it lives only in an inert
// template), but is present and editable for the custom service.
func TestDialMXAntlerDialogHidesReceiverURLs(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
	}, false); err != nil {
		t.Fatalf("antler save: %v", err)
	}
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx").Body.String(), "domain-receiving-dialog-"+domain.ID)
	settings := providerGroupHTML(t, body, "dialmx")

	if strings.Contains(stripDialMXReceiverTemplate(settings), `name="cfg_dialmx_receiver_urls"`) {
		t.Fatalf("Antler dialog renders an editable receiver_urls input:\n%s", settings)
	}
	if !strings.Contains(settings, `<template class="dialmx-receiver-urls-field">`) {
		t.Fatalf("Antler dialog is missing the inert receiver_urls template:\n%s", settings)
	}

	// A custom setup keeps the editable input.
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceCustom, "receiver_urls": "https://receiver.example",
	}, false); err != nil {
		t.Fatalf("custom save: %v", err)
	}
	body = dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx").Body.String(), "domain-receiving-dialog-"+domain.ID)
	settings = providerGroupHTML(t, body, "dialmx")
	if !strings.Contains(settings, `name="cfg_dialmx_receiver_urls"`) {
		t.Fatalf("custom dialog must render the editable receiver_urls input:\n%s", settings)
	}
	if strings.Contains(settings, `<template class="dialmx-receiver-urls-field">`) {
		t.Fatalf("custom dialog must not wrap the field in a template:\n%s", settings)
	}
}

// TestDialMXAntlerAPIRejectsReceiverURLs proves the receiving API refuses a
// client-supplied receiver_urls for the Antler service, so a crafted request
// cannot point the domain at an arbitrary receiver.
func TestDialMXAntlerAPIRejectsReceiverURLs(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	svc.AntlerEndpoints = fixedAntler{receivers: []mxdial.AntlerReceiver{
		{ID: "antler-1", SessionURL: "https://antler1.example.test", SMTPHostname: "antler1.example.test", MXPriority: 10},
	}}
	srv := httpapp.New(svc, nil)
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"provider":"dialmx","config":{"service":"antler","contact_email":"ops@example.test","receiver_urls":"https://evil.example.test"}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "Antler MX") {
		t.Fatalf("injected receiver_urls for antler: %d %s", rr.Code, rr.Body.String())
	}
	// The rejected save must not have replaced the fixture's existing provider
	// nor stored the injected URL.
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, domain.ID)
	if err != nil || cfg.Provider != "mailgun" {
		t.Fatalf("rejected save changed the provider: %+v %v", cfg, err)
	}
}

// TestDialMXCustomReceivingAPIHasNoAntlerInstructions pins that a custom setup
// keeps its manual receiver URLs and does not advertise Antler MX records.
func TestDialMXCustomReceivingAPIHasNoAntlerInstructions(t *testing.T) {
	svc, _, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example"}, false); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetDNSResolver(fakeResolver{mx: []*net.MX{{Host: "receiver.example.", Pref: 10}}})
	h := srv.Handler()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	body := waitForDNS(t, do)
	instr, _ := body["instructions"].(map[string]any)
	if instr == nil || instr["service"] != "custom" {
		t.Fatalf("instructions = %v, want custom", body["instructions"])
	}
	if _, ok := instr["mx"]; ok {
		t.Fatalf("custom setup advertised MX instructions: %v", instr["mx"])
	}
	for _, raw := range body["dns"].([]any) {
		v := raw.(map[string]any)
		if v["kind"] == "mx" {
			t.Fatalf("custom setup should not check MX records: %v", v)
		}
	}

	// The dialog must prefill the custom service so a save cannot silently
	// migrate a legacy configuration to Antler MX.
	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	settings := providerGroupHTML(t, dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID), "dialmx")
	if !strings.Contains(settings, `<option value="custom" selected>`) {
		t.Fatalf("legacy custom config not prefilled as custom:\n%s", settings)
	}
}

// stripDialMXReceiverTemplate removes the inert <template> that carries the
// Receiver URLs field for an Antler MX setup, so an assertion can check that no
// editable input is rendered without tripping on the template's own input.
func stripDialMXReceiverTemplate(settings string) string {
	const open = `<template class="dialmx-receiver-urls-field">`
	start := strings.Index(settings, open)
	if start < 0 {
		return settings
	}
	rest := settings[start:]
	if end := strings.Index(rest, "</template>"); end >= 0 {
		return settings[:start] + rest[end+len("</template>"):]
	}
	return settings[:start]
}
