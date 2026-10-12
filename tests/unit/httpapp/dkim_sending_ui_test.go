package httpapp_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/dkimsign"
)

// TestUIDirectMXSendingDKIMFieldsRender proves the DKIM controls appear on the
// Direct MX sending tab and that the generated public key is displayed for the
// operator to publish.
func TestUIDirectMXSendingDKIMFieldsRender(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	// Save through the form exactly as the browser would.
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf":                {csrf},
		"provider":             {"mx"},
		"cfg_mx_helo":          {"mail.example.com"},
		"cfg_mx_dkim_enabled":  {"on"},
		"cfg_mx_dkim_selector": {"mailmoose"},
	})
	if rr.Code != 303 {
		t.Fatalf("save direct mx sending %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dec["dkim_private_key"].(string); !ok {
		t.Fatal("no key was generated when DKIM was enabled through the form")
	}

	// Re-render the domain page: the public key must be shown for DNS, and the
	// private key must never be rendered into the page.
	storedPub, _ := dec["dkim_public_key"].(string)
	page := domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending").Body.String()
	if !strings.Contains(page, "Public key (publish in DNS)") {
		t.Error("the DKIM public key field is not present on the domain page")
	}
	if !strings.Contains(page, strings.SplitN(storedPub, "\n", 2)[0]) {
		t.Error("the stored public key is not displayed for the operator to publish")
	}
	if strings.Contains(page, "PRIVATE KEY") {
		t.Error("the domain page renders private key material")
	}
	if !strings.Contains(page, dkimsign.DefaultSelector) {
		t.Error("the default selector is not shown on the domain page")
	}
}

// TestUIDirectMXSendingAcceptsPastedKey proves an operator's own key survives
// the form round-trip, exercising the overridable-generated path in
// configFromForm (which must submit an overridable generated field).
func TestUIDirectMXSendingAcceptsPastedKey(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf":                   {csrf},
		"provider":                {"mx"},
		"cfg_mx_helo":             {"mail.example.com"},
		"cfg_mx_dkim_enabled":     {"on"},
		"cfg_mx_dkim_selector":    {"ownkey"},
		"cfg_mx_dkim_private_key": {privPEM},
	})
	if rr.Code != 303 {
		t.Fatalf("save pasted key %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := dec["dkim_private_key"].(string); got != dkimsign.CanonicalPEM(privPEM) {
		t.Error("the pasted key was not stored through the form")
	}
	// The stored public key must match the pasted private key.
	if got, _ := dec["dkim_public_key"].(string); strings.TrimSpace(got) != strings.TrimSpace(pubPEM) {
		t.Error("stored public key does not match the pasted private key")
	}
}

// TestUIDirectMXSendingRejectsUnusableKey proves an unusable-key save is
// rejected and surfaced as a field error, rather than persisting a config that
// could never sign.
func TestUIDirectMXSendingRejectsUnusableKey(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf":                   {csrf},
		"provider":                {"mx"},
		"cfg_mx_helo":             {"mail.example.com"},
		"cfg_mx_dkim_enabled":     {"on"},
		"cfg_mx_dkim_private_key": {"not-a-key"},
	})
	// A rejected save redirects back to the dialog carrying the error, so the
	// config must not have been written.
	if _, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID); err == nil {
		t.Fatal("an unusable pasted key was persisted")
	}
	// Following the redirect must show the error against the key field.
	loc := rr.Header().Get("Location")
	if loc == "" {
		t.Fatalf("no redirect location on a rejected save: %d %s", rr.Code, rr.Body.String())
	}
	page := domainGet(t, h, cookie, loc).Body.String()
	if !strings.Contains(page, "rivate key") {
		t.Errorf("the error does not point at the key field: %s", page)
	}
}

// TestUIDirectMXSendingReadOnlyPublicKeyIsNotSubmitterControlled proves a
// submitted public key is ignored: it is derived server-side, so a caller
// cannot make the UI display a key that does not match what is signed with.
func TestUIDirectMXSendingReadOnlyPublicKeyIsNotSubmitterControlled(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	_, attackerPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	// Submit the real private key but a mismatched public key.
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf":                   {csrf},
		"provider":                {"mx"},
		"cfg_mx_helo":             {"mail.example.com"},
		"cfg_mx_dkim_enabled":     {"on"},
		"cfg_mx_dkim_private_key": {privPEM},
		"cfg_mx_dkim_public_key":  {attackerPEM},
	})
	if rr.Code != 303 {
		t.Fatalf("save %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := dec["dkim_public_key"].(string)
	if strings.TrimSpace(got) == strings.TrimSpace(attackerPEM) {
		t.Fatal("a submitted public key was stored instead of the derived one")
	}
	if strings.TrimSpace(got) != strings.TrimSpace(pubPEM) {
		t.Error("stored public key is not the derived public half of the private key")
	}
}
