package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestDialMXAPIKeyRotationAndTXTWizard exercises the Dial MX receiving API: a
// strict HTTPS base URL is required, configuration is separate from key
// rotation, the returned TXT record is public and the private seed is never
// leaked. It then reads the UI wizard for the configured domain.
func TestDialMXAPIKeyRotationAndTXTWizard(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	path := "/v1/admin/domains/" + domain.ID + "/receiving"

	// A receiver URL with a path is not a bare HTTPS origin and must be
	// rejected, leaving no configuration behind.
	bad := do(http.MethodPut, path, `{"provider":"dialmx","config":{"receiver_urls":"https://receiver.example/inbound"}}`)
	if bad.Code != 400 {
		t.Fatalf("path-bearing receiver url should be rejected: %d %s", bad.Code, bad.Body.String())
	}
	// The fixture's mailgun configuration is untouched: the rejected dialmx
	// save must not have replaced it.
	if cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, domain.ID); err != nil || cfg.Provider != "mailgun" {
		t.Fatalf("rejected save must leave the existing provider: %+v %v", cfg, err)
	}

	rr := do(http.MethodPut, path, `{"provider":"dialmx","config":{"receiver_urls":"https://receiver.example","enforcement":"hard"}}`)
	if rr.Code != 200 {
		t.Fatalf("configure: %d %s", rr.Code, rr.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first["key_id"] == nil || first["public_key"] == nil || first["txt_record"] == nil || strings.Contains(rr.Body.String(), "private") {
		t.Fatalf("missing/redacted key details: %s", rr.Body.String())
	}
	// The TXT value carries the MM1 fields; the _mailmoose-mx.<domain> record
	// name is presented by the UI, not by the value.
	if !strings.Contains(first["txt_record"].(string), "v=MM1; k=ed25519") {
		t.Fatalf("txt record missing MM1 fields: %v", first["txt_record"])
	}

	// Rotation is a single action with no configuration payload; sending both
	// together is a client error.
	both := do(http.MethodPut, path, `{"provider":"dialmx","config":{"receiver_urls":"https://receiver.example"},"regenerate_secret":true}`)
	if both.Code != 400 || !strings.Contains(both.Body.String(), "separately") {
		t.Fatalf("rotation with config should be rejected: %d %s", both.Code, both.Body.String())
	}

	before := first["key_id"]
	rr = do(http.MethodPut, path, `{"provider":"dialmx","regenerate_secret":true}`)
	if rr.Code != 200 {
		t.Fatalf("confirmed rotation: %d %s", rr.Code, rr.Body.String())
	}
	var second map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if before == second["key_id"] {
		t.Fatal("key id did not rotate")
	}

	cookie, _ := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	for _, fragment := range []string{"Dial MX", "_mailmoose-mx." + domain.Name, "v=MM1; k=ed25519", "Regenerate key"} {
		if !strings.Contains(page.Body.String(), fragment) {
			t.Fatalf("wizard missing %q", fragment)
		}
	}
	credential, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, domain.ID)
	if err != nil || credential.PublicKey != second["public_key"] {
		t.Fatalf("credential mismatch: %+v %v", credential, err)
	}
	if _, err := svc.Store.GetDialMXCredential(ctx, "foreign", domain.ID); err == nil {
		t.Fatal("foreign account read credential")
	}
	// A non-admin key cannot reach the admin-only domain receiving API.
	_, nonAdmin, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "reader", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+nonAdmin)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin key must be scoped out of domain receiving: %d %s", rr.Code, rr.Body.String())
	}
}

// TestDialMXInheritedExactKeyAndNoOwnConfig proves a subdomain that inherits
// Dial MX from its parent still gets its own exact key on read and rotation,
// without materialising a receiving configuration of its own.
func TestDialMXInheritedExactKeyAndNoOwnConfig(t *testing.T) {
	svc, h, u, parent, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(http.MethodPut, "/v1/admin/domains/"+parent.ID+"/receiving", `{"provider":"dialmx","config":{"receiver_urls":"https://receiver.example","enforcement":"moderate"}}`); rr.Code != 200 {
		t.Fatalf("configure parent: %d %s", rr.Code, rr.Body.String())
	}
	child, err := svc.Store.CreateDomain(ctx, u.AccountID, "child."+parent.Name)
	if err != nil {
		t.Fatal(err)
	}
	if child.ReceivingInheritedFrom == "" || !strings.EqualFold(child.ReceivingProvider, "dialmx") {
		t.Fatalf("child must inherit dialmx: %+v", child)
	}

	childPath := "/v1/admin/domains/" + child.ID + "/receiving"
	childGet := do(http.MethodGet, childPath, "")
	if childGet.Code != 200 || !strings.Contains(childGet.Body.String(), `"public_key"`) {
		t.Fatalf("inherited setup GET: %d %s", childGet.Code, childGet.Body.String())
	}
	var childResp map[string]any
	if err := json.Unmarshal(childGet.Body.Bytes(), &childResp); err != nil {
		t.Fatal(err)
	}
	parentCred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	childCred, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childCred.KeyID == parentCred.KeyID {
		t.Fatalf("child must own an independent exact key: %+v", childCred)
	}
	// The GET must report the child's own key, not the parent's.
	if childCred.PublicKey != childResp["public_key"] || childCred.PublicKey == parentCred.PublicKey {
		t.Fatalf("GET must return the child's own key: child=%+v resp=%v parent=%+v", childCred, childResp["public_key"], parentCred)
	}
	// A read that resolves the inherited configuration must not persist one.
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, child.ID); err == nil {
		t.Fatal("inherited read materialised an own receiving config")
	}

	// Rotation through the UI (the inherited form's standalone regenerate
	// action) rotates the child's own key and still leaves no own config.
	cookie, csrf := uiSession(t, svc, u.ID)
	regPath := "/ui/domains/" + child.ID + "/receiving/regenerate"
	if rr := domainPost(t, h, cookie, regPath, url.Values{}); rr.Code != http.StatusForbidden {
		t.Fatalf("rotation without CSRF must be forbidden: %d", rr.Code)
	}
	if rr := domainPost(t, h, cookie, regPath, url.Values{"_csrf": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("rotation with CSRF: %d %s", rr.Code, rr.Body.String())
	}
	rotated, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.KeyID == childCred.KeyID {
		t.Fatal("child key did not rotate")
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, child.ID); err == nil {
		t.Fatal("inherited rotation materialised an own receiving config")
	}
	// The parent's key is untouched.
	afterParent, err := svc.Store.GetDialMXCredential(ctx, u.AccountID, parent.ID)
	if err != nil || afterParent.KeyID != parentCred.KeyID {
		t.Fatalf("parent key changed: %+v %v", afterParent, err)
	}

	// The dashboard renders the inherited domain's own TXT record even though
	// its provider is shown as inherited.
	page := domainGet(t, h, cookie, "/?domain="+child.ID+"&kind=receiving")
	body := dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+child.ID)
	for _, want := range []string{"_mailmoose-mx." + child.Name, "Regenerate key", "Inherited (from " + parent.Name + ")"} {
		if !strings.Contains(body, want) {
			t.Fatalf("inherited setup dialog missing %q:\n%s", want, body)
		}
	}
}

// TestDialMXUINoNestedForm parses the rendered receiving dialog and asserts it
// contains no <form> nested inside another <form>. The Dial MX setup panel and
// the regenerate action must sit outside the configuration form so the markup is
// valid HTML.
func TestDialMXUINoNestedForm(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{"receiver_urls": "https://receiver.example", "enforcement": "moderate"}, false); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx").Body.String(), "domain-receiving-dialog-"+domain.ID)

	if forms := countNestedForms(t, body); forms != 0 {
		t.Fatalf("receiving dialog must not nest forms, found %d nested", forms)
	}
	// The setup panel must be present and outside the config form.
	if !strings.Contains(body, "dialmx-setup") {
		t.Fatalf("dialmx setup panel missing:\n%s", body)
	}
	if strings.Contains(body, ">_mailmoose."+domain.Name+"<") {
		t.Fatalf("setup panel still uses the wrong _mailmoose hostname:\n%s", body)
	}
}

// TestDialMXRegenerateIsScopedToSelectedProvider pins that the key rotation
// action is only rendered for domains whose effective receiving provider is
// Dial MX; a domain viewed with ?provider=dialmx but configured for something
// else must neither show the action nor mint a key.
func TestDialMXRegenerateIsScopedToSelectedProvider(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	// httpFixture seeds a mailgun receiving config on the fixture domain.
	cookie, csrf := uiSession(t, svc, u.ID)
	page := domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving&provider=dialmx")
	body := dialogHTML(t, page.Body.String(), "domain-receiving-dialog-"+domain.ID)
	if strings.Contains(body, "Regenerate key") {
		t.Fatalf("non-dialmx domain must not offer Dial MX key rotation:\n%s", body)
	}
	// The query provider must not be trusted to mint a credential.
	if _, err := svc.Store.GetDialMXCredential(context.Background(), u.AccountID, domain.ID); err == nil {
		t.Fatal("viewing with ?provider=dialmx minted a credential")
	}
	// The regenerate route for a non-dialmx domain falls through to the generic
	// generated-secret path, which mailgun does not have.
	if rr := domainPost(t, h, cookie, "/ui/domains/"+domain.ID+"/receiving/regenerate", url.Values{"_csrf": {csrf}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("regenerate on non-dialmx/generated provider: %d %s", rr.Code, rr.Body.String())
	}
}

// countNestedForms parses an HTML fragment and counts <form> elements that have
// a <form> ancestor, which makes the markup invalid.
func countNestedForms(t *testing.T, fragment string) int {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(fragment))
	if err != nil {
		t.Fatalf("parse html: %v", err)
	}
	nested := 0
	var walk func(n *html.Node, inForm bool)
	walk = func(n *html.Node, inForm bool) {
		if n.Type == html.ElementNode && n.Data == "form" {
			if inForm {
				nested++
			}
			inForm = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inForm)
		}
	}
	walk(doc, false)
	return nested
}
