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
